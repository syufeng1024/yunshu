package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/redisx"
)

// Errors that make an account unusable for the self-service recovery paths.
//
// They are separate values rather than one bool so a caller can log the case
// that is genuinely surprising (an ambiguous handset) while still answering with
// the same 200 that an unknown address gets.
var (
	// ErrAmbiguousAccount means the identifier matches more than one live row.
	//
	// This is reachable only for a phone number, and only while the historical
	// rows are still being backfilled: t_user.phone has no unique key (0011 moved
	// uniqueness to uk_user_phone_hmac), and every write path now stores
	// phone_hmac, so once `backfill-phone-hmac verify` reports zero differences a
	// second live row with the same number is impossible.
	//
	// Logging in takes the first row silently; resetting a password must not,
	// because that would reset an account the caller may not own.
	ErrAmbiguousAccount = errors.New("auth: identifier matches more than one account")
	// ErrAccountUnavailable means the account exists but must not be recovered:
	// status is DISABLED or FROZEN_OVERDRAFT. Freezing is an access recall, and
	// self-service recovery must not become the way around it.
	ErrAccountUnavailable = errors.New("auth: account is not usable for self-service recovery")
)

// findUserUnique is findUser for the recovery and binding paths: it refuses an
// ambiguous match instead of silently taking the first row.
//
// findUser (login) is deliberately left alone. Changing it would alter the
// login semantics for every existing handset account in this increment, which is
// a different change with a different review.
//
// The column is a compile-time constant chosen by a closed switch, never client
// input; Value is always bound through a placeholder.
func (s *Service) findUserUnique(ctx context.Context, ident Identifier) (userRow, error) {
	column := "email"
	switch ident.Kind {
	case KindPhone:
		column = "phone"
	case KindUsername:
		column = "username"
	}
	// LIMIT 2 is the whole point: one row is unambiguous, two rows mean the
	// uniqueness assumption does not hold and neither row may be acted on.
	q := `SELECT id, password_hash, password_algo, status, role,
		COALESCE(email, ''), COALESCE(phone, ''), COALESCE(username, ''), nickname,
		password_is_initial
		FROM t_user
		WHERE deleted_at IS NULL AND ` + column + ` = ?
		LIMIT 2`
	rows, err := s.DB.QueryContext(ctx, q, ident.Value)
	if err != nil {
		return userRow{}, err
	}
	defer rows.Close() //nolint:errcheck // read-only

	found := make([]userRow, 0, 2)
	for rows.Next() {
		var row userRow
		if err := rows.Scan(
			&row.uid, &row.hash, &row.algo, &row.status, &row.role,
			&row.email, &row.phone, &row.username, &row.nickname, &row.passwordIsInitial,
		); err != nil {
			return userRow{}, err
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return userRow{}, err
	}
	switch len(found) {
	case 0:
		return userRow{}, sql.ErrNoRows
	case 1:
		return found[0], nil
	default:
		return userRow{}, ErrAmbiguousAccount
	}
}

// findResetAccount resolves the account a password reset may act on.
//
// It combines the two "usable" conditions — exactly one live row, and status
// NORMAL — so the send-code path (silently drop the code) and the submit path
// (answer invalid_credentials) cannot disagree about what "usable" means.
func (s *Service) findResetAccount(ctx context.Context, ident Identifier) (userRow, error) {
	row, err := s.findUserUnique(ctx, ident)
	if err != nil {
		return userRow{}, err
	}
	if row.status != "NORMAL" {
		return userRow{}, ErrAccountUnavailable
	}
	return row, nil
}

// isNotFound reports sql.ErrNoRows.
func isNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// isAccountUnavailable reports the two closed shapes findUserUnique and
// findResetAccount use to say "no recoverable account".
func isAccountUnavailable(err error) bool {
	return errors.Is(err, ErrAmbiguousAccount) || errors.Is(err, ErrAccountUnavailable)
}

// consumeUIDQuota charges one unit of a signed-in account's daily budget.
//
// It exists for the bind flow only. The per-address limits do not bound it: a
// compromised session could otherwise walk a list of mailboxes, each with a
// fresh cooldown and a fresh address quota, and turn our domain into free
// outbound mail. The limit is per (purpose, uid), so it also cannot be used to
// starve a different flow.
func (s *Service) consumeUIDQuota(ctx context.Context, purpose string, uid int64, limit int) *apierr.APIError {
	rdb, err := s.verifyRedis()
	if err != nil {
		s.errorLog(ctx, "verification_redis_unavailable: "+err.Error())
		return apierr.New(apierr.CodeAuthUnavailable)
	}
	key := redisx.VerifyCodeUIDQuotaKey(purpose, uid)
	n, err := rdb.Raw().Incr(ctx, key).Result()
	if err != nil {
		s.errorLog(ctx, "verification_uid_quota_failed: "+err.Error())
		return apierr.New(apierr.CodeAuthUnavailable)
	}
	if n == 1 {
		_ = rdb.Raw().Expire(ctx, key, 24*time.Hour).Err()
	}
	if n > int64(limit) {
		return apierr.Newf(apierr.CodeRPMLimitExceeded, limit, int((24 * time.Hour).Seconds())).
			WithHint("该账号今日获取验证码的次数已达上限，请明天再试")
	}
	return nil
}
