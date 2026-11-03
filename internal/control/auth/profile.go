package auth

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

// Nickname ceiling matches t_user.nickname varchar(64) and the check register
// already applies. Counting runes, not bytes: 20 个汉字 is a normal nickname and
// 60 bytes, while varchar(64) counts characters.
const nicknameMaxRunes = 64

type profileUpdateRequest struct {
	Nickname string `json:"nickname"`
}

// UpdateProfile implements PATCH /api/console/profile — the customer editing
// their own display name.
//
// Only the nickname is writable. Email changes need re-verification and are a
// different flow; role/status are operator-scoped and must never be settable
// from the account console, which is why this handler takes a dedicated request
// shape instead of decoding a user row.
func (s *Service) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	var req profileUpdateRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if utf8.RuneCountInString(nickname) > nicknameMaxRunes {
		mw.WriteError(w, r, mw.InvalidField("nickname").
			WithHint("昵称最多 64 个字符"))
		return
	}
	if hasControlChars(nickname) {
		// Control characters (including newlines) would break every place the
		// name is rendered and could forge log lines.
		mw.WriteError(w, r, mw.InvalidField("nickname").
			WithHint("昵称不能包含换行或控制字符"))
		return
	}

	ctx := r.Context()
	res, err := s.DB.ExecContext(ctx,
		`UPDATE t_user SET nickname = ? WHERE id = ? AND deleted_at IS NULL`,
		nickname, sess.UID)
	if err != nil {
		s.errorLog(ctx, "profile_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if n, rowsErr := res.RowsAffected(); rowsErr == nil && n == 0 {
		// 0 rows means the account vanished between the session check and the
		// update (cancelled concurrently); the session is no longer valid.
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}

	var email, phone, storedNickname, role, status string
	err = s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(email, ''), COALESCE(phone, ''), COALESCE(nickname, ''), role, status
		  FROM t_user
		 WHERE id = ? AND deleted_at IS NULL`, sess.UID).
		Scan(&email, &phone, &storedNickname, &role, &status)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	if err != nil {
		s.errorLog(ctx, "profile_readback_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	_ = mw.WriteJSON(w, http.StatusOK, map[string]any{
		"user": view(sess.UID, email, phone, storedNickname, role, status),
	})
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\t') {
			return true
		}
	}
	return false
}
