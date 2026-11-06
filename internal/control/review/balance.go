package review

// 余额调整（T24 段 2）。
//
// 三步链路全部落在这一处，避免“提交在 ops、落账在 fund、状态在 review”三套
// 真相：
//
//	POST /api/admin/users/{id}/balance-adjust
//	  → t_balance_adjust(PENDING) + t_review_task(BALANCE_ADJUST) + 审计
//	POST /api/admin/reviews/{id}/approve
//	  → t_balance_adjust 与 t_review_task 同时 PENDING→APPROVED，并经
//	    ledger.AdjustTx 追加分录；三者在同一事务
//	POST /api/admin/reviews/{id}/reject
//	  → 两张表同时 PENDING→REJECTED，不产生任何资金变动
//
// 审批方法没有走 approve.go 的“先副作用、后 claim”通用路径：资金单据要求
// 状态与分录同一事务，否则崩溃窗口里要么多一笔钱、要么少一张已通过的单据。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/redisx"
)

// t_balance_adjust.direction / state — exactly the 0025 DDL values.
const (
	BalanceDirectionIncrease = "INCREASE"
	BalanceDirectionDecrease = "DECREASE"

	BalanceStatePending  = "PENDING"
	BalanceStateApproved = "APPROVED"
	BalanceStateRejected = "REJECTED"
)

const (
	// balanceReasonMinRunes: a reason shorter than 5 characters cannot explain
	// why money moved, and the reviewer has nothing to decide on.
	balanceReasonMinRunes = 5
	// balanceReasonMaxRunes is t_balance_adjust.reason varchar(255).
	balanceReasonMaxRunes = 255
	// balanceAdjustMaxMicro caps one manual adjustment at 1,000,000 credits
	// (1 credit = 10,000 micro). The cap is a typo guard, not a product limit:
	// an operator who really needs more submits several documents, each with
	// its own reason and reviewer decision.
	balanceAdjustMaxMicro int64 = 1_000_000 * 10_000
)

type balanceAdjustRequest struct {
	Direction   string `json:"direction"`
	AmountMicro int64  `json:"amount_micro"`
	Reason      string `json:"reason"`
}

