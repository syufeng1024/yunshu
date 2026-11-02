package fund

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// G22 C1: the ledger row has to say who booked the money. Before the operator
// id reached grantOrderPaid, t_ledger_entry.operator_id stayed NULL on a human
// confirmation and attributing a credit meant joining t_admin_audit_log.
func TestOperatorConfirmWritesLedgerOperatorID(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	operator := newTestUser(t, nil)

	order, _ := createOrder(t, svc, uid, map[string]any{
		"order_type":     OrderTypeCustom,
		"pay_amount_fen": 10000,
		"pay_channel":    PayChannelWechat,
	})
	if rec := confirmOrder(t, svc, operator, order.ID, "LEDGER-OP-0001", ""); rec.Code != http.StatusOK {
		t.Fatalf("confirm status = %d body=%s", rec.Code, rec.Body.String())
	}

	operators := ledgerOperatorIDs(t, uid)
	if len(operators) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(operators))
	}
	if !operators[0].Valid || operators[0].Int64 != operator {
		t.Fatalf("ledger operator_id = %v, want the operator %d", operators[0], operator)
	}
}

// The callback path has no operator: its ledger row keeps operator_id NULL,
// which is the one representation of "system" this column has (DDL comment).
func TestCallbackLedgerOperatorIDStaysNull(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)

	order, _ := createOrder(t, svc, uid, map[string]any{
		"order_type":     OrderTypeCustom,
		"pay_amount_fen": 5000,
		"pay_channel":    PayChannelWechat,
	})
	rec := successfulCallback(t, svc, order, "WXLEDGER"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}

	operators := ledgerOperatorIDs(t, uid)
	if len(operators) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(operators))
	}
	if operators[0].Valid {
		t.Fatalf("ledger operator_id = %d, want NULL for a system credit", operators[0].Int64)
	}
}

// G22 C2: buying a PLAN stamps t_user.plan_purchased_at with the settlement
// instant. Both settlement paths must do it; the callback is checked here and
// the operator confirmation below.
func TestPlanPurchaseStampsPlanPurchasedAt(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)

	order := payPlanOrder(t, svc, uid, "STANDARD")
	purchasedAt, paidAt := planPurchaseTimestamps(t, uid, order.ID)
	if !purchasedAt.Valid {
		t.Fatal("plan_purchased_at is NULL after a PLAN order settled by callback")
	}
	if !paidAt.Valid {
		t.Fatal("paid_at is NULL after a callback settlement")
	}
	if !purchasedAt.Time.UTC().Equal(paidAt.Time.UTC()) {
		t.Fatalf("plan_purchased_at = %s, paid_at = %s; want the same instant",
			purchasedAt.Time.UTC(), paidAt.Time.UTC())
	}
}

// The operator path writes the same column: a plan sold by an offline transfer
// is a purchase too.
func TestOperatorConfirmPlanStampsPlanPurchasedAt(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	operator := newTestUser(t, nil)
	plan := planByCode(t, "STANDARD")

	order, _ := createOrder(t, svc, uid, map[string]any{
		"order_type":  OrderTypePlan,
		"plan_id":     plan.ID,
		"pay_channel": PayChannelWechat,
	})
	if rec := confirmOrder(t, svc, operator, order.ID, "PLAN-STAMP-0001", ""); rec.Code != http.StatusOK {
		t.Fatalf("confirm plan status = %d body=%s", rec.Code, rec.Body.String())
	}

	purchasedAt, paidAt := planPurchaseTimestamps(t, uid, order.ID)
	if !purchasedAt.Valid {
		t.Fatal("plan_purchased_at is NULL after an operator confirmation")
	}
	if !paidAt.Valid || !purchasedAt.Time.UTC().Equal(paidAt.Time.UTC()) {
		t.Fatalf("plan_purchased_at = %v, paid_at = %v; want the same instant",
			purchasedAt, paidAt)
	}
}

