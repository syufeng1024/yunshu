package ops

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/mw"
)

// The four role names of the PRD §6.3 enum, spelled exactly as t_user.role
// stores them. They are duplicated here instead of imported from mw because mw
// owns the *permission* matrix, not the stored vocabulary, and a role rename
// must break the compiler in both packages rather than silently fall through
// mw.PermissionsFor's "unknown role has no permissions" branch.
const (
	userRolePersonal   = "PERSONAL"
	userRoleOps        = "OPS"
	userRoleFinance    = "FINANCE"
	userRoleSuperAdmin = "SUPER_ADMIN"
)

// provisionableRoles are the roles POST /api/admin/users may create.
// PERSONAL is deliberately absent: see CreateOperatorUser.
var provisionableRoles = map[string]bool{
	userRoleOps:        true,
	userRoleFinance:    true,
	userRoleSuperAdmin: true,
}

// assignableRoles are the roles POST /api/admin/users/{id}/role may write. It is
// the full enum: a customer may be promoted to an operator and an operator may
// be demoted back to a customer.
var assignableRoles = map[string]bool{
	userRolePersonal:   true,
	userRoleOps:        true,
	userRoleFinance:    true,
	userRoleSuperAdmin: true,
}

type operatorCreateRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	Nickname   string `json:"nickname"`
	Role       string `json:"role"`
}

