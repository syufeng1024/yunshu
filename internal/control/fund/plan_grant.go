package fund

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
)

// planGrant is the effective entitlement written by one purchase, reported so
// the caller can log and audit the numbers it actually granted.
type planGrant struct {
	Source          string
	RPM             int64
	TPM             int64
	Concurrency     int64
	DailyQuotaMicro *int64
	EffectiveTo     *time.Time
	TrialExpireAt   *time.Time
	// Overridden is true when the purchased tier is lower than the running
	// one, so the four dimensions were merged per dimension instead of being
	// replaced. V1.0 sells no downgrade, but a stale order can still arrive.
	Overridden bool
}

// applyPlanTx grants the plan entitlement inside the caller's transaction
// (PRD §3.2.1 / T13 hard constraint 7):
//
//   - 取高不叠加: the four dimensions are never summed;
//   - 跨档升档覆盖: buying a higher tier replaces the specs immediately and the
//     new entitlement starts at the payment instant;
//   - 重复购同档: the specs stay identical (the tier's own numbers), while the
//     credit batches accumulate — the batches are written by the caller;
//   - 权益有效期: paid tiers are permanent (effective_to NULL), TRIAL ends after
//     plan.trial_days and then falls back to the free default.
func (s *Service) applyPlanTx(ctx context.Context, tx *sql.Tx, userID, planID int64, now time.Time) (planGrant, *apierr.APIError) {
	var (
		code      string
		rpm, tpm  int64
		conc      int64
		daily     sql.NullInt64
		permanent int64
		rank      int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT code, rpm, tpm, concurrency, daily_quota_micro, permanent, sort_order
		  FROM t_plan WHERE id = ? AND enabled = 1`, planID).
		Scan(&code, &rpm, &tpm, &conc, &daily, &permanent, &rank)
	if errors.Is(err, sql.ErrNoRows) {
		// The row was deactivated between order creation and payment. The
		// credits are already granted in this transaction; refusing here would
		// roll them back, and the user paid. Keep the entitlement as the free
		// default and let the anomaly be visible in the log.
		s.logError(ctx, "fund_plan_grant_plan_missing user="+itoa(userID)+" plan="+itoa(planID))
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}
	if err != nil {
		s.logError(ctx, "fund_plan_grant_read_plan_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}

	// Current rank: the tier the account holds today (the free default has no
	// plan row, hence the -1 sentinel).
	currentRank := int64(-1)
	var currentPlanID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT plan_id FROM t_user WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
		userID).Scan(&currentPlanID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return planGrant{}, apierr.New(apierr.CodeInternalError)
		}
		s.logError(ctx, "fund_plan_grant_lock_user_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}
	if currentPlanID.Valid {
		if err := tx.QueryRowContext(ctx,
			`SELECT sort_order FROM t_plan WHERE id = ?`, currentPlanID.Int64).
			Scan(&currentRank); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logError(ctx, "fund_plan_grant_current_rank_failed: "+err.Error())
			return planGrant{}, apierr.New(apierr.CodeInternalError)
		}
	}

	grant := planGrant{Source: "PLAN"}
	if code == "TRIAL" {
		grant.Source = "TRIAL"
	}
	grant.RPM, grant.TPM, grant.Concurrency = rpm, tpm, conc
	if daily.Valid {
		d := daily.Int64
		grant.DailyQuotaMicro = &d
	}

	// Merge with the currently active policy row when the purchase does not
	// raise the tier. A higher (or equal) tier replaces the specs wholesale.
	var (
		curSource string
		curRPM    int64
		curTPM    int64
		curConc   int64
		curDaily  sql.NullInt64
	)
	hasCurrent := false
	err = tx.QueryRowContext(ctx, `
		SELECT source, rpm, tpm, concurrency_quota, daily_quota_micro
		  FROM t_rate_limit_policy
		 WHERE scope_type = 'USER' AND scope_id = ?
		   AND effective_from <= ?
		   AND (effective_to IS NULL OR effective_to > ?)
		 ORDER BY effective_from DESC, id DESC
		 LIMIT 1
		 FOR UPDATE`, userID, now, now).
		Scan(&curSource, &curRPM, &curTPM, &curConc, &curDaily)
	switch {
	case err == nil:
		hasCurrent = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.logError(ctx, "fund_plan_grant_read_policy_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}
	if hasCurrent && rank < currentRank {
		grant.Overridden = true
		grant.RPM = maxInt64(grant.RPM, curRPM)
		grant.TPM = maxInt64(grant.TPM, curTPM)
		grant.Concurrency = maxInt64(grant.Concurrency, curConc)
		// NULL means "no daily ceiling"; the larger of the two by definition.
		if !curDaily.Valid {
			grant.DailyQuotaMicro = nil
		}
	}

	// Entitlement window: TRIAL expires, paid tiers do not.
	var effectiveTo *time.Time
	var trialExpireAt *time.Time
	if code == "TRIAL" {
		days := s.configInt(ctx, config.KeyPlanTrialDays, 0)
		if days > 0 {
			end := now.Add(time.Duration(days) * 24 * time.Hour)
			effectiveTo = &end
			trialExpireAt = &end
			grant.EffectiveTo = &end
			grant.TrialExpireAt = &end
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE t_user SET plan_id = ?, trial_expire_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		planID, trialExpireAt, now, userID); err != nil {
		s.logError(ctx, "fund_plan_grant_update_user_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}

	// A running QUOTA_APPLY row carries an entitlement the plan tier does not:
	// the approved daily quota. Closing it with the other rows would silently
	// drop that increase (G12), so capture its daily value and window first and
	// reopen a continuation row below (D-27 #3). Only rows already in force are
	// matched: a future row stays untouched and simply wins once its window
	// begins (D-27 #1).
	//
	// daily_quota_micro is nullable since 0018 and NULL means "no ceiling"
	// (PRD §3.2.1). Scanning it into int64 made a NULL row fail the whole
	// confirmation with a 500, so it is read as NullInt64 and carried through
	// as NULL: turning it into 0 here would read as "no daily allowance at
	// all", the opposite of what the approved row says.
	var (
		quotaDaily sql.NullInt64
		quotaTo    time.Time
	)
	hasQuota := false
	err = tx.QueryRowContext(ctx, `
		SELECT daily_quota_micro, effective_to
		  FROM t_rate_limit_policy
		 WHERE scope_type = 'USER' AND scope_id = ?
		   AND source = 'QUOTA_APPLY'
		   AND effective_from <= ?
		   AND effective_to > ?
		 ORDER BY effective_from DESC, id DESC
		 LIMIT 1
		 FOR UPDATE`, userID, now, now).
		Scan(&quotaDaily, &quotaTo)
	switch {
	case err == nil:
		hasQuota = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		s.logError(ctx, "fund_plan_grant_read_quota_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}

	// Close the running rows. It is history, not a delete: an auditor must be
	// able to see which specs were in force before this purchase. The
	// effective_from guard keeps future rows out of the close: without it a
	// next-day QUOTA_APPLY row got effective_to = now < effective_from, a
	// negative window that erased the approved increase.
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_rate_limit_policy SET effective_to = ?, updated_at = ?
		  WHERE scope_type = 'USER' AND scope_id = ?
		    AND effective_from <= ?
		    AND (effective_to IS NULL OR effective_to > ?)`,
		now, now, userID, now, now); err != nil {
		s.logError(ctx, "fund_plan_grant_close_policy_failed: "+err.Error())
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}

	inserted := false
	var planFrom time.Time
	var lastErr error
	for attempt := 0; attempt < 5 && !inserted; attempt++ {
		// uk_rlp_scope_active is (scope_type, scope_id, effective_from): two
		// purchases inside the same millisecond need distinct start instants.
		from := now.Add(time.Duration(attempt) * time.Millisecond)
		_, lastErr = tx.ExecContext(ctx, `
			INSERT INTO t_rate_limit_policy
				(scope_type, scope_id, source, rpm, tpm, concurrency_quota,
				 daily_quota_micro, effective_from, effective_to,
				 fallback_policy_id, created_at, updated_at)
			VALUES ('USER', ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
			userID, grant.Source, grant.RPM, grant.TPM, grant.Concurrency,
			dailyValue(grant.DailyQuotaMicro), from, effectiveTo, now, now)
		if lastErr == nil {
			inserted = true
			planFrom = from
			break
		}
		if !isDuplicateKey(lastErr) {
			break
		}
	}
	if !inserted {
		s.logError(ctx, "fund_plan_grant_insert_policy_failed: "+errString(lastErr))
		return planGrant{}, apierr.New(apierr.CodeInternalError)
	}

	// Reopen the approved quota window one millisecond after the plan row so
	// the runtime's "latest active row wins" lookup keeps the daily quota while
	// serving the newly purchased specs. The old row was closed at now above
	// and stays visible as history; this row carries the same window end.
	if hasQuota {
		var quotaDailyValue *int64
		if quotaDaily.Valid {
			v := quotaDaily.Int64
			quotaDailyValue = &v
		}
		continued := false
		var contErr error
		for attempt := 0; attempt < 5; attempt++ {
			from := planFrom.Add(time.Duration(attempt+1) * time.Millisecond)
			if !quotaTo.After(from) {
				// The approved window ends inside the retry span; there is no
				// room for a continuation and the plan row takes over.
				continued = true
				break
			}
			_, contErr = tx.ExecContext(ctx, `
				INSERT INTO t_rate_limit_policy
					(scope_type, scope_id, source, rpm, tpm, concurrency_quota,
					 daily_quota_micro, effective_from, effective_to,
					 fallback_policy_id, created_at, updated_at)
				VALUES ('USER', ?, 'QUOTA_APPLY', ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
				userID, grant.RPM, grant.TPM, grant.Concurrency, dailyValue(quotaDailyValue),
				from, quotaTo, now, now)
			if contErr == nil {
				continued = true
				break
			}
			if !isDuplicateKey(contErr) {
				break
			}
		}
		if !continued {
			s.logError(ctx, "fund_plan_grant_continue_quota_failed: "+errString(contErr))
			return planGrant{}, apierr.New(apierr.CodeInternalError)
		}
	}
	return grant, nil
}

// dailyValue maps "no ceiling" to SQL NULL.
func dailyValue(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// markPlanPurchasedTx stamps t_user.plan_purchased_at with the settlement
// instant.
//
// The column exists since 0001 but nothing ever wrote it, so a PLAN order left
// it NULL and "when did this account buy its tier" had no answer outside the
// order table. It is stamped here — from the two order-settlement paths, inside
// their own transaction — rather than inside applyPlanTx, because applyPlanTx
// also serves the TRIAL claim (trial.go): a trial is granted, not purchased.
func (s *Service) markPlanPurchasedTx(ctx context.Context, tx *sql.Tx, userID int64, now time.Time) *apierr.APIError {
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_user SET plan_purchased_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`, now, now, userID); err != nil {
		s.logError(ctx, "fund_plan_purchased_at_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}