type balanceAdjustView struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	Direction   string     `json:"direction"`
	AmountMicro int64      `json:"amount_micro"`
	Reason      string     `json:"reason"`
	State       string     `json:"state"`
	ApplicantID int64      `json:"applicant_id"`
	DecidedBy   *int64     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type balanceAdjustRow struct {
	ID          int64
	UserID      int64
	Direction   string
	AmountMicro int64
	Reason      string
	State       string
	ApplicantID int64
	DecidedBy   *int64
	DecidedAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (r balanceAdjustRow) view() balanceAdjustView {
	return balanceAdjustView{
		ID:          r.ID,
		UserID:      r.UserID,
		Direction:   r.Direction,
		AmountMicro: r.AmountMicro,
		Reason:      r.Reason,
		State:       r.State,
		ApplicantID: r.ApplicantID,
		DecidedBy:   r.DecidedBy,
		DecidedAt:   r.DecidedAt,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

type balanceAdjustSubmitResponse struct {
	BalanceAdjust balanceAdjustView `json:"balance_adjust"`
	ReviewTaskID  int64             `json:"review_task_id"`
}

type balanceAdjustDetailResponse struct {
	BalanceAdjust balanceAdjustView `json:"balance_adjust"`
}

// SubmitBalanceAdjust implements POST /api/admin/users/{id}/balance-adjust.
//
// The applicant is the session operator, never a body field: a request that
// could name someone else as applicant would make the self-audit check
// bypassable. The generated column uk_ba_user_pending refuses a second pending
// document for the same user without a read-then-insert race.
func (s *Service) SubmitBalanceAdjust(w http.ResponseWriter, r *http.Request) {
	userID, apiErr := pathID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req balanceAdjustRequest
	if apiErr := decodeOptionalJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	direction := strings.ToUpper(strings.TrimSpace(req.Direction))
	switch direction {
	case BalanceDirectionIncrease, BalanceDirectionDecrease:
	default:
		mw.WriteError(w, r, mw.InvalidField("direction"))
		return
	}
	if req.AmountMicro <= 0 || req.AmountMicro > balanceAdjustMaxMicro {
		mw.WriteError(w, r, mw.InvalidField("amount_micro"))
		return
	}
	reason := normalizeBalanceReason(req.Reason)
	if n := len([]rune(reason)); n < balanceReasonMinRunes || n > balanceReasonMaxRunes {
		mw.WriteError(w, r, mw.InvalidField("reason"))
		return
	}

	operatorID, operatorRole, apiErr := actor(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "balance_submit_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The user and the credit account are checked under the same transaction
	// that inserts the document. There is no foreign key on this schema, so a
	// check outside the transaction would leave a window where the account is
	// deleted between the check and the approval.
	if apiErr := balanceTargetExists(ctx, tx, userID); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_balance_adjust
			(user_id, direction, amount_micro, reason, state, applicant_id, created_at, updated_at)
		VALUES (?,?,?,?, 'PENDING', ?,?,?)`,
		userID, direction, req.AmountMicro, reason, operatorID, now, now)
	if err != nil {
		if isDuplicateBalanceKey(err) {
			mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
				"用户 #%d 已有待审核的余额调整单", userID).
				WithHint("请等待该单据审批完成或驳回后再提交新单"))
			return
		}
		s.logError(ctx, "balance_submit_insert_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	adjustID, err := res.LastInsertId()
	if err != nil {
		s.logError(ctx, "balance_submit_id_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	taskRes, err := tx.ExecContext(ctx, `
		INSERT INTO t_review_task (biz_type, biz_id, applicant_id, state, created_at)
		VALUES (?,?,?, 'PENDING', ?)`, BizBalanceAdjust, adjustID, operatorID, now)
	if err != nil {
		s.logError(ctx, "balance_submit_review_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	taskID, err := taskRes.LastInsertId()
	if err != nil {
		s.logError(ctx, "balance_submit_review_id_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	row := balanceAdjustRow{
		ID: adjustID, UserID: userID, Direction: direction,
		AmountMicro: req.AmountMicro, Reason: reason, State: BalanceStatePending,
		ApplicantID: operatorID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.insertBalanceAudit(ctx, tx, r, "USER", "SUBMIT", operatorID, operatorRole,
		adjustID, nil, balanceAuditView(row), reason); err != nil {
		s.logError(ctx, "balance_submit_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "balance_submit_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true
	_ = mw.WriteJSON(w, http.StatusCreated, balanceAdjustSubmitResponse{
		BalanceAdjust: row.view(),
		ReviewTaskID:  taskID,
	})
}

// approveBalanceAdjust is called from Approve after the queue row has passed
// the self-audit and PENDING checks. Everything that changes money happens in
// one transaction: business state CAS, ledger entry, queue state CAS and audit.
func (s *Service) approveBalanceAdjust(w http.ResponseWriter, r *http.Request,
	task reviewTask, operatorID int64, operatorRole, note string) {
	ctx := r.Context()
	now := s.now()
	if s.Ledger == nil {
		s.logError(ctx, "balance_approve_ledger_missing")
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "balance_approve_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadBalanceAdjustTx(ctx, tx, task.BizID, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if before.ApplicantID != task.ApplicantID {
		s.logError(ctx, fmt.Sprintf("balance_approve_applicant_mismatch adjust=%d task_applicant=%d row_applicant=%d",
			before.ID, task.ApplicantID, before.ApplicantID))
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if before.State != BalanceStatePending {
		mw.WriteError(w, r, stateConflict("余额调整单当前状态为 %s，不能再次审批", before.State))
		return
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE t_balance_adjust
		   SET state = 'APPROVED', decided_by = ?, decided_at = ?, updated_at = ?
		 WHERE id = ? AND state = 'PENDING'`,
		operatorID, now, now, before.ID)
	if err != nil {
		s.logError(ctx, "balance_approve_state_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		mw.WriteError(w, r, stateConflict("余额调整单已被处理，请刷新后重试"))
		return
	}

	adjustReason := before.Reason
	adjust, err := s.Ledger.AdjustTx(ctx, tx, ledger.AdjustParams{
		UserID:      before.UserID,
		AdjustID:    before.ID,
		Direction:   before.Direction,
		AmountMicro: before.AmountMicro,
		OperatorID:  operatorID,
		Reason:      adjustReason,
	}, now)
	if err != nil {
		mw.WriteError(w, r, s.balanceLedgerError(ctx, err))
		return
	}

	taskRes, err := tx.ExecContext(ctx, `
		UPDATE t_review_task
		   SET state = 'APPROVED', decision_note = ?, decided_at = ?
		 WHERE id = ? AND state = 'PENDING'`,
		noteArg(note), now, task.ID)
	if err != nil {
		s.logError(ctx, "balance_approve_review_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if affected, _ := taskRes.RowsAffected(); affected != 1 {
		mw.WriteError(w, r, stateConflict("审核任务已被处理，请刷新后重试"))
		return
	}

	after := before
	after.State = BalanceStateApproved
	after.DecidedBy = &operatorID
	after.DecidedAt = &now
	after.UpdatedAt = now
	afterMap := balanceAuditView(after)
	afterMap["balance_after_micro"] = adjust.BalanceAfterMicro
	afterMap["entry_count"] = len(adjust.EntryIDs)
	if err := s.insertBalanceAudit(ctx, tx, r, "REVIEW", "APPROVE", operatorID, operatorRole,
		before.ID, balanceAuditView(before), afterMap, note); err != nil {
		s.logError(ctx, "balance_approve_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "balance_approve_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	// The ledger is already correct; K26 is only the gateway's 60s view of it.
	// Without this DEL an approved adjustment stays invisible to admission for
	// up to a minute — a funded user is rejected, or an increased account keeps
	// spending against the pre-approval number. Post-commit and best-effort: a
	// Redis failure must never turn a committed money change into a 500.
	s.invalidateBalanceMirror(ctx, before.UserID)

	task.State = StateApproved
	task.DecidedAt = &now
	if note != "" {
		task.DecisionNote = &note
	}
	item, apiErr := s.enrichOne(ctx, task)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, reviewDetailResponse{Review: item})
}

// balanceRedis returns the Redis client K26 lives in.
//
// The client is not a field of this Service: production already hands the one
// shared *redisx.Client to fund.Service (server.go wires review.New(..., funds)
// and fund.New(db, redis, ...)), and reaching it through Fund keeps a
// hand-wired Service honest — no Fund means no mirror to invalidate, instead of
// a silently nil client that would skip the DEL in production.
func (s *Service) balanceRedis() *redisx.Client {
	if s == nil || s.Fund == nil {
		return nil
	}
	return s.Fund.Redis
}

// invalidateBalanceMirror deletes K26 (bal:{uid}) after a committed balance
// change so the gateway reloads the balance on its next admission instead of
// trusting a pre-approval value. A failed DEL is a WARN, not an error return:
// the ledger has already committed and the mirror expires on its own 60s TTL,
// so the worst case is one minute of staleness, not a lost adjustment.
//
// The DEL runs on a detached context with its own short budget. The request
// context is the wrong owner for this: the money has already moved, so a client
// that disconnects (or a proxy that drops the connection) between commit and
// purge must not be able to cancel the invalidation and leave a stale mirror for
// up to a minute. This is a background state change, and its bookkeeping side
// effect outlives the caller.
func (s *Service) invalidateBalanceMirror(ctx context.Context, userID int64) {
	rdb := s.balanceRedis()
	if rdb == nil {
		return
	}
	cctx, cancel := rdb.CommandContext(context.WithoutCancel(ctx))
	defer cancel()
	if err := rdb.Raw().Del(cctx, redisx.BalanceKey(userID)).Err(); err != nil {
		s.logWarn(ctx, "review_balance_mirror_invalidate_failed: "+err.Error())
	}
}

// rejectBalanceAdjust is the non-money path: both rows move together and no
// ledger entry is written.
func (s *Service) rejectBalanceAdjust(w http.ResponseWriter, r *http.Request,
	task reviewTask, operatorID int64, operatorRole, reason string) {
	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "balance_reject_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadBalanceAdjustTx(ctx, tx, task.BizID, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if before.State != BalanceStatePending {
		mw.WriteError(w, r, stateConflict("余额调整单当前状态为 %s，不能再次审批", before.State))
		return
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE t_balance_adjust
		   SET state = 'REJECTED', decided_by = ?, decided_at = ?, updated_at = ?
		 WHERE id = ? AND state = 'PENDING'`,
		operatorID, now, now, before.ID)
	if err != nil {
		s.logError(ctx, "balance_reject_state_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		mw.WriteError(w, r, stateConflict("余额调整单已被处理，请刷新后重试"))
		return
	}

	taskRes, err := tx.ExecContext(ctx, `
		UPDATE t_review_task
		   SET state = 'REJECTED', decision_note = ?, decided_at = ?
		 WHERE id = ? AND state = 'PENDING'`,
		reason, now, task.ID)
	if err != nil {
		s.logError(ctx, "balance_reject_review_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if affected, _ := taskRes.RowsAffected(); affected != 1 {
		mw.WriteError(w, r, stateConflict("审核任务已被处理，请刷新后重试"))
		return
	}

	after := before
	after.State = BalanceStateRejected
	after.DecidedBy = &operatorID
	after.DecidedAt = &now
	after.UpdatedAt = now
	if err := s.insertBalanceAudit(ctx, tx, r, "REVIEW", "REJECT", operatorID, operatorRole,
		before.ID, balanceAuditView(before), balanceAuditView(after), reason); err != nil {
		s.logError(ctx, "balance_reject_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "balance_reject_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	task.State = StateRejected
	task.DecidedAt = &now
	task.DecisionNote = &reason
	item, apiErr := s.enrichOne(ctx, task)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, reviewDetailResponse{Review: item})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func normalizeBalanceReason(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func balanceTargetExists(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID int64) *apierr.APIError {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM t_user WHERE id = ? AND deleted_at IS NULL`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	err = q.QueryRowContext(ctx,
		`SELECT user_id FROM t_credit_account WHERE user_id = ?`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return apierr.Newf(apierr.CodeResourceStateConflict,
			"用户 #%d 尚无积分账户，无法提交余额调整", userID).
			WithHint("积分账户在注册时创建；缺失账户需先排查数据完整性")
	}
	if err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return nil
}

func isDuplicateBalanceKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func noteArg(note string) any {
	if note == "" {
		return nil
	}
	return note
}

func (s *Service) loadBalanceAdjustTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id int64, forUpdate bool) (balanceAdjustRow, *apierr.APIError) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	var (
		row       balanceAdjustRow
		decidedBy sql.NullInt64
		decidedAt sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, user_id, direction, amount_micro, reason, state, applicant_id,
		       decided_by, decided_at, created_at, updated_at
		  FROM t_balance_adjust WHERE id = ?`+suffix, id).
		Scan(&row.ID, &row.UserID, &row.Direction, &row.AmountMicro, &row.Reason,
			&row.State, &row.ApplicantID, &decidedBy, &decidedAt,
			&row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return balanceAdjustRow{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		return balanceAdjustRow{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	if decidedBy.Valid {
		v := decidedBy.Int64
		row.DecidedBy = &v
	}
	if decidedAt.Valid {
		v := decidedAt.Time.UTC()
		row.DecidedAt = &v
	}
	row.CreatedAt = row.CreatedAt.UTC()
	row.UpdatedAt = row.UpdatedAt.UTC()
	return row, nil
}

func (s *Service) insertBalanceAudit(ctx context.Context, tx *sql.Tx, r *http.Request,
	module, action string, operatorID int64, operatorRole string, adjustID int64,
	before, after map[string]any, reason string) error {
	beforeJSON, afterJSON, err := audit.MarshalBeforeAfter(before, after)
	if err != nil {
		return err
	}
	return s.Audit.Insert(ctx, tx, audit.Entry{
		OperatorID:   operatorID,
		OperatorRole: operatorRole,
		OperatorIP:   audit.IPBytes(r),
		Module:       module,
		Action:       action,
		TargetType:   "t_balance_adjust",
		TargetID:     fmt.Sprintf("%d", adjustID),
		Before:       beforeJSON,
		After:        afterJSON,
		Reason:       truncateRunes(reason, 255),
		Result:       "OK",
	})
}

func balanceAuditView(r balanceAdjustRow) map[string]any {
	return map[string]any{
		"user_id":      r.UserID,
		"direction":    r.Direction,
		"amount_micro": r.AmountMicro,
		"reason":       r.Reason,
		"state":        r.State,
		"applicant_id": r.ApplicantID,
		"decided_by":   r.DecidedBy,
		"decided_at":   r.DecidedAt,
	}
}

// balanceLedgerError translates the money kernel's sentinels. A shortage or a
// missing account is a conflict the operator can act on (top up, re-check the
// account); anything else is internal and never echoes SQL.
func (s *Service) balanceLedgerError(ctx context.Context, err error) *apierr.APIError {
	switch {
	case errors.Is(err, ledger.ErrNoAccount):
		return stateConflict("用户尚无积分账户，无法落账")
	case errors.Is(err, ledger.ErrInsufficientCredits),
		errors.Is(err, ledger.ErrInsufficientBatches),
		errors.Is(err, ledger.ErrBelowOverdraft):
		return stateConflict("余额不足，无法执行减量调整：%s", err.Error())
	case errors.Is(err, ledger.ErrDuplicateIdem):
		return stateConflict("该调整单已产生资金分录，不能重复落账")
	case errors.Is(err, ledger.ErrInvalidParam):
		return stateConflict("资金域拒绝该操作：%s", err.Error())
	}
	s.logError(ctx, "balance_ledger_failed: "+err.Error())
	return apierr.New(apierr.CodeInternalError)
}
