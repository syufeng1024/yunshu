package ops

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

const (
	userStatusNormal   = "NORMAL"
	userStatusDisabled = "DISABLED"
	userStatusFrozen   = "FROZEN_OVERDRAFT"

	disableSourceOpsManual = "OPS_MANUAL"
)

type userView struct {
	ID     int64  `json:"id"`
	UserNo string `json:"user_no"`
	// Username is the optional login name. The built-in super admin has neither
	// an email nor a phone, so without this field its row in the operator's user
	// list renders with no identifier at all.
	Username      string `json:"username,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	Nickname      string `json:"nickname"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	DisableSource string `json:"disable_source,omitempty"`
	// DisableReason is operator-only. The customer-facing account_disabled
	// body never reads it (T15 hard constraint 13).
	DisableReason string     `json:"disable_reason,omitempty"`
	DisabledAt    *time.Time `json:"disabled_at,omitempty"`
	DisabledBy    *int64     `json:"disabled_by,omitempty"`
	RiskScore     int64      `json:"risk_score"`
	PlanID        *int64     `json:"plan_id,omitempty"`
	PlanCode      string     `json:"plan_code,omitempty"`
	PlanName      string     `json:"plan_name,omitempty"`
	// Active API keys: status='ENABLED' AND deleted_at IS NULL. Filled in one
	// GROUP BY query per page, never per row.
	KeyCount  int64     `json:"key_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type creditBalanceView struct {
	Exists               bool       `json:"exists"`
	BalanceRechargeMicro int64      `json:"balance_recharge_micro"`
	BalanceBonusMicro    int64      `json:"balance_bonus_micro"`
	BalanceMicro         int64      `json:"balance_micro"`
	FrozenRefundMicro    int64      `json:"frozen_refund_micro"`
	OverdraftLimitMicro  int64      `json:"overdraft_limit_micro"`
	TotalRechargeMicro   int64      `json:"total_recharge_micro"`
	TotalBonusMicro      int64      `json:"total_bonus_micro"`
	TotalConsumedMicro   int64      `json:"total_consumed_micro"`
	LastSettledAt        *time.Time `json:"last_settled_at,omitempty"`
}

// balanceAdjustSummary is the operator's view of the most recent
// t_balance_adjust row for a user. It is a read-only projection: writes go
// through POST /api/admin/users/{id}/balance-adjust in the review package.
type balanceAdjustSummary struct {
	ID          int64      `json:"id"`
	Direction   string     `json:"direction"`
	AmountMicro int64      `json:"amount_micro"`
	Reason      string     `json:"reason"`
	State       string     `json:"state"`
	ApplicantID int64      `json:"applicant_id"`
	DecidedBy   *int64     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type userDetailView struct {
	User    userView          `json:"user"`
	Balance creditBalanceView `json:"balance"`
	// LatestBalanceAdjust is null when the user has never had one. The console
	// renders the state so an operator does not submit a second document while
	// one is still PENDING.
	LatestBalanceAdjust *balanceAdjustSummary `json:"latest_balance_adjust"`
}

type userListResponse struct {
	Items    []userView `json:"items"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Total    int64      `json:"total"`
}

type userDisableRequest struct {
	Reason string `json:"reason"`
}

const userSelectColumns = `
	u.id, u.user_no, u.username, u.email, u.phone, u.nickname, u.role, u.status,
	u.disable_source, u.disable_reason, u.disabled_at, u.disabled_by,
	u.risk_score, u.plan_id, COALESCE(p.code, ''), COALESCE(p.name, ''),
	u.created_at, u.updated_at`

