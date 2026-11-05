package key

import (
	"database/sql"
	"errors"
	"net/http"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
)

// Reveal answers POST /api/console/keys/{id}/reveal: the owner asks for the
// plaintext of a key they already created, to paste into a client they lost the
// original for.
//
// Four decisions worth keeping:
//
//  1. POST, not GET. The console's CSRF guard covers write methods; a GET that
//     returns a credential can be triggered by a prefetcher or pasted link.
//  2. The lookup is scoped by user_id, so someone else's id answers 404 exactly
//     like a deleted one — same shape as loadOwnedKey, no existence oracle.
//  3. "Cannot reveal" is 200 with available=false, not an error code. Two cases
//     land here: a key created before key_enc existed (the hash is one-way, so
//     the plaintext is gone), and a deployment with no KEK. Neither is a client
//     mistake, and both need a sentence of explanation that a bare code cannot
//     carry. Adding a code would also widen the frozen error table for a state
//     the UI can act on either way.
//  4. Every successful reveal writes an audit row. Handing out a credential is
//     exactly the kind of event an owner should be able to find later.
func (s *Service) Reveal(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	id, apiErr := pathKeyID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	ctx := r.Context()
	var (
		envelope []byte
		prefix   string
		last4    string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT key_enc, key_prefix, key_last4
		FROM t_api_key
		WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, id, sess.UID).
		Scan(&envelope, &prefix, &last4)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, apierr.New(apierr.CodeKeyNotFound))
		return
	}
	if err != nil {
		s.logError(ctx, "key_reveal_lookup_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	if len(envelope) == 0 {
		// Two different causes, one honest answer: we do not have the plaintext.
		// Reporting them separately would need an extra code for a difference the
		// user cannot act on differently (both end in "rotate to get a new one").
		writeRevealUnavailable(w, "not_stored")
		return
	}
	if s.Box == nil {
		writeRevealUnavailable(w, "kek_unavailable")
		return
	}
	plain, openErr := s.Box.Open(string(envelope))
	if openErr != nil {
		// Wrong KEK or a corrupted envelope: the key still authenticates via
		// key_hash, so this is not worth failing the whole console over.
		s.logError(ctx, "key_reveal_open_failed: "+openErr.Error())
		writeRevealUnavailable(w, "decrypt_failed")
		return
	}

	now := s.now()
	if auditErr := insertKeyAudit(ctx, s.DB, id, "VIEW_SECRET", nil,
		map[string]any{"key_prefix": prefix, "key_last4": last4},
		sess.UID, audit.IPBytes(r), now); auditErr != nil {
		// Deliberately not fatal: refusing to show the key because the log write
		// failed would turn an audit-table hiccup into a user-facing outage. The
		// failure is recorded so it can be reconciled.
		s.logError(ctx, "key_reveal_audit_failed: "+auditErr.Error())
	}

	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"key":       string(plain),
	})
}

func writeRevealUnavailable(w http.ResponseWriter, reason string) {
	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{
		"available": false,
		"reason":    reason,
	})
}
