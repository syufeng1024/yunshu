package ops

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/jsonx"
)

const operatorTestPassword = "Ops#2026-Pass"

func uniqueOperatorIdentifier() string {
	return fmt.Sprintf("t15-operator-%d@test.local", time.Now().UnixNano())
}

// cleanupCreatedUser removes the account an operator provisioned plus its audit
// rows. newTestUser's cleanup cannot be reused: it deletes audit rows by
// operator_id, and a created account's audit row is written by the operator who
// created it (target_id holds the new uid).
func cleanupCreatedUser(t *testing.T, uid int64) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testDB.ExecContext(ctx,
			`DELETE FROM t_admin_audit_log WHERE target_type = 't_user' AND target_id = ?`,
			strconv.FormatInt(uid, 10))
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_credit_account WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_api_key WHERE user_id = ?`, uid)
		_, _ = testDB.ExecContext(ctx, `DELETE FROM t_user WHERE id = ?`, uid)
	})
}

// auditRow is one t_admin_audit_log row reduced to the columns these tests
// assert on. before/after are decoded as raw JSON text so a test can prove the
// password never reached them.
type auditRow struct {
	OperatorID   int64
	OperatorRole string
	Module       string
	Action       string
	TargetType   string
	TargetID     string
	Before       string
	After        string
	Reason       string
	Result       string
}

func latestUserAudit(t *testing.T, targetID int64, action string) auditRow {
	t.Helper()
	var (
		row                   auditRow
		before, after, reason sql.NullString
	)
	err := testDB.QueryRowContext(context.Background(), `
		SELECT operator_id, operator_role, module, action, target_type, target_id,
		       before_json, after_json, reason, result
		  FROM t_admin_audit_log
		 WHERE target_type = 't_user' AND target_id = ? AND action = ?
		 ORDER BY id DESC
		 LIMIT 1`, strconv.FormatInt(targetID, 10), action).
		Scan(&row.OperatorID, &row.OperatorRole, &row.Module, &row.Action,
			&row.TargetType, &row.TargetID, &before, &after, &reason, &row.Result)
	if err != nil {
		t.Fatalf("load audit row (action=%s target=%d): %v", action, targetID, err)
	}
	row.Before = before.String
	row.After = after.String
	row.Reason = reason.String
	return row
}

// createOperator calls the handler and registers cleanup for the created row.
func createOperator(t *testing.T, svc *Service, actorUID int64, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := call(t, svc.CreateOperatorUser, http.MethodPost, "/api/admin/users", body,
		testCallOptions{uid: actorUID, role: "SUPER_ADMIN"})
	if rec.Code == http.StatusCreated {
		var detail userDetailView
		decodeBody(t, rec, &detail)
		cleanupCreatedUser(t, detail.User.ID)
	}
	return rec
}

// The happy path asserts all three writes the endpoint owes us: the t_user row
// (role from the body, NORMAL, empty agreement), the empty t_credit_account, and
// the CREATE_OPERATOR audit row. It then proves the account is usable by logging
// in with the password the operator typed — that is what ties the bcrypt hash and
// the normalized identifier to the login handler instead of asserting them in
// isolation.
func TestCreateOperatorUserProvisionsAccount(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")
	identifier := uniqueOperatorIdentifier()

	rec := createOperator(t, svc, adminUID, map[string]any{
		"identifier": identifier,
		"password":   operatorTestPassword,
		"nickname":   "新运营",
		"role":       "OPS",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	var detail userDetailView
	decodeBody(t, rec, &detail)
	if detail.User.Role != "OPS" {
		t.Fatalf("response role = %q, want OPS", detail.User.Role)
	}
	if detail.User.Status != "NORMAL" {
		t.Fatalf("response status = %q, want NORMAL", detail.User.Status)
	}
	if detail.User.Email != identifier {
		t.Fatalf("response email = %q, want %q", detail.User.Email, identifier)
	}
	if detail.User.UserNo == "" {
		t.Fatal("response user_no is empty")
	}
	if !detail.Balance.Exists {
		t.Fatal("response balance.exists = false; the credit account row is missing")
	}
	if strings.Contains(rec.Body.String(), operatorTestPassword) {
		t.Fatalf("response echoed the password: %s", rec.Body.String())
	}

	var (
		role, status, agreement, hash, algo string
	)
	if err := testDB.QueryRowContext(context.Background(), `
		SELECT role, status, agreement_version, password_hash, password_algo
		  FROM t_user WHERE id = ?`, detail.User.ID).
		Scan(&role, &status, &agreement, &hash, &algo); err != nil {
		t.Fatalf("load created user: %v", err)
	}
	if role != "OPS" || status != "NORMAL" || agreement != "" {
		t.Fatalf("stored role/status/agreement = %q/%q/%q, want OPS/NORMAL/empty",
			role, status, agreement)
	}
	if algo != "bcrypt" {
		t.Fatalf("password_algo = %q, want bcrypt", algo)
	}
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != auth.BcryptCost {
		t.Fatalf("bcrypt cost = %d (err=%v), want auth.BcryptCost=%d", cost, err, auth.BcryptCost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(operatorTestPassword)); err != nil {
		t.Fatalf("stored hash does not match the submitted password: %v", err)
	}

	var accounts int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_credit_account WHERE user_id = ?`, detail.User.ID).
		Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 {
		t.Fatalf("t_credit_account rows = %d, want 1", accounts)
	}

	createAudit := latestUserAudit(t, detail.User.ID, "CREATE_OPERATOR")
	if createAudit.OperatorID != adminUID || createAudit.OperatorRole != "SUPER_ADMIN" {
		t.Fatalf("audit operator = %d/%s, want %d/SUPER_ADMIN",
			createAudit.OperatorID, createAudit.OperatorRole, adminUID)
	}
	if createAudit.Module != "USER" || createAudit.TargetType != "t_user" || createAudit.Result != "OK" {
		t.Fatalf("audit row = %+v, want module USER / target t_user / result OK", createAudit)
	}
	if createAudit.Before != "" {
		t.Fatalf("CREATE_OPERATOR before_json = %q, want empty", createAudit.Before)
	}
	if !strings.Contains(createAudit.After, `"OPS"`) {
		t.Fatalf("audit after_json = %q, want the created role", createAudit.After)
	}
	if strings.Contains(createAudit.After, operatorTestPassword) ||
		strings.Contains(createAudit.After, "password") {
		t.Fatalf("audit after_json mentions the credential: %s", createAudit.After)
	}
	// auth.MaskIdentifier keeps the local part's first character and the domain,
	// so "t15-operator-<ts>@test.local" lands as "t***@test.local".
	if !strings.Contains(createAudit.After, `"t***@test.local"`) {
		t.Fatalf("audit after_json = %q, want the masked identifier", createAudit.After)
	}
	if strings.Contains(createAudit.After, identifier) {
		t.Fatalf("audit after_json holds the full identifier: %s", createAudit.After)
	}

	// The account must be usable: the same identifier and password the operator
	// typed reach the login handler and succeed.
	loginSvc := newLoginService(t)
	payload, err := jsonx.Marshal(map[string]string{
		"identifier": identifier,
		"password":   operatorTestPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	loginRec := httptest.NewRecorder()
	loginSvc.Login(loginRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(string(payload))))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login as the provisioned operator status=%d body=%s, want 200",
			loginRec.Code, loginRec.Body.String())
	}
	if !strings.Contains(loginRec.Body.String(), `"OPS"`) {
		t.Fatalf("login body = %s, want the OPS role", loginRec.Body.String())
	}
}