// A TRIAL claim is a grant, not a purchase, so it must not stamp the column.
func TestTrialClaimDoesNotStampPlanPurchasedAt(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	claimTrial(t, svc, uid)

	var purchasedAt sql.NullTime
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT plan_purchased_at FROM t_user WHERE id = ?`, uid).Scan(&purchasedAt); err != nil {
		t.Fatalf("read plan_purchased_at: %v", err)
	}
	if purchasedAt.Valid {
		t.Fatalf("plan_purchased_at = %s after a trial claim; want NULL", purchasedAt.Time)
	}
}

// G14: an approved QUOTA_APPLY row may carry NULL daily_quota_micro, which means
// "no ceiling" since 0018. Scanning it into an int64 failed the whole purchase
// with a 500; the continuation row must keep NULL (unlimited), not 0 (nothing
// allowed).
func TestPlanGrantSurvivesNullQuotaDaily(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	operator := newTestUser(t, nil)
	plan := planByCode(t, "STANDARD")

	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := testDB.ExecContext(ctx, `
		INSERT INTO t_rate_limit_policy
			(scope_type, scope_id, source, rpm, tpm, concurrency_quota,
			 daily_quota_micro, effective_from, effective_to, fallback_policy_id,
			 created_at, updated_at)
		VALUES ('USER', ?, 'QUOTA_APPLY', 600, 60000, 10, NULL, ?, ?, NULL, ?, ?)`,
		uid, now.Add(-time.Hour), now.Add(30*24*time.Hour), now, now); err != nil {
		t.Fatalf("insert QUOTA_APPLY row with NULL quota: %v", err)
	}

	order, _ := createOrder(t, svc, uid, map[string]any{
		"order_type":  OrderTypePlan,
		"plan_id":     plan.ID,
		"pay_channel": PayChannelWechat,
	})
	rec := confirmOrder(t, svc, operator, order.ID, "NULLQUOTA-0001", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm with NULL daily quota status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Two QUOTA_APPLY rows now: the closed original and the continuation.
	var total int
	if err := testDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_rate_limit_policy
		  WHERE scope_type = 'USER' AND scope_id = ? AND source = 'QUOTA_APPLY'`,
		uid).Scan(&total); err != nil {
		t.Fatalf("count quota rows: %v", err)
	}
	if total != 2 {
		t.Fatalf("QUOTA_APPLY rows = %d, want 2 (closed original + continuation)", total)
	}

	var continuedDaily sql.NullInt64
	if err := testDB.QueryRowContext(ctx, `
		SELECT daily_quota_micro FROM t_rate_limit_policy
		 WHERE scope_type = 'USER' AND scope_id = ? AND source = 'QUOTA_APPLY'
		   AND (effective_to IS NULL OR effective_to > ?)
		 ORDER BY effective_from DESC, id DESC LIMIT 1`, uid, time.Now().UTC()).
		Scan(&continuedDaily); err != nil {
		t.Fatalf("read continuation row: %v", err)
	}
	if continuedDaily.Valid {
		t.Fatalf("continuation daily_quota_micro = %d; want NULL (no ceiling), not 0",
			continuedDaily.Int64)
	}
}

// ledgerOperatorIDs returns operator_id per entry, oldest first, so a test can
// assert both the value and the NULL-ness.
func ledgerOperatorIDs(t *testing.T, uid int64) []sql.NullInt64 {
	t.Helper()
	rows, err := testDB.QueryContext(context.Background(),
		`SELECT operator_id FROM t_ledger_entry WHERE user_id = ? ORDER BY id`, uid)
	if err != nil {
		t.Fatalf("read ledger operators: %v", err)
	}
	defer rows.Close()
	var out []sql.NullInt64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan ledger operator: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ledger operator rows: %v", err)
	}
	return out
}

// planPurchaseTimestamps reads the two columns a settled PLAN order must agree
// on: t_user.plan_purchased_at and the order's paid_at.
func planPurchaseTimestamps(t *testing.T, uid, orderID int64) (purchasedAt, paidAt sql.NullTime) {
	t.Helper()
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT plan_purchased_at FROM t_user WHERE id = ?`, uid).Scan(&purchasedAt); err != nil {
		t.Fatalf("read plan_purchased_at: %v", err)
	}
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT paid_at FROM t_recharge_order WHERE id = ?`, orderID).Scan(&paidAt); err != nil {
		t.Fatalf("read paid_at: %v", err)
	}
	return purchasedAt, paidAt
}

// claimTrial drives the trial claim endpoint so the "grant, not purchase" rule
// is checked through the real handler rather than inferred.
func claimTrial(t *testing.T, svc *Service, uid int64) {
	t.Helper()
	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "g22-trial-" + strconv.FormatInt(uid, 10)},
		uid, "203.0.113.7:1234", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trial claim status = %d body=%s", rec.Code, rec.Body.String())
	}
}
