package fund

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/phoneclaim"
)

// Trial anti-abuse lookups are bounded to this window. A device that claimed
// two years ago should not block a legitimate household forever, and an
// unbounded scan on t_admin_audit_log would only get slower.
const trialClaimLookbackDays = 30

// trialPhoneWindowDays is the phone-number window: a number may collect a trial
// once per gift-credit lifetime (90 days), counted from the last grant to that
// number, across accounts and unaffected by cancellation.
//
// It is an alias, not a second literal: phoneclaim.WindowDays is the boundary
// the ledger's Claim.WindowHit compares against, and a copy here would let the
// pre-flight check and the in-transaction check disagree by a day.
const trialPhoneWindowDays = phoneclaim.WindowDays

type trialClaimRequest struct {
	Fingerprint string `json:"fingerprint"`
}

type trialClaimResponse struct {
	PlanCode        string `json:"plan_code"`
	GrantedMicro    int64  `json:"granted_micro"`
	GrantedCredits  int64  `json:"granted_credits"`
	TrialExpireAt   string `json:"trial_expire_at"`
	BonusExpireAt   string `json:"bonus_expire_at"`
	BalanceAfter    int64  `json:"balance_after_micro"`
	AntiAbuseChecks string `json:"anti_abuse_checks"`
}

// ClaimTrial implements POST /api/console/trial/claim (G-09 / A7).
//
// Four independent fingerprints are compared, any one of which blocks the
// claim: device hash, phone, IP segment, and the account's own claim flag.
// E-mail is deliberately not a factor — it is free to create, so relying on
// it would turn the trial into an unlimited grant.
//
// The phone factor compares t_user.phone_hmac (the stored HMAC), never the
// plaintext column: the hash survives cancellation and any future PII clearing,
// which is why migration 0011 created it. Only fingerprintHash needs a key
// (FP_HMAC_KEY); the phone hash is read, not recomputed.
func (s *Service) ClaimTrial(w http.ResponseWriter, r *http.Request) {
	uid, apiErr := sessionUID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if strings.TrimSpace(s.FingerprintKey) == "" {
		s.logError(r.Context(), "fund_trial_fp_key_missing env=FP_HMAC_KEY")
		mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable))
		return
	}
	var req trialClaimRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	fp := strings.TrimSpace(req.Fingerprint)
	if fp == "" || len(fp) > 512 {
		mw.WriteError(w, r, mw.InvalidField("fingerprint"))
		return
	}
	ctx := r.Context()
	fpHash := fingerprintHash(s.FingerprintKey, fp)
	// One clock reading for the whole request: the window boundary, the grant
	// timestamps and the expiry dates must all be computed from the same
	// instant, or a claim that straddles a tick could write a window it has
	// already left.
	now := s.now()

	// Device: any account that already claimed with this fingerprint blocks.
	var deviceClaimed int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_device_fingerprint WHERE fp_hash = ? AND trial_granted = 1`,
		fpHash).Scan(&deviceClaimed); err != nil {
		s.logError(ctx, "fund_trial_device_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if deviceClaimed > 0 {
		mw.WriteError(w, r, trialRefusal())
		return
	}

	// Phone: the number's ledger decides, not the account.
	//
	// The ledger (t_phone_claim) is addressed by the number's HMAC and survives
	// cancellation, which is what makes the window cross-account: A claims the
	// trial, cancels (releasing the number), B registers the same number and is
	// still refused for 90 days.
	//
	// Three states, and only the middle one is normal:
	//   phone == ""                no handset on the account; skip the check
	//   phone != "" && hmac != ""  read the ledger
	//   phone != "" && hmac == ""  the number is known, its hash is not —
	//                              a manual DB edit or a backfill that has not
	//                              run. Refuse (500) rather than grant: paying
	//                              out with an unknown de-duplication key is
	//                              exactly what an abuser wants.
	//
	// A missing ledger row is refused the same way (fail closed). Every write
	// path creates the row, so "missing" can only mean the backfill has not
	// run; granting on it would silently hand out a second trial, while
	// refusing costs one 500 and one clear log token.
	var phone, phoneHMAC string
	var claimedAt sql.NullTime
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(phone, ''), COALESCE(phone_hmac, ''), trial_claimed_at
		  FROM t_user WHERE id = ? AND deleted_at IS NULL`, uid).Scan(&phone, &phoneHMAC, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	if err != nil {
		s.logError(ctx, "fund_trial_user_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if phone != "" && phoneHMAC == "" {
		// uid only: the phone number and its hash are both PII-adjacent and
		// this line ends up in tickets.
		s.logError(ctx, fmt.Sprintf("fund_trial_phone_hmac_missing uid=%d; refusing trial claim", uid))
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if phoneHMAC != "" {
		claim, claimErr := phoneclaim.Get(ctx, s.DB, phoneHMAC)
		switch {
		case errors.Is(claimErr, phoneclaim.ErrNotFound):
			// Runback step: `make backfill-phone-claim ARGS="backfill --commit"`.
			s.logError(ctx, fmt.Sprintf("trial_phone_claim_missing uid=%d; refusing trial claim", uid))
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		case claimErr != nil:
			s.logError(ctx, "fund_trial_claim_read_failed: "+claimErr.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		if claim.WindowHit(now) {
			s.logWarn(ctx, fmt.Sprintf("trial_phone_window_hit window_days=%d", trialPhoneWindowDays))
			mw.WriteError(w, r, trialRefusal())
			return
		}
	}

	// IP segment: the same /24 (IPv4) or /64 (IPv6) already claimed.
	ipBlocked, err := s.ipSegmentClaimed(ctx, audit.IPBytes(r))
	if err != nil {
		s.logError(ctx, "fund_trial_ip_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if ipBlocked {
		mw.WriteError(w, r, trialRefusal())
		return
	}
	if claimedAt.Valid {
		mw.WriteError(w, r, trialRefusal())
		return
	}

	var planID, bonusMicro, bonusDays, trialDays int64
	err = s.DB.QueryRowContext(ctx, `
		SELECT id, grant_bonus_micro, bonus_valid_days
		  FROM t_plan WHERE code = 'TRIAL' AND enabled = 1 LIMIT 1`).
		Scan(&planID, &bonusMicro, &bonusDays)
	if err != nil {
		s.logError(ctx, "fund_trial_plan_read_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	trialDays = s.configInt(ctx, "plan.trial_days", 0)
	if trialDays <= 0 {
		trialDays = 7
	}
	expireDays := s.bonusExpireDays(ctx, bonusDays)
	bonusExpire := now.AddDate(0, 0, int(expireDays))

	var balanceAfter int64
	err = func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()

		// Re-read under the row lock: two concurrent claims must not both
		// pass the checks above. The phone hash is read here too, so the
		// ledger check below sees the value that is protected by this lock
		// rather than the one read before it.
		var lockedClaimed sql.NullTime
		var lockedHMAC string
		if err := tx.QueryRowContext(ctx, `
			SELECT trial_claimed_at, COALESCE(phone_hmac, '') FROM t_user
			 WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, uid).
			Scan(&lockedClaimed, &lockedHMAC); err != nil {
			return err
		}
		if lockedClaimed.Valid {
			return errTrialAlreadyClaimed
		}
		// Lock the fingerprint's index range so two accounts racing with the
		// same device cannot both observe "not granted" and both insert.
		var anyFP int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM t_device_fingerprint WHERE fp_hash = ? LIMIT 1 FOR UPDATE`,
			fpHash).Scan(&anyFP); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var stillClaimed int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM t_device_fingerprint
			 WHERE fp_hash = ? AND trial_granted = 1`, fpHash).Scan(&stillClaimed); err != nil {
			return err
		}
		if stillClaimed > 0 {
			return errTrialDeviceClaimed
		}
		// Last ring of the lock order: t_user → t_device_fingerprint →
		// t_phone_claim. Re-checking the window here is what makes the
		// pre-flight check advisory: two claims on the same number both pass
		// the unlocked read, and only this row lock serialises them.
		if lockedHMAC != "" {
			_, hit, claimErr := phoneclaim.TrialWindowTx(ctx, tx, lockedHMAC, now)
			switch {
			case errors.Is(claimErr, phoneclaim.ErrNotFound):
				return errTrialPhoneClaimMissing
			case claimErr != nil:
				return claimErr
			case hit:
				return errTrialPhoneWindow
			}
		}

		if _, err := s.applyPlanTx(ctx, tx, uid, planID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE t_user SET trial_claimed_at = ?, updated_at = ? WHERE id = ?`,
			now, now, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO t_device_fingerprint
				(fp_hash, user_id, first_seen_at, last_seen_at, seen_count,
				 trial_granted, granted_user_id, created_at)
			VALUES (?,?,?,?,1,1,?,?)
			ON DUPLICATE KEY UPDATE
				last_seen_at = VALUES(last_seen_at),
				seen_count = seen_count + 1,
				trial_granted = 1,
				granted_user_id = COALESCE(granted_user_id, VALUES(granted_user_id))`,
			fpHash, uid, now, now, uid, now); err != nil {
			return err
		}
		// The window and the grant commit together. Writing it after the
		// device row (still inside the transaction) keeps the order the
		// ledger's lock discipline expects and lets the "row vanished"
		// failure roll everything back.
		if lockedHMAC != "" {
			if err := phoneclaim.MarkTrialGrantedTx(ctx, tx, lockedHMAC, uid, now); err != nil {
				return err
			}
		}
		reason := "体验版赠送积分"
		res, err := s.Ledger.GrantTx(ctx, tx, ledger.GrantParams{
			UserID:         uid,
			BonusMicro:     bonusMicro,
			BonusExpireAt:  &bonusExpire,
			BizType:        ledger.BizTypePlanGrant,
			Reason:         &reason,
			IdemKeyBonus:   fmt.Sprintf("trial:%d", uid),
			BonusBatchType: ledger.BatchTypeBonus,
		}, now)
		if err != nil {
			return err
		}
		trialAfter := map[string]any{"fp_hash": fpHash, "granted_micro": bonusMicro}
		if lockedHMAC != "" {
			// Which source the phone factor was judged on. Deliberately not the
			// ledger's timestamp or the number: the audit row is read by
			// operators, and "claim" is the only fact they need here.
			trialAfter["phone_window"] = "claim"
		}
		if err := s.Audit.Insert(ctx, tx, audit.Entry{
			OperatorID:   uid,
			OperatorRole: "PERSONAL",
			OperatorIP:   audit.IPBytes(r),
			Module:       "RISK",
			Action:       "TRIAL_CLAIM",
			TargetType:   "t_user",
			TargetID:     itoa(uid),
			After:        mustMarshal(trialAfter),
			Result:       "OK",
		}); err != nil {
			s.logWarn(ctx, "fund_trial_audit_failed: "+err.Error())
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		committed = true
		balanceAfter = res.BalanceAfterMicro
		return nil
	}()
	switch {
	case errors.Is(err, errTrialAlreadyClaimed), errors.Is(err, errTrialDeviceClaimed),
		errors.Is(err, errTrialPhoneWindow):
		mw.WriteError(w, r, trialRefusal())
		return
	case errors.Is(err, errTrialPhoneClaimMissing):
		// Inside the transaction: the ledger row disappeared between the
		// pre-flight read and the locked re-read. Same token as the pre-flight
		// path so one grep finds both.
		s.logError(ctx, fmt.Sprintf("trial_phone_claim_missing uid=%d; refusing trial claim", uid))
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	case err != nil:
		s.logError(ctx, "fund_trial_claim_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	if s.PublishRoutes != nil {
		if err := s.PublishRoutes(ctx); err != nil {
			// The grant is committed; a stale routing snapshot only delays the
			// trial tier's limits. Log and answer success.
			s.logWarn(ctx, "fund_trial_publish_failed: "+err.Error())
		}
	}
	_ = mw.WriteJSON(w, http.StatusOK, trialClaimResponse{
		PlanCode:        "TRIAL",
		GrantedMicro:    bonusMicro,
		GrantedCredits:  bonusMicro / MicroPerCredit,
		TrialExpireAt:   now.AddDate(0, 0, int(trialDays)).UTC().Format("2006-01-02T15:04:05Z07:00"),
		BonusExpireAt:   bonusExpire.UTC().Format("2006-01-02T15:04:05Z07:00"),
		BalanceAfter:    balanceAfter,
		AntiAbuseChecks: "DEVICE+PHONE+IP_SEGMENT+ACCOUNT",
	})
}

var (
	errTrialAlreadyClaimed = errors.New("fund: trial already claimed by this account")
	errTrialDeviceClaimed  = errors.New("fund: trial already granted to this device")
	// errTrialPhoneWindow is the number-level anti-arbitrage hit: the phone
	// collected a trial less than phoneclaim.WindowDays ago, possibly on a
	// different account.
	errTrialPhoneWindow = errors.New("fund: trial already granted to this phone number")
	// errTrialPhoneClaimMissing means the account has a phone hash but the
	// ledger has no row for it. Fail closed (design §6.3).
	errTrialPhoneClaimMissing = errors.New("fund: phone claim ledger row missing")
)

// trialRefusal answers every anti-abuse hit (device, phone, IP segment and the
// account's own earlier claim). The four factors deliberately share one message:
// naming the matched factor would tell an abuser which one to change next.
func trialRefusal() *apierr.APIError {
	return apierr.New(apierr.CodeTrialAlreadyClaimed)
}

// fingerprintHash is HMAC-SHA256(raw, FP_HMAC_KEY) in lowercase hex. The HMAC
// key keeps the hash from being reversible with a public rainbow table.
func fingerprintHash(key, raw string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// ipSegmentClaimed reports whether a claim audit row exists in the same /24
// (IPv4) or /64 (IPv6). The audit log is the only existing table that stores
// the claim IP; t_device_fingerprint has no IP column and migrations are
// frozen.
func (s *Service) ipSegmentClaimed(ctx context.Context, ip []byte) (bool, error) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || !addr.IsValid() {
		return false, nil
	}
	addr = addr.Unmap()
	prefix := 24
	if addr.Is6() {
		prefix = 64
	}
	current, err := addr.Prefix(prefix)
	if err != nil {
		return false, nil
	}
	since := s.now().AddDate(0, 0, -trialClaimLookbackDays)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT operator_ip FROM t_admin_audit_log
		 WHERE module = 'RISK' AND action = 'TRIAL_CLAIM'
		   AND operator_ip IS NOT NULL AND created_at >= ?`, since)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		other, ok := netip.AddrFromSlice(raw)
		if !ok {
			continue
		}
		other = other.Unmap()
		if other.Is4() != addr.Is4() {
			continue
		}
		otherPrefix, err := other.Prefix(prefix)
		if err != nil {
			continue
		}
		if otherPrefix == current {
			return true, nil
		}
	}
	return false, rows.Err()
}
