// Package bootstrap creates the built-in SUPER_ADMIN at controld startup.
//
// Why this is its own package rather than part of internal/boot: internal/boot
// is the configuration snapshot / price cache / periodic refresh machinery and
// is imported by gatewayd-facing code. Account creation belongs to the control
// plane only, and putting it in one place makes "gatewayd must never bootstrap
// an account" a property of the import graph rather than a review checklist.
//
// The package is deliberately small: one environment parser, one idempotent
// ensure function, two sentinel errors. All credential rules (password length,
// bcrypt cost, identifier normalization, the t_user INSERT column list) are
// reused from internal/control/auth so the built-in account cannot slowly drift
// into a differently-validated shape than every other account.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/jsonx"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// Environment variables. These three names are the contract: the runbook, the
// go-live checklist, .env.example and the deploy README all spell them exactly
// this way. No aliases — a second accepted spelling turns "I changed it and
// nothing happened" into an investigation.
const (
	EnvDisabled = "ADMIN_BOOTSTRAP_DISABLED"
	EnvUsername = "ADMIN_BOOTSTRAP_USERNAME"
	EnvPassword = "ADMIN_BOOTSTRAP_PASSWORD"
)

const (
	// defaultUsername is the published built-in account name.
	defaultUsername = "admin"

	// defaultPassword is the published initial password, and is the ONLY
	// occurrence of that literal in Go code.
	//
	// It exists because refusing to start when the variable is unset would take
	// every existing controld deployment down on upgrade: optional variables are
	// simply absent in real environments (see deploy/k8s/configmap.yaml, which
	// does not carry FP_HMAC_KEY either), and a control-plane process must not
	// become unstartable because a new, one-shot startup action is missing its
	// input. Falling back to the documented default keeps the upgrade additive.
	//
	// Mitigations that make this acceptable: the value is public and documented
	// as weak, the startup path logs a WARN the first time it creates the
	// account, docs/go-live-checklist.md makes changing it a release gate, and
	// the login lock does not exempt this account. P1 (forced password change on
	// first login) is the real fix and is out of scope here.
	//
	// Never log it, never echo it in an error, never restate it in a message.
	// It must stay byte-identical to ADMIN_BOOTSTRAP_PASSWORD in .env.example.
	defaultPassword = "12345678"

	// superAdminNickname fills t_user.nickname so userView.display_name is not
	// empty: view() falls back nickname -> email -> phone, and this account has
	// neither an email nor a phone, so an empty nickname would render a blank
	// name in the console header.
	superAdminNickname = "超级管理员"

	// logStage groups every line this package writes.
	logStage = "startup.bootstrap"
)

// Options is the complete input to the bootstrap.
//
// Role and nickname are constants, not options: letting an operator choose the
// role would allow "create the built-in account as an ordinary customer",
// which is not a configuration choice but a bug.
type Options struct {
	Disabled bool   // ADMIN_BOOTSTRAP_DISABLED
	Username string // ADMIN_BOOTSTRAP_USERNAME, default "admin"
	Password string // ADMIN_BOOTSTRAP_PASSWORD, default defaultPassword
}

// Outcome says what the bootstrap did. It is a string so one log line can carry
// it without a lookup table, and so a future counter label does not need a
// translation.
type Outcome string

const (
	// OutcomeDisabled means ADMIN_BOOTSTRAP_DISABLED was true and nothing was
	// read from or written to the database.
	OutcomeDisabled Outcome = "disabled"
	// OutcomeCreated means this process inserted the account.
	OutcomeCreated Outcome = "created"
	// OutcomeSkipped means the username already existed and nothing was
	// modified — not the role, not the status, not the password.
	OutcomeSkipped Outcome = "skipped"
)

// Result is what EnsureSuperAdmin reports back to main.
type Result struct {
	Outcome  Outcome
	UID      int64  // set only for OutcomeCreated
	Username string // the normalized username
}

