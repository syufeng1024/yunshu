package fund

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
)

// Operator-side order surface: the human half of the recharge loop (F10-1 /
// B-46 A 档).
//
// An offline payment (对公转账) never reaches a payment platform, so nothing
// calls POST /api/v1/pay/callback/{channel} and the order would stay PENDING
// forever. This file is the path a finance operator takes instead: list the
// orders that wait for money, read the bank statement, confirm the arrival.
//
// The confirmation runs exactly the transaction the channel callback runs —
// CAS on state='PENDING', grantOrderPaid, applyPlanTx for a PLAN order (plus
// its plan_purchased_at stamp), one audit row in the same transaction — so the
// two settlement paths can never disagree about what "paid" means. The credit
// SQL is not repeated here: it lives in ledger.GrantTx and is reached through
// grantOrderPaid.

const (
	// bankTxnNoMin and bankTxnNoMax bound the statement number the operator
	// types. t_recharge_order.pay_txn_id is varchar(64) and shares
	// uk_ro_pay_txn with channel transaction ids, so both paths obey one
	// ceiling.
	bankTxnNoMin = 6
	bankTxnNoMax = 64

	// confirmNoteMax caps the operator's remark. The note is explanatory, not
	// accounting data: it travels in the audit payload only.
	confirmNoteMax = 255

	// closeReasonMin/Max bound the reason an operator gives for voiding an
	// order. A one-word reason ("x") documents nothing; the ceiling matches
	// t_recharge_order-free free text and keeps the audit payload readable.
	closeReasonMin = 2
	closeReasonMax = 100
)

// bankTxnNoPattern is the statement-number alphabet: letters, digits, hyphen
// and underscore. A bank reference never contains a space, so accepting one
// would only produce an unsearchable key.
var bankTxnNoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

// adminOrderView is the operator console's order row: the account's own view
// plus the identity fields finance needs to match a customer to a statement.
type adminOrderView struct {
	orderView
	UserID       int64  `json:"user_id"`
	UserEmail    string `json:"user_email"`
	UserNickname string `json:"user_nickname"`
	// PayTxnID is the bound transaction id. For a channel payment it is the
	// platform's trade_no; for an operator confirmation it is the bank
	// statement number the operator typed. Empty until the order is paid.
	PayTxnID string `json:"pay_txn_id,omitempty"`
}

