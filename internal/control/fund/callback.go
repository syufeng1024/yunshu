package fund

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/redisx"
)

// Callback statuses accepted from a payment channel.
const (
	callbackStatusSuccess = "SUCCESS"
	callbackStatusFailed  = "FAILED"
)

// CallbackAnomaly reasons. Every one of them is recorded (log + audit) and
// answered with 200 so the channel stops retrying an event that needs a human,
// not a repeat.
const (
	anomalyOrderNotFound = "ORDER_NOT_FOUND"
	anomalyOrderClosed   = "ORDER_CLOSED"
	anomalyOrderRefunded = "ORDER_REFUNDED"
	anomalyTxnMismatch   = "PAY_TXN_MISMATCH"
	anomalyChannelFailed = "CHANNEL_REPORTED_FAILURE"
)

type callbackRequest struct {
	OutTradeNo string `json:"out_trade_no"`
	TradeNo    string `json:"trade_no"`
	AmountFen  int64  `json:"amount_fen"`
	Status     string `json:"status"`
	Sign       string `json:"sign"`
}

// callbackResponse is the channel-facing acknowledgement. It is deliberately
// not the unified error envelope on success: payment platforms match on
// their own contract, so the body carries the order and the decision.
type callbackResponse struct {
	Code     string `json:"code"`
	OrderNo  string `json:"order_no,omitempty"`
	Credited bool   `json:"credited"`
	Replayed bool   `json:"replayed"`
	Reason   string `json:"reason,omitempty"`
}

// Callback implements POST /api/v1/pay/callback/{channel} (F10-1 / F10-2).
//
// It is a public route: the payment platform has no console session, so
// authentication is the signature, nothing else. The endpoint never degrades
// to "skip verification when the secret is missing" — a missing secret is a
// 503 and an ERROR log.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel := strings.ToUpper(strings.TrimSpace(r.PathValue("channel")))
	if channel != PayChannelWechat && channel != PayChannelAlipay {
		mw.WriteError(w, r, apierr.New(apierr.CodeUnknownEndpoint))
		return
	}
	secret := strings.TrimSpace(s.PaySecrets[channel])
	if secret == "" {
		// Faithfulness beats convenience: without the secret no callback can
		// be trusted, so the channel must keep retrying while ops fix it.
		s.logError(ctx, "fund_callback_secret_missing channel="+channel+
			" env="+paySecretEnvName(channel))
		mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable))
		return
	}

	var req callbackRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	outTradeNo := strings.TrimSpace(req.OutTradeNo)
	tradeNo := strings.TrimSpace(req.TradeNo)
	status := strings.ToUpper(strings.TrimSpace(req.Status))
	switch {
	case outTradeNo == "" || len(outTradeNo) > 28:
		mw.WriteError(w, r, mw.InvalidField("out_trade_no"))
		return
	case tradeNo == "" || len(tradeNo) > 64:
		mw.WriteError(w, r, mw.InvalidField("trade_no"))
		return
	case req.AmountFen <= 0:
		mw.WriteError(w, r, mw.InvalidField("amount_fen"))
		return
	case status != callbackStatusSuccess && status != callbackStatusFailed:
		mw.WriteError(w, r, mw.InvalidField("status"))
		return
	case req.Sign == "":
		mw.WriteError(w, r, mw.InvalidField("sign"))
		return
	}
	if !verifyCallbackSignature(secret, outTradeNo, tradeNo, req.AmountFen, status, req.Sign) {
		// A wrong signature is a wrong `sign` field: the integrator can fix it
		// by re-signing, so invalid_request_body with the field path is the
		// honest diagnosis (T12 P0 rule). Nothing is credited and the order
		// stays PENDING (G-03 And).
		s.logWarn(ctx, "fund_callback_signature_invalid channel="+channel+" order="+outTradeNo)
		s.auditCallback(ctx, 0, outTradeNo, "PAY_CALLBACK_REJECTED", map[string]any{
			"channel": channel, "trade_no": tradeNo, "reason": "SIGNATURE_INVALID",
		})
		mw.WriteError(w, r, mw.InvalidField("sign"))
		return
	}

	if status == callbackStatusFailed {
		// A failure notice changes no state; it is kept so support can answer
		// "the channel said the payment failed at 12:03".
		s.auditCallback(ctx, 0, outTradeNo, "PAY_CALLBACK_FAILED", map[string]any{
			"channel": channel, "trade_no": tradeNo, "amount_fen": req.AmountFen,
			"reason": anomalyChannelFailed,
		})
		_ = mw.WriteJSON(w, http.StatusOK, callbackResponse{
			Code: "SUCCESS", OrderNo: outTradeNo, Reason: anomalyChannelFailed,
		})
		return
	}

	resp, apiErr := s.settleCallback(ctx, r, channel, outTradeNo, tradeNo, req.AmountFen)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, resp)
}