// PERSONAL is the one role this endpoint refuses. The customer path carries the
// trial grant, the device fingerprint and the agreement consent, none of which
// this form collects.
func TestCreateOperatorUserRejectsPersonalRole(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")
	identifier := uniqueOperatorIdentifier()

	rec := createOperator(t, svc, adminUID, map[string]any{
		"identifier": identifier,
		"password":   operatorTestPassword,
		"role":       "PERSONAL",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	body := errorBody(t, rec)
	if body.Error.Code != "invalid_request_body" || !strings.Contains(body.Error.Message, "role") {
		t.Fatalf("error = %+v, want invalid_request_body naming role", body.Error)
	}
	var count int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE email = ?`, identifier).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a PERSONAL account was created through the operator endpoint (rows=%d)", count)
	}
}

func TestCreateOperatorUserRejectsUnknownRole(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")

	for _, role := range []string{"", "ADMIN", "ops", "ROOT"} {
		t.Run("role="+role, func(t *testing.T) {
			rec := createOperator(t, svc, adminUID, map[string]any{
				"identifier": uniqueOperatorIdentifier(),
				"password":   operatorTestPassword,
				"role":       role,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			body := errorBody(t, rec)
			if !strings.Contains(body.Error.Message, "role") {
				t.Fatalf("message = %q, want the role field path", body.Error.Message)
			}
		})
	}
}

// A duplicate identifier is a 409 account_exists, the same code registration
// answers (erratum E-21: a state conflict, not a malformed body).
func TestCreateOperatorUserRejectsDuplicateIdentifier(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")
	identifier := uniqueOperatorIdentifier()

	first := createOperator(t, svc, adminUID, map[string]any{
		"identifier": identifier,
		"password":   operatorTestPassword,
		"role":       "FINANCE",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s, want 201", first.Code, first.Body.String())
	}
	second := createOperator(t, svc, adminUID, map[string]any{
		"identifier": strings.ToUpper(identifier), // NormalizeIdentifier lowercases it
		"password":   operatorTestPassword,
		"role":       "OPS",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s, want 409", second.Code, second.Body.String())
	}
	if got := errorBody(t, second).Error.Code; got != "account_exists" {
		t.Fatalf("duplicate code = %q, want account_exists", got)
	}
}

// The password rule is auth.ValidatePassword — 8..72 bytes — because a second
// rule set here would be the one that drifts.
func TestCreateOperatorUserRejectsWeakPassword(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")

	for name, password := range map[string]string{
		"too-short":  "short12",
		"blank":      "        ",
		"over-72byt": strings.Repeat("a", 73),
	} {
		t.Run(name, func(t *testing.T) {
			rec := createOperator(t, svc, adminUID, map[string]any{
				"identifier": uniqueOperatorIdentifier(),
				"password":   password,
				"role":       "OPS",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			body := errorBody(t, rec)
			if body.Error.Code != "invalid_request_body" ||
				!strings.Contains(body.Error.Message, "password") {
				t.Fatalf("error = %+v, want invalid_request_body naming password", body.Error)
			}
		})
	}
}

// A username is not a deliverable address, so an operator-provisioned account
// must not be created with one: it could never receive a recovery code. The
// built-in SUPER_ADMIN is the sole username account and is created by the
// controld startup bootstrap, not by this endpoint.
func TestCreateOperatorUserRejectsUsername(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")

	for name, identifier := range map[string]string{
		"plain":           "opszhang",
		"mixed-case":      "OpsZhang",
		"with-dot":        "ops.zhang",
		"with-underscore": "ops_zhang",
	} {
		t.Run(name, func(t *testing.T) {
			rec := createOperator(t, svc, adminUID, map[string]any{
				"identifier": identifier,
				"password":   operatorTestPassword,
				"nickname":   "运营张",
				"role":       "OPS",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			body := errorBody(t, rec)
			if body.Error.Code != "invalid_request_body" ||
				!strings.Contains(body.Error.Message, "identifier") {
				t.Fatalf("error = %+v, want invalid_request_body naming identifier", body.Error)
			}
		})
	}

	var n int
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM t_user WHERE username IS NOT NULL AND username LIKE 'ops%'`).Scan(&n); err != nil {
		t.Fatalf("count username rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("a rejected operator creation wrote %d username rows", n)
	}
}

func TestChangeUserRoleRejectsSelfChange(t *testing.T) {
	svc := newTestService(t)
	adminUID := newTestUser(t, "SUPER_ADMIN")

	rec := call(t, svc.ChangeUserRole, http.MethodPost,
		fmt.Sprintf("/api/admin/users/%d/role", adminUID),
		map[string]any{"role": "OPS", "reason": "手滑把自己降级了"},
		testCallOptions{uid: adminUID, role: "SUPER_ADMIN",
			pathVal: map[string]string{"id": fmt.Sprint(adminUID)}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self change status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if got := errorBody(t, rec).Error.Code; got != "permission_denied" {
		t.Fatalf("self change code = %q, want permission_denied", got)
	}
	var role string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT role FROM t_user WHERE id = ?`, adminUID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "SUPER_ADMIN" {
		t.Fatalf("own role = %q after a refused self change, want SUPER_ADMIN", role)
	}
}

// The last SUPER_ADMIN may not be demoted, or nobody could reach /admin again.
//
// The invariant counts rows, not a fixture, so the only way to test "the last
// one" is to make the target the last one: parkOtherSuperAdmins moves every
// other SUPER_ADMIN to PERSONAL and puts them back as soon as the call returns.
// The parked window is a single handler invocation; the restore also runs from a
// deferred safety net if an assertion aborts the test.
func TestChangeUserRoleRefusesLastSuperAdminDemotion(t *testing.T) {
	svc := newTestService(t)
	caller := newTestUser(t, "SUPER_ADMIN")
	target := newTestUser(t, "SUPER_ADMIN")

	restore := parkOtherSuperAdmins(t, target)
	rec := call(t, svc.ChangeUserRole, http.MethodPost,
		fmt.Sprintf("/api/admin/users/%d/role", target),
		map[string]any{"role": "PERSONAL", "reason": "转岗为普通用户"},
		testCallOptions{uid: caller, role: "SUPER_ADMIN",
			pathVal: map[string]string{"id": fmt.Sprint(target)}})
	restore()

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if got := errorBody(t, rec).Error.Code; got != "resource_state_conflict" {
		t.Fatalf("code = %q, want resource_state_conflict", got)
	}
	var role string
	if err := testDB.QueryRowContext(context.Background(),
		`SELECT role FROM t_user WHERE id = ?`, target).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "SUPER_ADMIN" {
		t.Fatalf("target role = %q, want it untouched (SUPER_ADMIN)", role)
	}
}

// parkOtherSuperAdmins demotes every SUPER_ADMIN except keep and returns a
// restore function. It is idempotent so the caller can both call it directly and
// defer it.
func parkOtherSuperAdmins(t *testing.T, keep int64) func() {
	t.Helper()
	ctx := context.Background()
	rows, err := testDB.QueryContext(ctx, `
		SELECT id FROM t_user
		 WHERE role = 'SUPER_ADMIN' AND deleted_at IS NULL AND id <> ?`, keep)
	if err != nil {
		t.Fatalf("list super admins: %v", err)
	}
	ids := make([]int64, 0, 4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan super admin id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("list super admins: %v", err)
	}
	rows.Close()

	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	if len(ids) > 0 {
		if _, err := testDB.ExecContext(ctx,
			`UPDATE t_user SET role = 'PERSONAL' WHERE id IN (`+placeholders(len(ids))+`)`,
			args...); err != nil {
			t.Fatalf("park super admins: %v", err)
		}
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if len(ids) == 0 {
			return
		}
		if _, err := testDB.ExecContext(context.Background(),
			`UPDATE t_user SET role = 'SUPER_ADMIN' WHERE id IN (`+placeholders(len(ids))+`)`,
			args...); err != nil {
			t.Logf("restore super admins: %v", err)
		}
	}
	t.Cleanup(restore)

	// The fixture is only meaningful if the target really is the last one.
	var count int
	if err := testDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_user WHERE role = 'SUPER_ADMIN' AND deleted_at IS NULL`,
	).Scan(&count); err != nil {
		t.Fatalf("count super admins: %v", err)
	}
	if count != 1 {
		t.Fatalf("fixture: %d super admins remain, want exactly 1", count)
	}
	return restore
}

// Promotion and demotion both work, and both write a ROLE_CHANGE audit row whose
// before/after name the two roles and nothing else.
func TestChangeUserRolePromotesAndDemotes(t *testing.T) {
	svc := newTestService(t)
	caller := newTestUser(t, "SUPER_ADMIN")
	target := newTestUser(t, "PERSONAL")

	change := func(role, reason string) *httptest.ResponseRecorder {
		t.Helper()
		return call(t, svc.ChangeUserRole, http.MethodPost,
			fmt.Sprintf("/api/admin/users/%d/role", target),
			map[string]any{"role": role, "reason": reason},
			testCallOptions{uid: caller, role: "SUPER_ADMIN",
				pathVal: map[string]string{"id": fmt.Sprint(target)}})
	}
	storedRole := func() string {
		t.Helper()
		var role string
		if err := testDB.QueryRowContext(context.Background(),
			`SELECT role FROM t_user WHERE id = ?`, target).Scan(&role); err != nil {
			t.Fatal(err)
		}
		return role
	}

	rec := change("OPS", "转入运营团队")
	if rec.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var detail userDetailView
	decodeBody(t, rec, &detail)
	if detail.User.Role != "OPS" {
		t.Fatalf("response role = %q, want OPS", detail.User.Role)
	}
	if got := storedRole(); got != "OPS" {
		t.Fatalf("stored role = %q after promotion, want OPS", got)
	}
	promoteAudit := latestUserAudit(t, target, "ROLE_CHANGE")
	if promoteAudit.OperatorID != caller || promoteAudit.OperatorRole != "SUPER_ADMIN" {
		t.Fatalf("promote audit operator = %d/%s, want %d/SUPER_ADMIN",
			promoteAudit.OperatorID, promoteAudit.OperatorRole, caller)
	}
	if !strings.Contains(promoteAudit.Before, `"PERSONAL"`) {
		t.Fatalf("promote before_json = %q, want PERSONAL", promoteAudit.Before)
	}
	if !strings.Contains(promoteAudit.After, `"OPS"`) {
		t.Fatalf("promote after_json = %q, want OPS", promoteAudit.After)
	}
	if promoteAudit.Reason != "转入运营团队" {
		t.Fatalf("promote reason = %q, want the submitted reason", promoteAudit.Reason)
	}
	if strings.Contains(promoteAudit.Before+promoteAudit.After, "password") ||
		strings.Contains(promoteAudit.Before+promoteAudit.After, "not-a-real-hash") {
		t.Fatalf("role audit leaked credential columns: before=%s after=%s",
			promoteAudit.Before, promoteAudit.After)
	}

	rec = change("PERSONAL", "试用期结束，转回普通用户")
	if rec.Code != http.StatusOK {
		t.Fatalf("demote status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &detail)
	if detail.User.Role != "PERSONAL" {
		t.Fatalf("response role = %q, want PERSONAL", detail.User.Role)
	}
	if got := storedRole(); got != "PERSONAL" {
		t.Fatalf("stored role = %q after demotion, want PERSONAL", got)
	}
	demoteAudit := latestUserAudit(t, target, "ROLE_CHANGE")
	if !strings.Contains(demoteAudit.Before, `"OPS"`) {
		t.Fatalf("demote before_json = %q, want OPS", demoteAudit.Before)
	}
	if !strings.Contains(demoteAudit.After, `"PERSONAL"`) {
		t.Fatalf("demote after_json = %q, want PERSONAL", demoteAudit.After)
	}
}

// A same-role submit is refused rather than written: an audit row that says
// "OPS → OPS" is noise in a trail operators use to answer "who changed this".
func TestChangeUserRoleRefusesNoOp(t *testing.T) {
	svc := newTestService(t)
	caller := newTestUser(t, "SUPER_ADMIN")
	target := newTestUser(t, "FINANCE")

	rec := call(t, svc.ChangeUserRole, http.MethodPost,
		fmt.Sprintf("/api/admin/users/%d/role", target),
		map[string]any{"role": "FINANCE", "reason": "无变化"},
		testCallOptions{uid: caller, role: "SUPER_ADMIN",
			pathVal: map[string]string{"id": fmt.Sprint(target)}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-op status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
}

func TestChangeUserRoleValidatesBody(t *testing.T) {
	svc := newTestService(t)
	caller := newTestUser(t, "SUPER_ADMIN")
	target := newTestUser(t, "PERSONAL")

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"unknown-role", map[string]any{"role": "OWNER", "reason": "理由是有的"}, "role"},
		{"empty-role", map[string]any{"role": "", "reason": "理由是有的"}, "role"},
		{"missing-reason", map[string]any{"role": "OPS"}, "reason"},
		{"blank-reason", map[string]any{"role": "OPS", "reason": "   "}, "reason"},
		{"long-reason", map[string]any{"role": "OPS", "reason": strings.Repeat("长", 256)}, "reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(t, svc.ChangeUserRole, http.MethodPost,
				fmt.Sprintf("/api/admin/users/%d/role", target), tc.body,
				testCallOptions{uid: caller, role: "SUPER_ADMIN",
					pathVal: map[string]string{"id": fmt.Sprint(target)}})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			body := errorBody(t, rec)
			if body.Error.Code != "invalid_request_body" ||
				!strings.Contains(body.Error.Message, tc.field) {
				t.Fatalf("error = %+v, want invalid_request_body naming %s", body.Error, tc.field)
			}
		})
	}
}