type adminOrderListResponse struct {
	Items    []adminOrderView `json:"items"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Total    int64            `json:"total"`
}

type confirmPaymentRequest struct {
	// BankTxnNo is the operator's proof of payment. It is required and is
	// what makes a repeat confirmation idempotent instead of a second credit.
	BankTxnNo string `json:"bank_txn_no"`
	Note      string `json:"note"`
}

type confirmPaymentResponse struct {
	Order adminOrderView `json:"order"`
	// Credited is true once the money is in the account — this response and
	// every idempotent replay both report it, so a UI retry is safe.
	Credited bool `json:"credited"`
	// Replayed is true when this statement number had already settled this
	// order. Nothing was written.
	Replayed bool `json:"replayed"`
}

type closeOrderRequest struct {
	// Reason is the operator's justification, required: a voided order with
	// no stated reason cannot be defended to the customer or to an auditor.
	Reason string `json:"reason"`
}

type closeOrderResponse struct {
	Order adminOrderView `json:"order"`
	// Closed is true once the order can no longer be paid — this response and
	// every idempotent replay both report it.
	Closed bool `json:"closed"`
	// Replayed is true when the order was already CLOSED; nothing was written.
	Replayed bool `json:"replayed"`
}

// adminOrderSelect selects the console order columns plus the account
// identity and the plan code. The order column list stays in step with
// orderColumns so scanOrder and this view never drift.
const adminOrderSelect = `
	ro.id, ro.order_no, ro.order_type, ro.plan_id, ro.pay_amount_fen,
	ro.recharge_micro, ro.bonus_micro, ro.pay_channel, ro.state, ro.paid_at,
	ro.expire_at, ro.created_at, ro.pay_txn_id,
	COALESCE(p.code, ''), ro.user_id, COALESCE(u.email, ''), COALESCE(u.nickname, '')`

// adminOrderFrom is the join shared by the list, the count and the read-back.
const adminOrderFrom = `
	FROM t_recharge_order ro
	LEFT JOIN t_plan p ON p.id = ro.plan_id
	LEFT JOIN t_user u ON u.id = ro.user_id`

// scanAdminOrder decodes one joined row. planID stays unexported exactly as
// orderView keeps it: the console shows the plan code, never the raw id.
func scanAdminOrder(sc scanner) (adminOrderView, error) {
	var (
		v      adminOrderView
		planID sql.NullInt64
		paidAt sql.NullTime
		txn    sql.NullString
	)
	if err := sc.Scan(&v.ID, &v.OrderNo, &v.OrderType, &planID, &v.PayAmountFen,
		&v.RechargeMicro, &v.BonusMicro, &v.PayChannel, &v.State,
		&paidAt, &v.ExpireAt, &v.CreatedAt, &txn,
		&v.PlanCode, &v.UserID, &v.UserEmail, &v.UserNickname); err != nil {
		return adminOrderView{}, err
	}
	if planID.Valid {
		id := planID.Int64
		v.planID = &id
	}
	if paidAt.Valid {
		t := paidAt.Time.UTC()
		v.PaidAt = &t
	}
	if txn.Valid {
		v.PayTxnID = txn.String
	}
	v.PayAmountYuan = v.PayAmountFen / FenPerYuan
	v.Recharge = microToFen(v.RechargeMicro) / FenPerYuan
	v.Bonus = microToFen(v.BonusMicro) / FenPerYuan
	v.ExpireAt = v.ExpireAt.UTC()
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

// ListAdminOrders implements GET /api/admin/orders (fund.read).
//
// Unlike the account list this one is not scoped to a session: it exists so
// finance can see every customer's unpaid order. The state filter and the
// keyword search intersect, the same way the console list does.
func (s *Service) ListAdminOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, apiErr := mw.ParsePage(r, "created_at", "paid_at")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	query := r.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	if len([]rune(q)) > 64 {
		mw.WriteError(w, r, mw.InvalidQueryField("q"))
		return
	}
	states, apiErr := parseOrderStatus(query.Get("state"))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	where := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if len(states) == 1 {
		where = append(where, "ro.state = ?")
		args = append(args, states[0])
	} else if len(states) > 1 {
		placeholders := make([]string, len(states))
		for i, st := range states {
			placeholders[i] = "?"
			args = append(args, st)
		}
		where = append(where, "ro.state IN ("+strings.Join(placeholders, ",")+")")
	}
	if q != "" {
		// One box, two columns: an operator holds either the order number
		// from the customer's screenshot or the payer's e-mail address.
		where = append(where, `(ro.order_no LIKE ? ESCAPE '!' OR u.email LIKE ? ESCAPE '!')`)
		pattern := likePattern(q)
		args = append(args, pattern, pattern)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*)`+adminOrderFrom+whereSQL, args...).Scan(&total); err != nil {
		s.logError(ctx, "fund_admin_order_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	orderBy := "ro.created_at"
	if page.Sort == "paid_at" {
		orderBy = "ro.paid_at"
	}
	direction := "DESC"
	if strings.EqualFold(page.Order, "asc") {
		direction = "ASC"
	}
	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+adminOrderSelect+adminOrderFrom+whereSQL+
			` ORDER BY `+orderBy+` `+direction+`, ro.id `+direction+` LIMIT ? OFFSET ?`,
		queryArgs...)
	if err != nil {
		s.logError(ctx, "fund_admin_order_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]adminOrderView, 0, page.Size)
	for rows.Next() {
		v, err := scanAdminOrder(rows)
		if err != nil {
			s.logError(ctx, "fund_admin_order_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		s.logError(ctx, "fund_admin_order_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, adminOrderListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// adminOrderByID reads one order for the operator surface. It runs on the
// pool, not on a transaction: every caller uses it after the money write has
// either committed or been decided to be a replay.
func (s *Service) adminOrderByID(ctx context.Context, orderID int64) (adminOrderView, *apierr.APIError) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+adminOrderSelect+adminOrderFrom+` WHERE ro.id = ?`, orderID)
	v, err := scanAdminOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return adminOrderView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		s.logError(ctx, "fund_admin_order_read_failed: "+err.Error())
		return adminOrderView{}, apierr.New(apierr.CodeInternalError)
	}
	return v, nil
}

// ConfirmOrderPayment implements
// POST /api/admin/orders/{id}/confirm-payment (fund.approve).
//
// OPS holds neither fund.read nor fund.approve, so the route middleware
// answers 403 before this handler runs: recording money is a finance act.
func (s *Service) ConfirmOrderPayment(w http.ResponseWriter, r *http.Request) {
	operatorID, operatorRole, apiErr := actorOf(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	orderID, apiErr := operatorPathID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req confirmPaymentRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	bankTxnNo := strings.TrimSpace(req.BankTxnNo)
	if !bankTxnNoPattern.MatchString(bankTxnNo) {
		// The operator fixes this by copying the statement number again, so
		// it is a body-value problem, not a conflict.
		mw.WriteError(w, r, mw.InvalidField("bank_txn_no"))
		return
	}
	note := strings.TrimSpace(req.Note)
	if len([]rune(note)) > confirmNoteMax {
		mw.WriteError(w, r, mw.InvalidField("note"))
		return
	}

	resp, apiErr := s.confirmOrderPaymentTx(r.Context(), r, operatorID, operatorRole, orderID, bankTxnNo, note)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, resp)
}

// confirmOrderPaymentTx settles one order exactly once.
//
// The transaction is the callback's transaction with a different actor:
//
//  1. the row is locked FOR UPDATE, so the state switch and the CAS below
//     cannot interleave with a concurrent confirmation or channel callback;
//  2. only a PENDING row is moved, and the move carries refundable_micro so a
//     later refund has the same ceiling the channel path would have set;
//  3. the credit is written by grantOrderPaid (ledger.GrantTx), never by SQL
//     copied into this file, and its uk_le_idem keys make a second credit for
//     the same order fail the whole transaction;
//  4. the audit row commits with the money, so an unaudited credit cannot
//     exist.
func (s *Service) confirmOrderPaymentTx(ctx context.Context, r *http.Request, operatorID int64, operatorRole string, orderID int64, bankTxnNo, note string) (confirmPaymentResponse, *apierr.APIError) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "fund_confirm_begin_failed: "+err.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		orderNo       string
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
		SELECT order_no, user_id, order_type, plan_id, pay_amount_fen,
		       recharge_micro, bonus_micro, state, pay_txn_id
		  FROM t_recharge_order
		 WHERE id = ?
		 FOR UPDATE`, orderID).Scan(
		&orderNo, &userID, &orderType, &planID, &payAmountFen,
		&rechargeMicro, &bonusMicro, &state, &existingTxn)
	if errors.Is(err, sql.ErrNoRows) {
		s.logWarn(ctx, "fund_confirm_unknown_order id="+itoa(orderID))
		return confirmPaymentResponse{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		s.logError(ctx, "fund_confirm_order_read_failed: "+err.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}

	switch state {
	case OrderStatePending:
		// The only state that may be settled.
	case OrderStatePaid:
		if existingTxn.Valid && existingTxn.String == bankTxnNo {
			// Idempotent replay: the same statement number already settled
			// this order, so the money is in the account exactly once.
			view, apiErr := s.adminOrderByID(ctx, orderID)
			if apiErr != nil {
				return confirmPaymentResponse{}, apiErr
			}
			return confirmPaymentResponse{Order: view, Credited: true, Replayed: true}, nil
		}
		s.logWarn(ctx, "fund_confirm_txn_mismatch order="+orderNo+
			" bound="+existingTxn.String)
		return confirmPaymentResponse{}, confirmConflict(
			"订单已由流水号 %s 结算，与本次填写的 %s 不一致", existingTxn.String, bankTxnNo)
	default:
		// CLOSED / REFUNDING / REFUNDED / PARTIAL_REFUNDED: money recorded
		// against one of these would be money the system has already moved
		// on from. A human has to reconcile it, so refuse and name the state.
		s.logWarn(ctx, "fund_confirm_state_refused order="+orderNo+" state="+state)
		return confirmPaymentResponse{}, confirmConflict("订单当前状态为 %s", state)
	}

	now := s.now()
	// CAS: only a PENDING row may be paid. RowsAffected is the second guard
	// behind uk_ro_pay_txn.
	res, err := tx.ExecContext(ctx, `
		UPDATE t_recharge_order
		   SET state = 'PAID', pay_txn_id = ?, paid_at = ?, updated_at = ?,
		       refundable_micro = recharge_micro
		 WHERE id = ? AND state = 'PENDING'`,
		bankTxnNo, now, now, orderID)
	if err != nil {
		if isDuplicateKey(err) {
			// uk_ro_pay_txn: the statement number is already bound to another
			// order. Copying a number twice is an operator mistake, and the
			// message says which bound value to fix.
			s.logError(ctx, "fund_confirm_duplicate_txn order="+orderNo+" txn="+bankTxnNo)
			return confirmPaymentResponse{}, confirmConflict(
				"银行流水号 %s 已绑定其他订单", bankTxnNo)
		}
		s.logError(ctx, "fund_confirm_state_cas_failed: "+err.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		s.logError(ctx, "fund_confirm_cas_rows_failed: "+err.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}
	if affected != 1 {
		// The row was PENDING under the lock above, so this means a concurrent
		// writer moved it between the lock release and the CAS. Re-read and
		// answer the same way the state switch would have.
		var currentState string
		var currentTxn sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT state, pay_txn_id FROM t_recharge_order WHERE id = ?`, orderID).
			Scan(&currentState, &currentTxn); err != nil {
			s.logError(ctx, "fund_confirm_cas_reread_failed: "+err.Error())
			return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
		}
		if currentState == OrderStatePaid && currentTxn.Valid && currentTxn.String == bankTxnNo {
			view, apiErr := s.adminOrderByID(ctx, orderID)
			if apiErr != nil {
				return confirmPaymentResponse{}, apiErr
			}
			return confirmPaymentResponse{Order: view, Credited: true, Replayed: true}, nil
		}
		return confirmPaymentResponse{}, confirmConflict("订单当前状态为 %s", currentState)
	}

	bonusDays, apiErr := s.orderBonusDays(ctx, tx, planID, bonusMicro)
	if apiErr != nil {
		return confirmPaymentResponse{}, apiErr
	}

	// The operator is the actor here: the ledger row carries their id, so the
	// credit is attributable without joining t_admin_audit_log.
	grant, apiErr := s.grantOrderPaid(ctx, tx, orderID, userID, rechargeMicro, bonusMicro, bonusDays, operatorID, now)
	if apiErr != nil {
		return confirmPaymentResponse{}, apiErr
	}

	if orderType == OrderTypePlan {
		if !planID.Valid {
			s.logError(ctx, "fund_confirm_plan_order_without_plan order="+orderNo)
			return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
		}
		if _, apiErr := s.applyPlanTx(ctx, tx, userID, planID.Int64, now); apiErr != nil {
			return confirmPaymentResponse{}, apiErr
		}
		if apiErr := s.markPlanPurchasedTx(ctx, tx, userID, now); apiErr != nil {
			return confirmPaymentResponse{}, apiErr
		}
	}

	if aerr := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   operatorID,
		OperatorRole: operatorRole,
		OperatorIP:   audit.IPBytes(r),
		Module:       "FUND",
		Action:       "ORDER_CONFIRMED_BY_OPERATOR",
		TargetType:   "t_recharge_order",
		TargetID:     itoa(orderID),
		Before:       mustJSON(map[string]any{"state": OrderStatePending}),
		After: mustJSON(map[string]any{
			"state":          OrderStatePaid,
			"order_no":       orderNo,
			"bank_txn_no":    bankTxnNo,
			"pay_amount_fen": payAmountFen,
			"note":           note,
			"recharge_micro": rechargeMicro,
			"bonus_micro":    bonusMicro,
			"bonus_days":     bonusDays,
			"recharge_entry": grant.RechargeEntryID,
			"bonus_entry":    grant.BonusEntryID,
		}),
		Reason: note,
		Result: "OK",
	}); aerr != nil {
		// A fund write must be audited: roll back rather than leave an
		// unaudited credit behind.
		s.logError(ctx, "fund_confirm_audit_failed: "+aerr.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}

	if err := tx.Commit(); err != nil {
		s.logError(ctx, "fund_confirm_commit_failed: "+err.Error())
		return confirmPaymentResponse{}, apierr.New(apierr.CodeInternalError)
	}

	// Post-commit side effects are best-effort: the money is already correct.
	s.invalidateBalance(ctx, userID)
	if orderType == OrderTypePlan && s.PublishRoutes != nil {
		if perr := s.PublishRoutes(ctx); perr != nil {
			s.logWarn(ctx, "fund_confirm_route_publish_failed: "+perr.Error())
		}
	}

	view, apiErr := s.adminOrderByID(ctx, orderID)
	if apiErr != nil {
		return confirmPaymentResponse{}, apiErr
	}
	return confirmPaymentResponse{Order: view, Credited: true}, nil
}