// settleCallback applies a successful payment notice exactly once.
//
// Idempotency has three layers, in this order:
//  1. uk_ro_pay_txn(pay_channel, pay_txn_id): the same channel transaction can
//     only ever bind to one order row;
//  2. the state CAS "PENDING → PAID" inside a row lock: only one session can
//     move the order, and it does so in the same transaction as the credit;
//  3. uk_le_idem on the two ledger idem keys, which fails the whole
//     transaction if a credit for this order already exists.
//
// There is no "read the state, then write" gap: both statements run inside one
// transaction with the row locked FOR UPDATE.
func (s *Service) settleCallback(ctx context.Context, r *http.Request, channel, orderNo, tradeNo string, amountFen int64) (callbackResponse, *apierr.APIError) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "fund_callback_begin_failed: "+err.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		orderID       int64
		userID        int64
		orderType     string
		planID        sql.NullInt64
		payAmountFen  int64
		rechargeMicro int64
		bonusMicro    int64
		state         string
		existingTxn   sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, order_type, plan_id, pay_amount_fen,
		       recharge_micro, bonus_micro, state, pay_txn_id
		  FROM t_recharge_order
		 WHERE order_no = ?
		 FOR UPDATE`, orderNo).Scan(
		&orderID, &userID, &orderType, &planID, &payAmountFen,
		&rechargeMicro, &bonusMicro, &state, &existingTxn)
	if errors.Is(err, sql.ErrNoRows) {
		s.logError(ctx, "fund_callback_unknown_order channel="+channel+" order="+orderNo)
		s.auditCallback(ctx, 0, orderNo, "PAY_CALLBACK_REJECTED", map[string]any{
			"channel": channel, "trade_no": tradeNo, "reason": anomalyOrderNotFound,
		})
		return callbackResponse{Code: "SUCCESS", OrderNo: orderNo, Reason: anomalyOrderNotFound}, nil
	}
	if err != nil {
		s.logError(ctx, "fund_callback_order_read_failed: "+err.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}

	switch state {
	case OrderStatePaid:
		// Replay: the money is already credited exactly once.
		if existingTxn.Valid && existingTxn.String == tradeNo {
			return callbackResponse{Code: "SUCCESS", OrderNo: orderNo,
				Credited: true, Replayed: true}, nil
		}
		s.logError(ctx, "fund_callback_paid_by_other_txn order="+orderNo+
			" bound="+existingTxn.String+" incoming="+tradeNo)
		s.auditCallback(ctx, userID, orderNo, "PAY_CALLBACK_REJECTED", map[string]any{
			"channel": channel, "trade_no": tradeNo, "reason": anomalyTxnMismatch,
		})
		return callbackResponse{Code: "SUCCESS", OrderNo: orderNo, Reason: anomalyTxnMismatch}, nil
	case OrderStateClosed, OrderStateRefunded, OrderStatePartialRefunded, OrderStateRefunding:
		// The payment arrived after the order window closed (or after a
		// refund). Crediting it would create money nobody priced; a human has
		// to decide. 200 stops the channel from retrying forever.
		s.logError(ctx, "fund_callback_late_payment order="+orderNo+" state="+state)
		s.auditCallback(ctx, userID, orderNo, "PAY_CALLBACK_REJECTED", map[string]any{
			"channel": channel, "trade_no": tradeNo, "state": state,
			"amount_fen": amountFen, "reason": anomalyOrderClosed,
		})
		return callbackResponse{Code: "SUCCESS", OrderNo: orderNo, Reason: anomalyOrderClosed}, nil
	case OrderStatePending:
	default:
		s.logError(ctx, "fund_callback_unexpected_state order="+orderNo+" state="+state)
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}

	if amountFen != payAmountFen {
		s.logError(ctx, "fund_callback_amount_mismatch order="+orderNo+
			" want="+itoa(payAmountFen)+" got="+itoa(amountFen))
		s.auditCallback(ctx, userID, orderNo, "PAY_CALLBACK_REJECTED", map[string]any{
			"channel": channel, "trade_no": tradeNo,
			"want_fen": payAmountFen, "got_fen": amountFen,
		})
		return callbackResponse{}, mw.InvalidField("amount_fen")
	}

	now := s.now()
	// CAS: only a PENDING row may be paid. The rows-affected check is the
	// second half of the guard behind uk_ro_pay_txn.
	res, err := tx.ExecContext(ctx, `
		UPDATE t_recharge_order
		   SET state = 'PAID', pay_txn_id = ?, paid_at = ?, updated_at = ?,
		       refundable_micro = recharge_micro
		 WHERE id = ? AND state = 'PENDING'`,
		tradeNo, now, now, orderID)
	if err != nil {
		if isDuplicateKey(err) {
			// The same channel transaction is already bound to another order.
			s.logError(ctx, "fund_callback_duplicate_txn channel="+channel+" txn="+tradeNo)
			s.auditCallback(ctx, userID, orderNo, "PAY_CALLBACK_REJECTED", map[string]any{
				"channel": channel, "trade_no": tradeNo, "reason": anomalyTxnMismatch,
			})
			return callbackResponse{Code: "SUCCESS", OrderNo: orderNo, Reason: anomalyTxnMismatch}, nil
		}
		s.logError(ctx, "fund_callback_state_cas_failed: "+err.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		s.logError(ctx, "fund_callback_cas_rows_failed: "+err.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}
	if affected != 1 {
		// A concurrent callback won the CAS; this one is a replay.
		return callbackResponse{Code: "SUCCESS", OrderNo: orderNo,
			Credited: true, Replayed: true}, nil
	}

	bonusDays, apiErr := s.orderBonusDays(ctx, tx, planID, bonusMicro)
	if apiErr != nil {
		return callbackResponse{}, apiErr
	}

	// operatorID 0: the payment channel is the actor, so the ledger row keeps
	// operator_id NULL exactly as every other system entry does.
	grant, apiErr := s.grantOrderPaid(ctx, tx, orderID, userID, rechargeMicro, bonusMicro, bonusDays, 0, now)
	if apiErr != nil {
		return callbackResponse{}, apiErr
	}

	if orderType == OrderTypePlan {
		if !planID.Valid {
			s.logError(ctx, "fund_callback_plan_order_without_plan order="+orderNo)
			return callbackResponse{}, apierr.New(apierr.CodeInternalError)
		}
		if _, apiErr := s.applyPlanTx(ctx, tx, userID, planID.Int64, now); apiErr != nil {
			return callbackResponse{}, apiErr
		}
		if apiErr := s.markPlanPurchasedTx(ctx, tx, userID, now); apiErr != nil {
			return callbackResponse{}, apiErr
		}
	}

	if aerr := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   0, // the payment channel is the actor
		OperatorRole: "SYSTEM",
		OperatorIP:   audit.IPBytes(r),
		Module:       "FUND",
		Action:       "ORDER_PAID",
		TargetType:   "t_recharge_order",
		TargetID:     itoa(orderID),
		Before:       mustJSON(map[string]any{"state": OrderStatePending}),
		After: mustJSON(map[string]any{
			"state": OrderStatePaid, "channel": channel, "trade_no": tradeNo,
			"amount_fen": amountFen, "recharge_micro": rechargeMicro,
			"bonus_micro": bonusMicro, "bonus_expire_days": bonusDays,
			"recharge_batch_id": grant.RechargeBatchID,
			"bonus_batch_id":    grant.BonusBatchID,
			"recharge_entry_id": grant.RechargeEntryID,
			"bonus_entry_id":    grant.BonusEntryID,
		}),
		Result: "OK",
	}); aerr != nil {
		// Hard constraint 13: a fund write must be audited. Roll back rather
		// than leave an unaudited credit; the channel will retry.
		s.logError(ctx, "fund_callback_audit_failed: "+aerr.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}

	if err := tx.Commit(); err != nil {
		s.logError(ctx, "fund_callback_commit_failed: "+err.Error())
		return callbackResponse{}, apierr.New(apierr.CodeInternalError)
	}

	// Post-commit side effects. They are best-effort by design (hard
	// constraint 14): the money is already correct, and K26/a snapshot will
	// converge on the next read or refresh.
	s.invalidateBalance(ctx, userID)
	if orderType == OrderTypePlan && s.PublishRoutes != nil {
		if err := s.PublishRoutes(ctx); err != nil {
			s.logWarn(ctx, "fund_callback_route_publish_failed: "+err.Error())
		}
	}
	return callbackResponse{Code: "SUCCESS", OrderNo: orderNo, Credited: true}, nil
}

// grantOrderPaid writes the split credit inside the caller's transaction.
//
// operatorID attributes the ledger row: the operator id on a human
// confirmation, 0 for the channel callback. t_ledger_entry.operator_id is
// nullable and stays NULL for system entries (see the column comment), so 0
// maps to NULL rather than to a sentinel row — the ledger keeps one
// representation of "nobody signed for this".
func (s *Service) grantOrderPaid(ctx context.Context, tx *sql.Tx, orderID, userID, rechargeMicro, bonusMicro, bonusDays, operatorID int64, now time.Time) (ledger.GrantResult, *apierr.APIError) {
	bizType := ledger.BizTypeRecharge
	if bonusMicro > 0 {
		bizType = ledger.BizTypePlanGrant
	}
	p := ledger.GrantParams{
		UserID:          userID,
		RechargeMicro:   rechargeMicro,
		BonusMicro:      bonusMicro,
		BizType:         bizType,
		OrderID:         &orderID,
		IdemKeyRecharge: "recharge:" + itoa(orderID),
		IdemKeyBonus:    "bonus:" + itoa(orderID),
	}
	if operatorID > 0 {
		// A human booked this money, so the ledger row names that human and
		// attribution no longer depends on joining the audit table.
		id := operatorID
		p.OperatorID = &id
	}
	if bonusMicro > 0 {
		expire := now.Add(time.Duration(bonusDays) * 24 * time.Hour)
		p.BonusExpireAt = &expire
	}
	grant, err := s.Ledger.GrantTx(ctx, tx, p, now)
	if err != nil {
		s.logError(ctx, "fund_callback_grant_failed order="+itoa(orderID)+": "+err.Error())
		return ledger.GrantResult{}, apierr.New(apierr.CodeInternalError)
	}
	return grant, nil
}

// invalidateBalance deletes K26 so the gateway reloads the balance on its next
// admission instead of trusting a pre-payment value.
//
// The DEL runs on a detached context: the money has already moved, so a client
// that disconnects between commit and purge must not be able to cancel the
// invalidation and leave a stale mirror for up to a minute. Every caller of this
// helper (payment callback, manual confirm, redeem) is a background state
// change, and its bookkeeping side effect outlives the caller.
func (s *Service) invalidateBalance(ctx context.Context, userID int64) {
	if s.Redis == nil {
		return
	}
	cctx, cancel := s.Redis.CommandContext(context.WithoutCancel(ctx))
	defer cancel()
	if err := s.Redis.Raw().Del(cctx, redisx.BalanceKey(userID)).Err(); err != nil {
		s.logWarn(ctx, "fund_balance_mirror_invalidate_failed: "+err.Error())
	}
}

// ---------------------------------------------------------------------------
// signature
// ---------------------------------------------------------------------------

// callbackCanonical is the signed string. It deliberately covers the amount:
// a signature that ignored the amount would let a replay of a small payment
// settle a large order.
func callbackCanonical(outTradeNo, tradeNo string, amountFen int64, status string) string {
	return outTradeNo + "|" + tradeNo + "|" + strconv.FormatInt(amountFen, 10) + "|" + status
}

// signCallback computes hex(HMAC-SHA256(secret, canonical)). Tests and the
// local stack use the same helper, so the real chain exercises the exact
// production formula.
func signCallback(secret, outTradeNo, tradeNo string, amountFen int64, status string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(callbackCanonical(outTradeNo, tradeNo, amountFen, status)))
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyCallbackSignature compares in constant time.
func verifyCallbackSignature(secret, outTradeNo, tradeNo string, amountFen int64, status, sign string) bool {
	want := signCallback(secret, outTradeNo, tradeNo, amountFen, status)
	return hmac.Equal([]byte(want), []byte(strings.ToLower(strings.TrimSpace(sign))))
}

// paySecretEnvName is the environment variable an operator must set for the
// channel; the log line names it so the fix is obvious.
func paySecretEnvName(channel string) string {
	return "YUNSHU_PAY_CALLBACK_SECRET_" + strings.ToUpper(channel)
}

// auditCallback records a callback decision that is not a fund write.
func (s *Service) auditCallback(ctx context.Context, userID int64, orderNo, action string, payload map[string]any) {
	entry := audit.Entry{
		OperatorID:   userID,
		OperatorRole: "SYSTEM",
		Module:       "FUND",
		Action:       action,
		TargetType:   "t_recharge_order",
		TargetID:     orderNo,
		After:        mustJSON(payload),
		Result:       "REJECTED",
	}
	if err := s.Audit.Insert(ctx, s.DB, entry); err != nil {
		s.logWarn(ctx, "fund_callback_audit_failed: "+err.Error())
	}
}

func mustJSON(v any) []byte {
	raw, err := jsonx.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