// ListUsers implements GET /api/admin/users.
func (s *Service) ListUsers(w http.ResponseWriter, r *http.Request) {
	page, apiErr := mw.ParsePage(r, "created_at", "updated_at", "id", "risk_score", "user_no")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	q := r.URL.Query()
	where := []string{"u.deleted_at IS NULL"}
	args := make([]any, 0, 4)
	for _, f := range []struct {
		param string
		col   string
	}{
		{"email", "u.email"},
		{"phone", "u.phone"},
		{"username", "u.username"},
		{"user_no", "u.user_no"},
	} {
		raw := strings.TrimSpace(q.Get(f.param))
		if raw == "" {
			continue
		}
		if len([]rune(raw)) > 128 {
			mw.WriteError(w, r, mw.InvalidQueryField(f.param))
			return
		}
		where = append(where, f.col+" LIKE ? ESCAPE '!'")
		args = append(args, likePattern(raw))
	}
	if raw := strings.ToUpper(strings.TrimSpace(q.Get("status"))); raw != "" {
		if raw != userStatusNormal && raw != userStatusDisabled && raw != userStatusFrozen {
			mw.WriteError(w, r, mw.InvalidQueryField("status"))
			return
		}
		where = append(where, "u.status = ?")
		args = append(args, raw)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM t_user u WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(r.Context(), "ops_user_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	orderBy := "u.created_at"
	switch page.Sort {
	case "updated_at":
		orderBy = "u.updated_at"
	case "id":
		orderBy = "u.id"
	case "risk_score":
		orderBy = "u.risk_score"
	case "user_no":
		orderBy = "u.user_no"
	}
	dir := orderDirection(page.Order)
	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT `+userSelectColumns+`
		  FROM t_user u LEFT JOIN t_plan p ON p.id = u.plan_id
		 WHERE `+whereSQL+`
		 ORDER BY `+orderBy+` `+dir+`, u.id `+dir+` LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.logError(r.Context(), "ops_user_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]userView, 0, page.Size)
	for rows.Next() {
		v, err := scanUser(rows)
		if err != nil {
			s.logError(r.Context(), "ops_user_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		s.logError(r.Context(), "ops_user_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	userPtrs := make([]*userView, len(items))
	for i := range items {
		userPtrs[i] = &items[i]
	}
	if err := s.fillKeyCounts(r.Context(), s.DB, userPtrs); err != nil {
		s.logError(r.Context(), "ops_user_key_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, userListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// GetUser implements GET /api/admin/users/{id}. Balance comes from
// t_credit_account; a user with no account yet reports exists=false with zero
// values rather than a 500.
func (s *Service) GetUser(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	user, apiErr := s.loadUser(r.Context(), s.DB, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	userPtrs := []*userView{&user}
	if err := s.fillKeyCounts(r.Context(), s.DB, userPtrs); err != nil {
		s.logError(r.Context(), "ops_user_key_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	balance, err := loadBalance(r.Context(), s.DB, id)
	if err != nil {
		s.logError(r.Context(), "ops_user_balance_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	latest, err := loadLatestBalanceAdjust(r.Context(), s.DB, id)
	if err != nil {
		s.logError(r.Context(), "ops_user_latest_adjust_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, userDetailView{
		User: user, Balance: balance, LatestBalanceAdjust: latest,
	})
}

// DisableUser implements POST /api/admin/users/{id}/disable.
//
// The reason is stored and returned only by operator endpoints. The user-facing
// account_disabled body is a fixed generic message (hard constraint 13).
func (s *Service) DisableUser(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req userDisableRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len([]rune(reason)) > 255 {
		mw.WriteError(w, r, mw.InvalidField("reason"))
		return
	}
	uid, _, apiErr := actor(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if uid == id {
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"不能禁用当前登录账号"))
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_user_disable_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	before, apiErr := s.loadUserTx(ctx, tx, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET status = ?, disable_source = ?, disable_reason = ?,
		       disabled_at = ?, disabled_by = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		userStatusDisabled, disableSourceOpsManual, reason, now, uid, now, id); err != nil {
		s.logError(ctx, "ops_user_disable_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	after, apiErr := s.loadUserTx(ctx, tx, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	userPtrs := []*userView{&after}
	if err := s.fillKeyCounts(ctx, tx, userPtrs); err != nil {
		s.logError(ctx, "ops_user_key_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.insertAudit(ctx, tx, r, "USER", "DISABLE", "t_user", id,
		userAuditView(before), userAuditView(after), reason); err != nil {
		s.logError(ctx, "ops_user_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_user_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true
	balance, err := loadBalance(ctx, s.DB, id)
	if err != nil {
		s.logError(ctx, "ops_user_balance_failed: "+err.Error())
		balance = creditBalanceView{}
	}
	_ = mw.WriteJSON(w, http.StatusOK, userDetailView{User: after, Balance: balance})
}

// EnableUser implements POST /api/admin/users/{id}/enable.
func (s *Service) EnableUser(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_user_enable_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	before, apiErr := s.loadUserTx(ctx, tx, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if before.Status == userStatusFrozen {
		// FROZEN_OVERDRAFT is a billing state, not an operator ban. Clearing
		// it here would let a customer with an unpaid overdraft call again.
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"账号处于透支冻结状态，需充值后由账务自动恢复，运营启用无效"))
		return
	}
	if before.Status != userStatusNormal {
		if _, err := tx.ExecContext(ctx, `
			UPDATE t_user
			   SET status = ?, disable_source = NULL, disable_reason = NULL,
			       disabled_at = NULL, disabled_by = NULL, updated_at = ?
			 WHERE id = ? AND deleted_at IS NULL`,
			userStatusNormal, now, id); err != nil {
			s.logError(ctx, "ops_user_enable_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	after, apiErr := s.loadUserTx(ctx, tx, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	userPtrs := []*userView{&after}
	if err := s.fillKeyCounts(ctx, tx, userPtrs); err != nil {
		s.logError(ctx, "ops_user_key_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.insertAudit(ctx, tx, r, "USER", "ENABLE", "t_user", id,
		userAuditView(before), userAuditView(after), ""); err != nil {
		s.logError(ctx, "ops_user_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_user_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true
	balance, err := loadBalance(ctx, s.DB, id)
	if err != nil {
		s.logError(ctx, "ops_user_balance_failed: "+err.Error())
		balance = creditBalanceView{}
	}
	_ = mw.WriteJSON(w, http.StatusOK, userDetailView{User: after, Balance: balance})
}

// ---------------------------------------------------------------------------
// storage
// ---------------------------------------------------------------------------

func scanUser(row rowScanner) (userView, error) {
	var (
		v                      userView
		username, email, phone sql.NullString
		disableSource          sql.NullString
		disableReason          sql.NullString
		disabledAt             sql.NullTime
		disabledBy             sql.NullInt64
		planID                 sql.NullInt64
		createdAt              time.Time
		updatedAt              time.Time
	)
	if err := row.Scan(&v.ID, &v.UserNo, &username, &email, &phone, &v.Nickname, &v.Role,
		&v.Status, &disableSource, &disableReason, &disabledAt, &disabledBy,
		&v.RiskScore, &planID, &v.PlanCode, &v.PlanName, &createdAt, &updatedAt); err != nil {
		return userView{}, err
	}
	v.Username = username.String
	v.Email = email.String
	v.Phone = phone.String
	v.DisableSource = disableSource.String
	v.DisableReason = disableReason.String
	v.DisabledAt = nullableTime(disabledAt)
	v.DisabledBy = nullableInt64Ptr(disabledBy)
	v.PlanID = nullableInt64Ptr(planID)
	v.CreatedAt = createdAt.UTC()
	v.UpdatedAt = updatedAt.UTC()
	return v, nil
}

func (s *Service) loadUser(ctx context.Context, q priceDB, id int64) (userView, *apierr.APIError) {
	v, err := scanUser(q.QueryRowContext(ctx, `
		SELECT `+userSelectColumns+`
		  FROM t_user u LEFT JOIN t_plan p ON p.id = u.plan_id
		 WHERE u.id = ? AND u.deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return userView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		return userView{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return v, nil
}

func (s *Service) loadUserTx(ctx context.Context, tx *sql.Tx, id int64, forUpdate bool) (userView, *apierr.APIError) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	v, err := scanUser(tx.QueryRowContext(ctx, `
		SELECT `+userSelectColumns+`
		  FROM t_user u LEFT JOIN t_plan p ON p.id = u.plan_id
		 WHERE u.id = ? AND u.deleted_at IS NULL`+suffix, id))
	if errors.Is(err, sql.ErrNoRows) {
		return userView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		return userView{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return v, nil
}

// loadLatestBalanceAdjust reads the newest document through idx_ba_user_created
// (user_id, created_at). It returns nil, not a zero row, when none exists.
func loadLatestBalanceAdjust(ctx context.Context, q priceDB, userID int64) (*balanceAdjustSummary, error) {
	var (
		v         balanceAdjustSummary
		decidedBy sql.NullInt64
		decidedAt sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, direction, amount_micro, reason, state, applicant_id,
		       decided_by, decided_at, created_at
		  FROM t_balance_adjust
		 WHERE user_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, userID).
		Scan(&v.ID, &v.Direction, &v.AmountMicro, &v.Reason, &v.State,
			&v.ApplicantID, &decidedBy, &decidedAt, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if decidedBy.Valid {
		id := decidedBy.Int64
		v.DecidedBy = &id
	}
	v.DecidedAt = nullableTime(decidedAt)
	v.CreatedAt = v.CreatedAt.UTC()
	return &v, nil
}

func loadBalance(ctx context.Context, q priceDB, userID int64) (creditBalanceView, error) {
	var (
		v         creditBalanceView
		settledAt sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT balance_recharge_micro, balance_bonus_micro, balance_micro,
		       frozen_refund_micro, overdraft_limit_micro,
		       total_recharge_micro, total_bonus_micro, total_consumed_micro,
		       last_settled_at
		  FROM t_credit_account WHERE user_id = ?`, userID).Scan(
		&v.BalanceRechargeMicro, &v.BalanceBonusMicro, &v.BalanceMicro,
		&v.FrozenRefundMicro, &v.OverdraftLimitMicro,
		&v.TotalRechargeMicro, &v.TotalBonusMicro, &v.TotalConsumedMicro,
		&settledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return creditBalanceView{Exists: false}, nil
	}
	if err != nil {
		return creditBalanceView{}, err
	}
	v.Exists = true
	v.LastSettledAt = nullableTime(settledAt)
	return v, nil
}

type keyCountQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// fillKeyCounts runs one GROUP BY for the whole page. idx_key_user_status
// (user_id, status) serves the IN list; deleted_at is filtered on the fetched
// rows. Missing users simply keep the zero value.
func (s *Service) fillKeyCounts(ctx context.Context, q keyCountQuerier, users []*userView) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(users))
	args := make([]any, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
		args = append(args, user.ID)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT user_id, COUNT(*)
		  FROM t_api_key
		 WHERE user_id IN (`+placeholders(len(ids))+`)
		   AND status = 'ENABLED' AND deleted_at IS NULL
		 GROUP BY user_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make(map[int64]int64, len(ids))
	for rows.Next() {
		var (
			userID int64
			count  int64
		)
		if err := rows.Scan(&userID, &count); err != nil {
			return err
		}
		counts[userID] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, user := range users {
		user.KeyCount = counts[user.ID]
	}
	return nil
}

func userAuditView(v userView) map[string]any {
	return map[string]any{
		"user_no":        v.UserNo,
		"status":         v.Status,
		"disable_source": v.DisableSource,
		"disable_reason": v.DisableReason,
		"disabled_at":    v.DisabledAt,
		"disabled_by":    v.DisabledBy,
		"risk_score":     v.RiskScore,
		"plan_id":        v.PlanID,
	}
}