// CloseOrder implements POST /api/admin/orders/{id}/close (fund.approve).
//
// An order that will never be paid must be voided, or the finance queue grows
// forever: a customer who never transfers leaves a PENDING row that nothing
// expires until jobsd grows an auto-close job. Closing moves the order to
// CLOSED, which is terminal for payment — no money moves, no ledger entry is
// written, and a later confirmation is refused by its own CAS.
func (s *Service) CloseOrder(w http.ResponseWriter, r *http.Request) {
	operatorID, operatorRole, apiErr := actorOf(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	orderID, apiErr := operatorPathID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req closeOrderRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if n := len([]rune(reason)); n < closeReasonMin || n > closeReasonMax {
		mw.WriteError(w, r, mw.InvalidField("reason"))
		return
	}

	resp, apiErr := s.closeOrderTx(r.Context(), r, operatorID, operatorRole, orderID, reason)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, resp)
}

// closeOrderTx voids one PENDING order.
//
// The transaction is deliberately small: a state CAS and one audit row. No
// ledger, no balance, no batch — a voided order never held money. The lock and
// the CAS together mean a confirmation racing this close either wins (order is
// PAID and this close is refused) or loses (order is CLOSED and the
// confirmation's CAS finds no PENDING row).
func (s *Service) closeOrderTx(ctx context.Context, r *http.Request, operatorID int64, operatorRole string, orderID int64, reason string) (closeOrderResponse, *apierr.APIError) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "fund_close_begin_failed: "+err.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		orderNo string
		userID  int64
		state   string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT order_no, user_id, state
		  FROM t_recharge_order
		 WHERE id = ?
		 FOR UPDATE`, orderID).Scan(&orderNo, &userID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		s.logWarn(ctx, "fund_close_unknown_order id="+itoa(orderID))
		return closeOrderResponse{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		s.logError(ctx, "fund_close_order_read_failed: "+err.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}

	switch state {
	case OrderStatePending:
		// The only state that may be voided.
	case OrderStateClosed:
		// Idempotent replay: already voided, nothing to write.
		view, apiErr := s.adminOrderByID(ctx, orderID)
		if apiErr != nil {
			return closeOrderResponse{}, apiErr
		}
		return closeOrderResponse{Order: view, Closed: true, Replayed: true}, nil
	default:
		// PAID / REFUNDING / REFUNDED / PARTIAL_REFUNDED: money has moved on
		// this order. Voiding it would orphan a credit that really exists, so
		// refuse and name the state.
		s.logWarn(ctx, "fund_close_state_refused order="+orderNo+" state="+state)
		return closeOrderResponse{}, confirmConflict("订单当前状态为 %s", state)
	}

	now := s.now()
	res, err := tx.ExecContext(ctx, `
		UPDATE t_recharge_order
		   SET state = 'CLOSED', updated_at = ?
		 WHERE id = ? AND state = 'PENDING'`, now, orderID)
	if err != nil {
		s.logError(ctx, "fund_close_state_cas_failed: "+err.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		s.logError(ctx, "fund_close_cas_rows_failed: "+err.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}
	if affected != 1 {
		// The row was PENDING under the lock, so a concurrent writer moved it
		// between the lock release and the CAS.
		var currentState string
		if err := tx.QueryRowContext(ctx,
			`SELECT state FROM t_recharge_order WHERE id = ?`, orderID).Scan(&currentState); err != nil {
			s.logError(ctx, "fund_close_cas_reread_failed: "+err.Error())
			return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
		}
		if currentState == OrderStateClosed {
			view, apiErr := s.adminOrderByID(ctx, orderID)
			if apiErr != nil {
				return closeOrderResponse{}, apiErr
			}
			return closeOrderResponse{Order: view, Closed: true, Replayed: true}, nil
		}
		return closeOrderResponse{}, confirmConflict("订单当前状态为 %s", currentState)
	}

	if aerr := s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   operatorID,
		OperatorRole: operatorRole,
		OperatorIP:   audit.IPBytes(r),
		Module:       "FUND",
		Action:       "ORDER_CLOSED_BY_OPERATOR",
		TargetType:   "t_recharge_order",
		TargetID:     itoa(orderID),
		Before:       mustJSON(map[string]any{"state": OrderStatePending}),
		After: mustJSON(map[string]any{
			"state":          OrderStateClosed,
			"previous_state": OrderStatePending,
			"order_no":       orderNo,
			"reason":         reason,
			"user_id":        userID,
		}),
		Reason: reason,
		Result: "OK",
	}); aerr != nil {
		// Voiding an order changes what the customer can do next; an
		// unauditable void is not worth keeping.
		s.logError(ctx, "fund_close_audit_failed: "+aerr.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}

	if err := tx.Commit(); err != nil {
		s.logError(ctx, "fund_close_commit_failed: "+err.Error())
		return closeOrderResponse{}, apierr.New(apierr.CodeInternalError)
	}

	view, apiErr := s.adminOrderByID(ctx, orderID)
	if apiErr != nil {
		return closeOrderResponse{}, apiErr
	}
	return closeOrderResponse{Order: view, Closed: true}, nil
}

// orderBonusDays resolves how long the bonus part of an order stays valid.
//
// It is shared by the channel callback and the operator confirmation: the
// same order must expire identically whichever path settled it. A plan's own
// bonus_valid_days wins, then billing.bonus_expire_days, then 90 days.
func (s *Service) orderBonusDays(ctx context.Context, tx *sql.Tx, planID sql.NullInt64, bonusMicro int64) (int64, *apierr.APIError) {
	if bonusMicro <= 0 {
		return 0, nil
	}
	days := int64(0)
	if planID.Valid {
		var planBonusDays sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT bonus_valid_days FROM t_plan WHERE id = ?`, planID.Int64).
			Scan(&planBonusDays); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logError(ctx, "fund_order_bonus_days_failed: "+err.Error())
			return 0, apierr.New(apierr.CodeInternalError)
		}
		if planBonusDays.Valid {
			days = planBonusDays.Int64
		}
	}
	return s.bonusExpireDays(ctx, days), nil
}

// actorOf returns the operator's id and role. Session middleware has already
// run; a missing session here is a wiring bug answered as 401.
func actorOf(r *http.Request) (int64, string, *apierr.APIError) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok || sess == nil {
		return 0, "", mw.SessionMissing()
	}
	return sess.UID, sess.Role, nil
}

// operatorPathID reads the {id} path value of an operator route. A malformed
// id addresses no row, so the honest answer is 404, not a body error.
func operatorPathID(r *http.Request) (int64, *apierr.APIError) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, apierr.New(apierr.CodeResourceNotFound)
	}
	return id, nil
}

// confirmConflict is the 409 for a confirmation the order cannot accept. The
// message always names the state or value that blocked it, never a bare
// "conflict".
func confirmConflict(format string, args ...any) *apierr.APIError {
	return apierr.Newf(apierr.CodeResourceStateConflict, fmt.Sprintf(format, args...))
}