type userRoleChangeRequest struct {
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

// CreateOperatorUser implements POST /api/admin/users.
//
// The route is gated by admin.all (SUPER_ADMIN only), never by user.write. OPS
// holds user.write, so gating here would let an operations account mint an
// account — including a SUPER_ADMIN — and then sign in as it: a self-service
// privilege escalation with extra steps.
//
// PERSONAL is refused with 400 (invalid_request_body, message naming role), and
// that refusal is the point of the endpoint existing at all. A customer account
// must be created by public registration, which carries the trial grant, the
// device fingerprint and the agreement-version consent that this operator form
// does not collect. Creating one here would produce a customer account that
// skipped all three.
//
// Everything credential-shaped is reused from internal/control/auth rather than
// re-implemented: NormalizeIdentifier, ValidatePassword and the bcrypt cost are
// the registration rules, so the operator path cannot quietly end up weaker
// than the public form.
//
// The password is hashed before the transaction opens and appears in exactly
// one place: the INSERT. It is never logged, never audited and never echoed —
// the response is the same user view GET /api/admin/users/{id} returns.
func (s *Service) CreateOperatorUser(w http.ResponseWriter, r *http.Request) {
	var req operatorCreateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ident, err := auth.NormalizeIdentifier(req.Identifier)
	if err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier"))
		return
	}
	// A username is refused here: an account whose only identifier is a username
	// has no address to receive a recovery code, so forgetting its password
	// would leave it permanently unreachable. Opening it would mean minting an
	// account that cannot rescue itself. The one deliberate exception is the
	// built-in SUPER_ADMIN, created by the controld startup bootstrap
	// (internal/control/bootstrap) which is not reachable over HTTP at all.
	if err := auth.RequireEmailOrPhone(ident); err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier").
			WithHint("运营建号只接受邮箱或手机号，用户名仅供内置超级管理员使用"))
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		mw.WriteError(w, r, mw.InvalidField("password"))
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if len([]rune(nickname)) > 64 {
		mw.WriteError(w, r, mw.InvalidField("nickname"))
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == userRolePersonal {
		// Not a typo in the caller's body that a different value would fix in
		// the same form: the account kind itself is wrong for this endpoint
		// (see the doc comment above), so 400 with the field name is the
		// honest answer — the operator has to use the public registration
		// flow instead.
		mw.WriteError(w, r, mw.InvalidField("role"))
		return
	}
	if !provisionableRoles[role] {
		mw.WriteError(w, r, mw.InvalidField("role"))
		return
	}

	// A phone account must carry t_user.phone_hmac, which is where the unique
	// key lives (t_user.phone has none since 0011). Resolve it before the bcrypt
	// hash so a missing key costs the operator nothing, and answer 503 rather
	// than writing a phone row the unique key cannot see — a NULL hmac means
	// "this number is not claimed", so the next registration of the same number
	// would silently create a second account.
	phoneMAC := ""
	if ident.Kind == auth.KindPhone {
		mac, macErr := auth.PhoneHMAC(s.PhoneHMACKey, ident.Value)
		if macErr != nil {
			// WARN, not ERROR: logError stamps status=500 on the record, and a
			// deliberate 503 config_unavailable would then read as a server
			// fault on every dashboard. The key is validated once at startup
			// (prod logs it at ERROR there), so this line is a repeat, not a
			// new incident.
			s.logWarn(r.Context(), "ops_create_operator_phone_hmac_unavailable env="+
				config.EnvPhoneHMACKey+" status=503 config_unavailable")
			mw.WriteError(w, r, apierr.New(apierr.CodeConfigUnavailable).
				WithHint("手机号建号暂不可用，请使用邮箱"))
			return
		}
		phoneMAC = mac
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), auth.BcryptCost)
	if err != nil {
		s.logError(r.Context(), "ops_create_operator_hash_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_create_operator_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	uid, err := auth.InsertUserRowTx(ctx, tx, ident, phoneMAC, nickname, "", string(hash), role, now)
	if err != nil {
		if auth.IsDuplicate(err) {
			// Same code registration answers: the identifier is taken, the
			// body was well-formed. Erratum E-21 added account_exists for it.
			//
			// WARN (logError would stamp status=500 on a deliberate 409) and the
			// index name only: "phone already used" and "e-mail already used"
			// are the same response and two different operator actions, while
			// the driver's "Duplicate entry '…'" value is the hmac and must not
			// reach a log.
			s.logWarn(ctx, "ops_create_operator_dup_identifier: "+duplicateKeyName(err))
			mw.WriteError(w, r, apierr.New(apierr.CodeAccountExists))
			return
		}
		s.logError(ctx, "ops_create_operator_insert_user_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	// The credit account is created empty, exactly like registration. Without
	// it SUPER_ADMIN's own /api/console/overview answers exists=false and the
	// balance block reads as broken; every other account has the row.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
		uid, now, now,
	); err != nil {
		s.logError(ctx, "ops_create_operator_insert_account_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	created, apiErr := s.loadUserTx(ctx, tx, uid, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	// The identifier is masked with the registration rule: the audit trail has
	// to let an operator recognise the account without becoming a PII dump.
	// CREATE_OPERATOR rather than the CREATE that registration writes, because
	// the audit reader has to be able to tell "a customer signed up" from "an
	// operator opened an account" — the latter is what changes who can reach
	// /admin.
	after := map[string]string{
		"user_no":           created.UserNo,
		"identifier":        auth.MaskIdentifier(ident.Kind, ident.Value),
		"identifier_kind":   ident.Kind,
		"role":              created.Role,
		"status":            created.Status,
		"agreement_version": "",
	}
	if err := s.insertAudit(ctx, tx, r, "USER", "CREATE_OPERATOR", "t_user", uid, nil, after, ""); err != nil {
		s.logError(ctx, "ops_create_operator_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_create_operator_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	balance, err := loadBalance(ctx, s.DB, uid)
	if err != nil {
		// The account row was written in the same transaction as the user, so
		// a read failure here is a pool problem, not a missing row. Reporting
		// the created account matters more than the balance projection.
		s.logError(ctx, "ops_create_operator_balance_failed: "+err.Error())
		balance = creditBalanceView{}
	}
	_ = mw.WriteJSON(w, http.StatusCreated, userDetailView{User: created, Balance: balance})
}

// ChangeUserRole implements POST /api/admin/users/{id}/role.
//
// Two invariants hold under concurrency, and both are enforced inside one
// transaction rather than by a read-then-write on the handler's own:
//
//  1. an operator cannot change their own role. Guarded before the transaction
//     (id == session uid → 403): the row lock cannot express "not yourself",
//     and doing it first keeps self-escalation from ever reaching the UPDATE.
//     Both directions are dangerous — promoting yourself is an escalation,
//     demoting yourself locks the only account that could undo it.
//
//  2. the last SUPER_ADMIN cannot be demoted. Enforced by lockSuperAdminSet:
//     every current SUPER_ADMIN row is locked in ascending id order and
//     re-read under the lock, so two operators demoting each other cannot both
//     observe "there are two of us" and both succeed. See that function for
//     why the obvious `SELECT COUNT(*) ... FOR UPDATE` is not used here.
//
// The write itself is still conditional (WHERE role = <the role read under the
// lock>): if anything slipped past the locks, RowsAffected is 0 and the caller
// gets 409 instead of a silent overwrite.
//
// Demoting an operator does not revoke their sessions, and does not need to:
// mw.Session loads t_user.role on every request, so the next call from the
// demoted account is already evaluated against the new role.
func (s *Service) ChangeUserRole(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req userRoleChangeRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	role := strings.TrimSpace(req.Role)
	if !assignableRoles[role] {
		mw.WriteError(w, r, mw.InvalidField("role"))
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
		// 403, not 409: this is a capability the operator's role does not have
		// (no role may rewrite its own row), not a state the target happens to
		// be in.
		mw.WriteError(w, r, apierr.New(apierr.CodePermissionDenied))
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_user_role_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Lock the super-admin set first, then the target row. The order is fixed
	// for every role change, so two concurrent changes serialise instead of
	// deadlocking on each other's target.
	lockedSuperAdmins, err := lockSuperAdminSet(ctx, tx)
	if err != nil {
		s.logError(ctx, "ops_user_role_lock_super_admins_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	before, apiErr := s.loadUserTx(ctx, tx, id, true)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if before.Role == role {
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"该账号当前已是该角色，未做修改"))
		return
	}
	if before.Role == userRoleSuperAdmin && role != userRoleSuperAdmin {
		others := 0
		for lockedID := range lockedSuperAdmins {
			if lockedID != id {
				others++
			}
		}
		if others == 0 {
			mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
				"该账号是最后一个超级管理员，不能降级；请先把另一个账号提升为超级管理员"))
			return
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE t_user
		   SET role = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL AND role = ?`,
		role, now, id, before.Role)
	if err != nil {
		s.logError(ctx, "ops_user_role_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		s.logError(ctx, "ops_user_role_update_not_applied")
		mw.WriteError(w, r, apierr.Newf(apierr.CodeResourceStateConflict,
			"账号角色已被其他操作修改，请刷新后重试"))
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
	// before/after carry the role and the status only. The row also holds the
	// password hash; copying the whole object into the audit trail would put a
	// credential in a table that is read by every audit.read holder (OPS and
	// FINANCE included).
	if err := s.insertAudit(ctx, tx, r, "USER", "ROLE_CHANGE", "t_user", id,
		userRoleAuditView(before), userRoleAuditView(after), reason); err != nil {
		s.logError(ctx, "ops_user_role_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_user_role_commit_failed: "+err.Error())
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

// lockSuperAdminSet locks every row that currently holds SUPER_ADMIN and
// returns the ids that still hold it once the locks are held.
//
// Why not the one-liner `SELECT COUNT(*) FROM t_user WHERE role='SUPER_ADMIN'
// AND deleted_at IS NULL FOR UPDATE`? t_user has no index on role —
// 0001_account_credential only indexes (status, created_at), plan_id and
// trial_expire_at — so InnoDB would answer it with a full table scan, and a
// locking read locks every row it scans. On the customer table that is a
// table-wide exclusive lock for the length of the transaction: it would block
// registration (an INSERT into t_user) and every other user write, and its cost
// would grow with the account count. Role changes are rare enough that the
// lock is never noticed until the day it freezes sign-ups.
//
// The two-phase form keeps the guarantee with a tiny lock footprint:
//
//	phase 1  read the ids without locks (a snapshot; it may be stale)
//	phase 2  take an exclusive primary-key lock on each id in ascending order
//	         and re-read its role under that lock
//
// Ascending order is what makes it deadlock-free: every role change walks the
// set in the same direction, so the second one waits on the first one's lock
// instead of each holding a row the other wants. Re-reading after the lock is
// what makes it correct: locking reads always see the latest committed row, so
// a SUPER_ADMIN demoted by a concurrent transaction is no longer counted.
//
// A stale phase-1 snapshot can only under-count (a SUPER_ADMIN created after
// it is missing), and under-counting refuses the demotion — the safe direction.
func lockSuperAdminSet(ctx context.Context, tx *sql.Tx) (map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM t_user
		 WHERE role = ? AND deleted_at IS NULL
		 ORDER BY id`, userRoleSuperAdmin)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// The transaction has a single connection: the rows must be drained and
	// closed before the next statement runs on it.
	rows.Close()

	locked := make(map[int64]bool, len(ids))
	for _, id := range ids {
		var role string
		err := tx.QueryRowContext(ctx, `
			SELECT role FROM t_user WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			id).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			// Demoted or soft-deleted between the snapshot and the lock.
			continue
		}
		if err != nil {
			return nil, err
		}
		if role == userRoleSuperAdmin {
			locked[id] = true
		}
	}
	return locked, nil
}

// userRoleAuditView is the before/after projection for a role change: role and
// status only, never the credential columns.
func userRoleAuditView(v userView) map[string]any {
	return map[string]any{
		"role":   v.Role,
		"status": v.Status,
	}
}

// duplicateKeyName names the unique key that produced a 1062, for the log.
//
// The driver message is read only to pick the key; the message itself carries
// the duplicated value (the hmac for uk_user_phone_hmac) and is never logged.
// "phone already used" and "e-mail already used" are the same 409 to the
// caller and two different operator actions, which is why the key is worth a
// field of its own.
func duplicateKeyName(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "uk_user_phone_hmac"):
		return "uk_user_phone_hmac"
	case strings.Contains(msg, "uk_user_email"):
		return "uk_user_email"
	case strings.Contains(msg, "uk_user_username"):
		return "uk_user_username"
	default:
		return "unknown_key"
	}
}