// ErrSchemaNotMigrated is returned when t_user has no username column (MySQL
// 1054). It is a sentinel so main can print "run make migrate first" instead of
// making an operator decode a 1054 from a Go error string.
var ErrSchemaNotMigrated = errors.New("bootstrap: t_user.username 不存在，迁移 0027 未应用")

// ErrInvalidConfig is returned for input that can never become valid, so the
// caller must refuse to start rather than retry. OptionsFromEnv already returns
// these directly; EnsureSuperAdmin re-checks the same rules because tests and
// future callers may construct Options by hand.
var ErrInvalidConfig = errors.New("bootstrap: 配置不合法")

// Deps are the collaborators. They are fields rather than package-level
// variables so a test can point them at the test schema and a frozen clock.
type Deps struct {
	DB     *sql.DB
	Audit  *audit.Writer
	Logger *obslog.Logger
	Now    clock.Clock
}

func (d Deps) now() clock.Clock {
	if d.Now != nil {
		return d.Now
	}
	return clock.Real{}
}

// OptionsFromEnv reads and fully validates the three variables without touching
// the database. A returned error means the process must not start: none of these
// conditions can heal on its own.
//
// Order matters — it is the order the error messages describe: the switch, then
// the username, then the password. It also means a deployment with two problems
// hears about the switch first, which is the cheapest one to fix.
//
// The switch is also a hard short-circuit: when ADMIN_BOOTSTRAP_DISABLED is true
// the documented promise is "完全跳过引导", so the username and the password are
// not validated at all. A deployment that turned the bootstrap off must keep
// starting even if some unrelated value in the same .env file is now illegal —
// the variables are read but never used, and refusing to start would turn a
// no-op into an outage. Validation lives on the branch that actually creates the
// account (and again inside EnsureSuperAdmin, because callers may build Options
// by hand).
func OptionsFromEnv() (Options, error) {
	opts := Options{Username: defaultUsername, Password: defaultPassword}

	if raw := strings.TrimSpace(os.Getenv(EnvDisabled)); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Options{}, fmt.Errorf("%w：%s 必须是布尔值（true/false/1/0），当前值无法解析",
				ErrInvalidConfig, EnvDisabled)
		}
		opts.Disabled = parsed
	}

	if opts.Disabled {
		// The username is still carried through (unvalidated) so a log line or a
		// test can echo what the environment said; the password is not, because
		// nothing reads it on this path and keeping a secret that is not used
		// only widens the surface for it to be logged by accident.
		if raw := strings.TrimSpace(os.Getenv(EnvUsername)); raw != "" {
			opts.Username = raw
		}
		opts.Password = ""
		return opts, nil
	}

	if raw := strings.TrimSpace(os.Getenv(EnvUsername)); raw != "" {
		opts.Username = raw
	}

	// Only the trailing line break is removed, never the surrounding spaces:
	// a space is a legal password character, and TrimSpace would silently change
	// the secret. The trailing "\r\n" is not: a CRLF .env / configmap file hands
	// it to us, and a '\r' baked into the password makes the documented value
	// unloggable forever. An entry that is empty after trimming falls back to the
	// documented default, like an absent one.
	if raw := strings.TrimRight(os.Getenv(EnvPassword), "\r\n"); raw != "" {
		opts.Password = raw
	}

	// The username may appear in the error (it is not a secret); the password
	// value never does, and ValidatePassword's own message does not include it.
	ident, err := auth.NormalizeIdentifier(opts.Username)
	if err != nil || ident.Kind != auth.KindUsername {
		return Options{}, fmt.Errorf("%w：%s 必须是合法用户名（3..32 字节，字母开头，仅 a-z0-9._-；不能是邮箱或手机号）",
			ErrInvalidConfig, EnvUsername)
	}
	opts.Username = ident.Value

	if err := auth.ValidatePassword(opts.Password); err != nil {
		return Options{}, fmt.Errorf("%w：%s 不符合密码规则（8..72 字节、不可为纯空白）：%v",
			ErrInvalidConfig, EnvPassword, err)
	}
	return opts, nil
}

// EnsureSuperAdmin makes sure the built-in SUPER_ADMIN exists, and reports what
// it did. It is safe to call on every start and from several replicas at once.
//
// The insert is attempted first and a duplicate key is the "already exists"
// answer. A SELECT-then-INSERT would be a two-phase race: two replicas starting
// together both see nothing and both insert. The unique key uk_user_username is
// the actual source of idempotency; this function only translates its error.
//
// The user row, the credit account and the audit row commit together. A
// bootstrap that created the account but left no audit trail would be a silent
// privilege grant, so a failing audit aborts the whole thing.
func EnsureSuperAdmin(ctx context.Context, deps Deps, opts Options) (Result, error) {
	if deps.DB == nil {
		return Result{}, fmt.Errorf("%w：数据库连接为空", ErrInvalidConfig)
	}
	if deps.Audit == nil {
		// Checked up front, not after the INSERT: a bootstrap that created the
		// account but could not record it would be an unlogged privilege grant,
		// and finding that out mid-transaction is a worse error to reason about
		// even though the rollback covers it.
		return Result{}, fmt.Errorf("%w：审计写入器为空", ErrInvalidConfig)
	}
	if opts.Disabled {
		logInfo(ctx, deps, "admin bootstrap: "+EnvDisabled+"=true，已跳过")
		return Result{Outcome: OutcomeDisabled, Username: opts.Username}, nil
	}

	ident, err := auth.NormalizeIdentifier(opts.Username)
	if err != nil || ident.Kind != auth.KindUsername {
		return Result{}, fmt.Errorf("%w：%s 必须是合法用户名", ErrInvalidConfig, EnvUsername)
	}
	if err := auth.ValidatePassword(opts.Password); err != nil {
		return Result{}, fmt.Errorf("%w：%s 不符合密码规则：%v", ErrInvalidConfig, EnvPassword, err)
	}

	// Stage 1: the pre-check.
	//
	// 预查只为省一次 bcrypt；正确性由 uk_user_username 保证。
	//
	// This SELECT is NOT the idempotency mechanism: two replicas starting
	// together can both miss here and both insert, which is why the unique key —
	// and only the unique key — decides. What the pre-check buys is the ~100ms
	// bcrypt hash on every restart after the first, an avoided wasted INSERT, and
	// a log line that can name the existing role/status instead of translating a
	// 1062.
	if existing, found, err := lookupExisting(ctx, deps.DB, ident.Value); err != nil {
		if isUnknownColumn(err) {
			// A schema without migration 0027 fails here, at the pre-check, not at
			// the INSERT. Both are classified the same way for main.
			return Result{}, ErrSchemaNotMigrated
		}
		return Result{}, fmt.Errorf("bootstrap: 预查失败：%w", err)
	} else if found {
		return reportExisting(ctx, deps, ident.Value, existing, nil), nil
	}

	// Stage 2: the actual creation.
	//
	// Hashed before the transaction opens: bcrypt at cost 12 costs ~100ms of CPU
	// and holding a database transaction open for it would pin a connection and
	// stretch the write lock window for no benefit.
	hash, err := bcrypt.GenerateFromPassword([]byte(opts.Password), auth.BcryptCost)
	if err != nil {
		// Deliberately not wrapping the password: bcrypt's error text never
		// contains it, and neither does this message.
		return Result{}, fmt.Errorf("bootstrap: 生成密码哈希失败：%w", err)
	}

	now := deps.now().Now().UTC()
	tx, err := deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: 开启事务失败：%w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	uid, err := auth.InsertUserRowTx(ctx, tx, ident, superAdminNickname, "", string(hash),
		auth.RoleSuperAdmin, now)
	if err != nil {
		if isUnknownColumn(err) {
			return Result{}, ErrSchemaNotMigrated
		}
		if auth.IsDuplicate(err) {
			// Every 1062 from this INSERT means "no new row was created", so the
			// answer is always OutcomeSkipped — never an error. Two keys can
			// produce it, and only the message tells them apart:
			//
			//   uk_user_username  a concurrent replica won the race (the
			//                     pre-check saw nothing a moment ago);
			//   uk_user_no        insertUserRow already retried this five times
			//                     and lost; the next start will try again with a
			//                     fresh random suffix.
			//
			// The raw driver error is appended on purpose: it carries the key
			// name, which is the only thing that lets an operator tell those two
			// apart in the log.
			_ = tx.Rollback()
			identValue := ident.Value
			if !strings.Contains(err.Error(), "uk_user_username") {
				logWarn(ctx, deps, fmt.Sprintf(
					"admin bootstrap: username=%s 未创建：user_no 分配冲突且重试已耗尽（非用户名冲突），下次启动会重试。err=%v",
					identValue, err))
				return Result{Outcome: OutcomeSkipped, Username: identValue}, nil
			}
			if existing, found, lookupErr := lookupExisting(ctx, deps.DB, identValue); lookupErr == nil && found {
				return reportExisting(ctx, deps, identValue, existing, err), nil
			}
			logWarn(ctx, deps, fmt.Sprintf(
				"admin bootstrap: username=%s 唯一键已被占用，视为已存在；未做任何修改。err=%v",
				identValue, err))
			return Result{Outcome: OutcomeSkipped, Username: identValue}, nil
		}
		return Result{}, fmt.Errorf("bootstrap: 创建内置管理员失败：%w", err)
	}

	// Mark the password as "initial" in the same transaction. This is what makes
	// the console show a change-password reminder after login; it is a hint, not
	// a gate, so the account logs in and works exactly like any other. Changing
	// the password clears the flag (internal/control/auth/password.go).
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_user SET password_is_initial = 1 WHERE id = ?`, uid,
	); err != nil {
		return Result{}, fmt.Errorf("bootstrap: 标记初始口令失败：%w", err)
	}

	// The credit account must exist even at zero balance: SUPER_ADMIN's own
	// /api/console/overview reads it, and without the row the balance block on
	// the console home page renders as broken. Every other account has one.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
		uid, now, now,
	); err != nil {
		return Result{}, fmt.Errorf("bootstrap: 创建信用账户失败：%w", err)
	}

	after, err := jsonx.Marshal(map[string]string{
		"identifier":        auth.MaskIdentifier(ident.Kind, ident.Value),
		"identifier_kind":   ident.Kind,
		"role":              auth.RoleSuperAdmin,
		"status":            "NORMAL",
		"agreement_version": "",
	})
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: 序列化审计 after_json 失败：%w", err)
	}
	if err := deps.Audit.Insert(ctx, tx, audit.Entry{
		// Operator 0 + role SYSTEM is the "no human did this" marker; audit
		// readers use it to separate machine actions from operator actions.
		OperatorID:   0,
		OperatorRole: "SYSTEM",
		Module:       "USER",
		Action:       "CREATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(uid, 10),
		After:        after,
		Result:       "OK",
	}); err != nil {
		return Result{}, fmt.Errorf("bootstrap: 写审计失败：%w", err)
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("bootstrap: 提交事务失败：%w", err)
	}
	committed = true

	// WARN, not INFO: this line is one of the mitigations for shipping a
	// publicly known initial password, so it must be impossible to miss. It
	// names the account and the action and never the password or its default.
	logWarn(ctx, deps, fmt.Sprintf(
		"admin bootstrap: 已创建内置超级管理员 username=%s（role=%s）；初始密码取 %s，请立即修改初始密码",
		ident.Value, auth.RoleSuperAdmin, EnvPassword))

	return Result{Outcome: OutcomeCreated, UID: uid, Username: ident.Value}, nil
}

// existingRow is what the pre-check reads: enough to log accurately, nothing
// more.
type existingRow struct {
	UID       int64
	Role      string
	Status    string
	DeletedAt sql.NullTime
}

// lookupExisting reports whether any row already owns username, live or
// soft-deleted.
//
// The WHERE clause deliberately has no `deleted_at` filter, and that is the
// single easiest thing to get wrong here. uk_user_username does not contain
// deleted_at, so a soft-deleted row still occupies the name. A pre-check that
// filtered it out would answer "not found", the INSERT would hit 1062, and the
// bootstrap would report "already exists" — while findUser (which does filter
// deleted_at IS NULL) can never reach that row. The result is a silent deadlock:
// the bootstrap says the admin exists, and logging in says it does not.
//
// deleted_at is therefore SELECTed (to tell the soft-deleted case apart and warn
// about it) and never used as a WHERE predicate.
func lookupExisting(ctx context.Context, db *sql.DB, username string) (existingRow, bool, error) {
	var row existingRow
	err := db.QueryRowContext(ctx,
		`SELECT id, role, status, deleted_at FROM t_user WHERE username = ?`,
		username,
	).Scan(&row.UID, &row.Role, &row.Status, &row.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return existingRow{}, false, nil
	}
	if err != nil {
		return existingRow{}, false, err
	}
	return row, true, nil
}

// reportExisting logs what the pre-check found and returns the skipped result.
//
// Nothing is repaired, ever: rewriting the role or the status here would mean a
// restart could silently promote — or re-enable — an account somebody
// deliberately demoted or disabled. A soft-deleted row is left deleted for the
// same reason (reviving it is a data repair, not a startup task).
//
// Levels follow what the operator has to do about it: the plain repeat is INFO
// (it happens on every start, and a WARN here would train people to ignore the
// stage); an unexpected role/status or a soft-deleted occupant is WARN because a
// human has to look.
func reportExisting(ctx context.Context, deps Deps, username string, row existingRow, cause error) Result {
	suffix := ""
	if cause != nil {
		suffix = fmt.Sprintf("。err=%v", cause)
	}
	switch {
	case row.DeletedAt.Valid:
		logWarn(ctx, deps, fmt.Sprintf(
			"admin bootstrap: username=%s 被软删除记录占用，唯一键 uk_user_username 仍被占，需人工处理（引导不会复活它）；未做任何修改%s",
			username, suffix))
	case row.Role != auth.RoleSuperAdmin || row.Status != "NORMAL":
		logWarn(ctx, deps, fmt.Sprintf(
			"admin bootstrap: username=%s 已存在但 role=%s status=%s，引导未做任何修改，请人工核查%s",
			username, row.Role, row.Status, suffix))
	default:
		logInfo(ctx, deps, fmt.Sprintf(
			"admin bootstrap: username=%s 已存在（role=%s status=%s），未做任何修改%s",
			username, row.Role, row.Status, suffix))
	}
	return Result{Outcome: OutcomeSkipped, Username: username}
}

// isUnknownColumn reports MySQL 1054 (unknown column). It is how a schema that
// predates migration 0027 is detected: the driver reports 1054 and names the
// column, and nothing else in this INSERT can produce it.
func isUnknownColumn(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1054
}

// logInfo and logWarn take the caller's context: this package must not mint a
// fresh background context (scripts/lint.sh enforces that under internal/), and
// the trace/request metadata on the startup context should reach the log line.
func logInfo(ctx context.Context, deps Deps, msg string) {
	if deps.Logger != nil {
		deps.Logger.Info(ctx, msg, obslog.Fields{Stage: logStage})
	}
}

func logWarn(ctx context.Context, deps Deps, msg string) {
	if deps.Logger != nil {
		deps.Logger.Warn(ctx, msg, obslog.Fields{Stage: logStage})
	}
}
