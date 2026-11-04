// Package control assembles the controld HTTP surface: /api/v1/auth/*,
// /api/console/* and /api/admin/*.
//
// Every route is registered through one helper that declares its permission.
// A handler cannot opt out of authorization by forgetting a menu entry: the
// permission is part of the route table in this file (T11 hard constraint 1).
//
// T12 routes are implemented in internal/control/{user,key,model}; this file
// only wires them so the permission column stays in one place.
package control

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auditq"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/control/console"
	"yunshu.dev/core/internal/control/fund"
	"yunshu.dev/core/internal/control/key"
	"yunshu.dev/core/internal/control/logq"
	"yunshu.dev/core/internal/control/model"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/control/ops"
	"yunshu.dev/core/internal/control/publicapi"
	"yunshu.dev/core/internal/control/review"
	"yunshu.dev/core/internal/control/ticket"
	"yunshu.dev/core/internal/control/usage"
	"yunshu.dev/core/internal/control/user"
	"yunshu.dev/core/internal/cryptobox"
	obslog "yunshu.dev/core/internal/obs/log"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
)

// Deps is the control plane's dependency set. controld owns the general main
// database and Redis pools (TDS §1).
type Deps struct {
	DB *sql.DB
	// LogDB is the dedicated log database. It is optional: nil means the
	// single-database deployment, where t_usage_record and
	// t_upstream_attempt live in DB. The T14 log reads use it exclusively
	// when set, so a production split never falls back to the main schema.
	LogDB    *sql.DB
	Redis    *redisx.Client
	Sessions *mw.SessionManager
	Audit    *audit.Writer
	Logger   *obslog.Logger
	Now      clock.Clock

	// PublishRoutes republishes cfg:snap:ROUTING. The IP whitelist travels in
	// that snapshot, so a key created or edited with rules must trigger a
	// publish; P06 treats a missing snapshot entry as "no whitelist" (allow
	// all), which would make a fresh whitelist ineffective. Nil is allowed in
	// unit tests and degrades to the timer's four-minute refresh.
	PublishRoutes func(ctx context.Context) error

	// PublishPrices rebuilds K19 (standard price per model). nil is allowed in
	// unit tests; a committed price without a refresh answers 503
	// config_unavailable so the operator knows gatewayd still sees the old one.
	PublishPrices func(ctx context.Context) error

	// PaySecrets maps a payment channel (WECHAT / ALIPAY) to its callback
	// signing secret. It comes from the process environment: the 45 frozen
	// configuration keys carry no payment credential, and a secret stored in
	// t_config_item would be readable by every process that reads config.
	// A channel with no secret answers 503 and never skips verification.
	PaySecrets map[string]string

	// Mailer is the outbound e-mail seam. Nil is allowed: notifications fall
	// back to the in-app row plus a WARN, and verification codes answer 503
	// instead of pretending a message was sent.
	Mailer fund.Mailer

	// VerificationMode is AUTH_VERIFICATION_MODE for /api/v1/auth: "off",
	// "log" or "smtp". Empty means "log" (see auth.verificationMode).
	VerificationMode string

	// SMS is the handset delivery seam (internal/notify/sms). Nil is allowed:
	// the phone channel then answers 503 instead of pretending, and every
	// e-mail path keeps working.
	SMS auth.SMSSender
	// SMSMode is AUTH_SMS_MODE: "off", "log" or "provider". Empty means "off",
	// the opposite default from VerificationMode, because a text message is
	// billed per unit.
	SMSMode string

	// FingerprintKey is FP_HMAC_KEY from the environment (the YUNSHU_ prefix
	// is accepted as a fallback). Empty makes the trial claim answer 503
	// rather than hash device identifiers with no key.
	FingerprintKey string

	// PhoneHMACKey is PHONE_HMAC_KEY from the environment. It is deliberately a
	// separate secret from FingerprintKey: a phone number is an enumerable
	// ~10^9 space, so leaking its key is equivalent to leaking the numbers,
	// while a fingerprint is client-chosen high-entropy input. Empty means
	// absent or shorter than 32 bytes: phone *writes* (registration, operator
	// provisioning) answer 503 config_unavailable, and every read path — login,
	// recovery, listings — is untouched because it reads the plaintext column.
	PhoneHMACKey string

	// Metrics receives log-read counters (gw_log_body_open_failed_total).
	// Nil is allowed in tests; the daemon passes the same registry it serves
	// on /metrics.
	Metrics *obsmetrics.Recorder

	// GatewayInternalURL is gatewayd's internal base URL (compose:
	// http://gatewayd:8080). The console playground posts to
	// <GatewayInternalURL>/internal/v1/chat/completions. Empty leaves the relay
	// disabled with 503 config_unavailable.
	GatewayInternalURL string

	// InternalCallSecret signs the relayed request. It must equal gatewayd's
	// INTERNAL_CALL_SECRET and must not reuse SESSION_SECRET or either KEK.
	// Shorter than 32 bytes leaves the relay disabled.
	InternalCallSecret string

	// InternalHTTPClient is optional. Nil builds the no-proxy relay transport
	// with a 130s response-header budget (the gateway's non-stream P01 budget
	// is 120s). Tests inject one to observe the stream.
	InternalHTTPClient *http.Client
}

// Server holds the route table and its dependencies.
type Server struct {
	d       Deps
	auth    *auth.Service
	user    *user.Service
	keys    *key.Service
	models  *model.Service
	fund    *fund.Service
	ops     *ops.Service
	tickets *ticket.Service
	reviews *review.Service
	logs    *logq.Service
	usage   *usage.Service
	auditQ  *auditq.Service
	pub     *publicapi.Service
	console *console.Service
}

// New validates the wiring and builds the server.
func New(d Deps) (*Server, error) {
	if d.DB == nil || d.Redis == nil || d.Sessions == nil {
		return nil, errors.New("control: server requires mysql, redis and a session manager")
	}
	if d.Now == nil {
		d.Now = clock.Real{}
	}
	if d.Audit == nil {
		d.Audit = &audit.Writer{DB: d.DB, Now: d.Now}
	}
	keys := key.New(d.DB, d.Redis, d.Now, d.Logger)
	keys.PublishRoutes = d.PublishRoutes
	funds := fund.New(d.DB, d.Redis, nil, d.Audit, d.Now, d.Logger)
	funds.PaySecrets = d.PaySecrets
	funds.PublishRoutes = d.PublishRoutes
	funds.Mailer = d.Mailer
	funds.FingerprintKey = d.FingerprintKey

	// CHANNEL_KEY_KEK is required only by the channel write path. A missing or
	// malformed KEK leaves Box nil: reads still work, and creating or rotating
	// an upstream key answers 503 config_unavailable instead of storing
	// plaintext or an envelope no node can open.
	box, boxErr := cryptobox.FromEnv()
	if boxErr != nil && d.Logger != nil {
		d.Logger.Warn(context.TODO(), "control: CHANNEL_KEY_KEK unavailable: "+boxErr.Error(),
			obslog.Fields{Stage: "control"})
	}
	opss := ops.New(d.DB, d.Audit, d.Now, d.Logger, d.PublishRoutes, box)
	opss.KeyAuth = redisx.NewKeyAuthCache(d.Redis)
	opss.PriceRefresh = d.PublishPrices
	opss.PhoneHMACKey = d.PhoneHMACKey
	opss.ConfigPublisher = &config.Publisher{
		DB:          d.DB,
		Redis:       config.NewGoRedis(d.Redis.Raw()),
		Now:         d.Now.Now,
		LockTimeout: 5 * time.Second,
	}
	// Tickets need no Redis, but they reuse the one outbound seam. fund.Mailer
	// and ticket.Mailer have identical method sets, so no adapter is needed.
	tickets := ticket.New(d.DB, d.Audit, d.Now, d.Logger)
	if d.Mailer != nil {
		tickets.Mailer = d.Mailer
	}
	// The review centre decides through the fund service; it has no balance
	// writer of its own. The ledger store is shared with fund so both paths use
	// one entry_no generator.
	reviews := review.New(d.DB, d.Audit, d.Now, d.Logger, funds)
	authSvc := &auth.Service{
		DB:               d.DB,
		Sessions:         d.Sessions,
		Audit:            d.Audit,
		Logger:           d.Logger,
		Now:              d.Now,
		VerificationMode: d.VerificationMode,
		SMSMode:          d.SMSMode,
		PhoneHMACKey:     d.PhoneHMACKey,
	}
	if d.Mailer != nil {
		authSvc.Mailer = d.Mailer
	}
	if d.SMS != nil {
		authSvc.SMSSender = d.SMS
	}
	authSvc.CancellableCheck = tickets.CheckAccountCancellable
	// Log reads go to LogDB when a separate log database is configured. The
	// service falls back to DB on its own, so both the split and the
	// single-database deployments share one route table.
	logs := logq.New(d.DB, d.LogDB, d.Now, d.Logger)
	logs.Metrics = d.Metrics
	// Usage reads stay on the main pool: t_usage_record carries its dimension
	// snapshots, so the aggregation never joins the log database and never
	// joins t_model / t_channel.
	usageSvc := usage.New(d.DB, d.Redis, d.Now, d.Logger)
	auditSvc := auditq.New(d.DB, d.Now, d.Logger)
	// The playground relay owns no balance and writes no usage row: it only
	// signs the DB-authoritative key attributes and forwards to gatewayd, where
	// P10/P14 do the billing (T27 hard constraint 8).
	consoleSvc := console.New(console.Deps{
		DB:         d.DB,
		GatewayURL: d.GatewayInternalURL,
		Secret:     d.InternalCallSecret,
		Client:     d.InternalHTTPClient,
		Logger:     d.Logger,
		Now:        d.Now,
	})
	return &Server{
		d:       d,
		auth:    authSvc,
		user:    user.New(d.DB, d.Redis, d.Now, d.Logger),
		keys:    keys,
		models:  model.New(d.DB, d.Now, d.Logger),
		fund:    funds,
		ops:     opss,
		tickets: tickets,
		reviews: reviews,
		logs:    logs,
		usage:   usageSvc,
		auditQ:  auditSvc,
		pub:     publicapi.New(d.DB, d.Now, d.Logger),
		console: consoleSvc,
	}, nil
}

// Register installs every control-plane route. Each entry names its permission
// in one place; adding a route without a permission is a compile-time omission
// here, not a runtime surprise.
func (s *Server) Register(mux *http.ServeMux) {
	// Public: unauthenticated by design.
	mux.Handle("POST /api/v1/auth/register", s.public(s.auth.Register))
	mux.Handle("POST /api/v1/auth/verification-code", s.public(s.auth.SendVerificationCode))
	mux.Handle("POST /api/v1/auth/login", s.public(s.auth.Login))
	// 自助找回密码：匿名，且**故意不校验 CSRF**（public 使其结构豁免）。
	// 跨站伪造它能达成什么：伪造受害者的重置要拿到受害者 6 位验证码（拿不到）；
	// 用受害者浏览器重置攻击者自己的账号，结果只是攻击者自己的口令变了。
	// 加一条「有会话就校验」的分支只会让「已登录用户在公共设备上重置」出现 403 死路。
	// 口径同时写在 docs/control-api.md 的 CSRF 豁免表里。
	mux.Handle("POST /api/v1/auth/password/reset", s.public(s.auth.ResetPassword))

	// Website data. Read-only and session-free: the marketing pages are
	// anonymous, so these routes must never enter the Session → CSRF chain.
	// Cache-Control: public, max-age=60 is set by the handlers themselves.
	mux.Handle("GET /api/public/plans", s.public(s.pub.Plans))
	mux.Handle("GET /api/public/models", s.public(s.pub.Models))
	mux.Handle("GET /api/public/announcements", s.public(s.pub.Announcements))
	mux.Handle("GET /api/public/endpoints", s.public(s.pub.Endpoints))
	mux.Handle("GET /api/public/integration-links", s.public(s.pub.IntegrationLinks))

	// Authenticated, no capability required.
	mux.Handle("GET /api/v1/auth/me", s.authed("", s.auth.Me))
	mux.Handle("POST /api/v1/auth/logout", s.authed("", s.auth.Logout))
	// Password change verifies the old password, then revokes every sid the
	// account owns (K30 → K20). No capability: every signed-in account may
	// change its own credential.
	mux.Handle("POST /api/v1/auth/password", s.authed("", s.auth.ChangePassword))
	// 昵称自助修改。用 account.write 而不是空权限：它是「改自己的资料」，与
	// 运营侧 user.write（可改任意账号状态）严格分开。
	mux.Handle("PATCH /api/v1/auth/profile", s.authed(mw.PermAccountWrite, s.auth.UpdateProfile))
	// 绑定登录邮箱。两个接口都挂 account.write + CSRF：这是「改自己的资料」，
	// 与运营侧的 user.write（可改任意账号）严格分开。不塞进 PATCH /auth/profile：
	// nickname 是必填的未验证字段，混进「已验码的登录标识写入」会让一次请求同时
	// 有未验证与已验证两种语义，失败时（昵称已存、邮箱 409）无法定义整体结果。
	// 目标账号一律取会话 uid，不接受任何 user_id 参数。
	mux.Handle("POST /api/v1/auth/email/code", s.authed(mw.PermAccountWrite, s.auth.SendEmailBindCode))
	mux.Handle("POST /api/v1/auth/email", s.authed(mw.PermAccountWrite, s.auth.BindEmail))
	// Self-service cancellation: open tickets block it, then status=DISABLED
	// and every session key are cleared. No capability beyond being signed in.
	mux.Handle("POST /api/v1/auth/cancel", s.authed("", s.auth.Cancel))

	// Console account. account.read, not user.read: this handler scopes every
	// aggregate by the session uid, while user.read is the operator-wide
	// directory read and must never be handed to a customer (E-31/E-33).
	mux.Handle("GET /api/console/overview", s.authed(mw.PermAccountRead, s.user.Overview))

	// Console keys: reads need key.read, mutations need key.write.
	mux.Handle("GET /api/console/keys", s.authed(mw.PermKeyRead, s.keys.List))
	mux.Handle("POST /api/console/keys", s.authed(mw.PermKeyWrite, s.keys.Create))
	mux.Handle("GET /api/console/keys/{id}", s.authed(mw.PermKeyRead, s.keys.Detail))
	mux.Handle("PATCH /api/console/keys/{id}", s.authed(mw.PermKeyWrite, s.keys.Update))
	mux.Handle("DELETE /api/console/keys/{id}", s.authed(mw.PermKeyWrite, s.keys.Delete))
	mux.Handle("GET /api/console/keys/{id}/audit", s.authed(mw.PermKeyRead, s.keys.ListAudit))
	// 密钥维度的用量趋势：详情页判断「这把密钥还用不用」的依据。
	mux.Handle("GET /api/console/keys/{id}/usage", s.authed(mw.PermKeyRead, s.keys.Usage))
	mux.Handle("POST /api/console/keys/{id}/rotate", s.authed(mw.PermKeyWrite, s.keys.Rotate))
	// T27 playground. key.read is enough: the handler proves the key belongs to
	// the session and is ENABLED, then relays to gatewayd where P06..P14 run.
	// The CSRF check comes from s.authed like every other POST.
	mux.Handle("POST /api/console/playground/chat", s.authed(mw.PermKeyRead, s.console.Chat))

	// Console model catalog.
	mux.Handle("GET /api/console/models", s.authed(mw.PermModelRead, s.models.List))

	// Console fund: plans (read), orders (read/write), redeem (write).
	mux.Handle("GET /api/console/plans", s.authed(mw.PermFundRead, s.fund.Plans))
	mux.Handle("GET /api/console/orders", s.authed(mw.PermFundRead, s.fund.ListOrders))
	mux.Handle("POST /api/console/orders", s.authed(mw.PermFundWrite, s.fund.CreateOrder))
	mux.Handle("POST /api/console/redeem", s.authed(mw.PermFundWrite, s.fund.Redeem))

	// Console fund: refunds, invoices, quota applies and the trial claim.
	// Approval/payment transitions are exported methods for T16; the console
	// only submits and lists.
	mux.Handle("GET /api/console/refunds", s.authed(mw.PermFundRead, s.fund.ListRefunds))
	mux.Handle("POST /api/console/refunds", s.authed(mw.PermFundWrite, s.fund.CreateRefund))
	mux.Handle("GET /api/console/invoices", s.authed(mw.PermFundRead, s.fund.ListInvoices))
	mux.Handle("POST /api/console/invoices", s.authed(mw.PermFundWrite, s.fund.CreateInvoice))
	mux.Handle("GET /api/console/quota-applies", s.authed(mw.PermFundRead, s.fund.ListQuotaApplies))
	mux.Handle("POST /api/console/quota-applies", s.authed(mw.PermFundWrite, s.fund.CreateQuotaApply))
	mux.Handle("POST /api/console/trial/claim", s.authed(mw.PermFundWrite, s.fund.ClaimTrial))

	// Payment callback: public by design. The payment platform has no console
	// session; the HMAC signature over the body is the authentication, so this
	// route must never be wired through s.authed (no cookie, no sid).
	mux.Handle("POST /api/v1/pay/callback/{channel}", s.public(s.fund.Callback))

	// Admin: whoami stays behind admin.all. Business admin routes declare the
	// concrete permission instead: OPS may write channels/models, FINANCE may
	// read models, and neither gets admin.all.
	mux.Handle("GET /api/admin/whoami", s.authed(mw.PermAdminAll, s.handleAdminWhoami))

	// T11 second segment: audit trail reads. audit.read is held by OPS and
	// FINANCE; PERSONAL has no route and no permission here.
	mux.Handle("GET /api/admin/audit-logs", s.authed(mw.PermAuditRead, s.auditQ.List))

	// T24 overview: one read-only aggregate over the whole platform. admin.read
	// is the right scope here and deliberately stays shared: OPS, FINANCE and
	// SUPER_ADMIN all need it, and it carries no per-account or per-channel
	// detail. The per-resource reads below are narrower (user.read /
	// channel.read) precisely so that "may see the aggregate" does not imply
	// "may see every account".
	mux.Handle("GET /api/admin/overview", s.authed(mw.PermAdminRead, s.ops.Overview))

	// Operator channels. Reads use channel.read, not model.read and not
	// admin.read: PERSONAL holds model.read for the console catalog, and these
	// views expose base_url and the key mask. Mutations need channel.write.
	mux.Handle("GET /api/admin/channels", s.authed(mw.PermChannelRead, s.ops.ListChannels))
	mux.Handle("GET /api/admin/channels/{id}", s.authed(mw.PermChannelRead, s.ops.GetChannel))
	mux.Handle("POST /api/admin/channels", s.authed(mw.PermChannelWrite, s.ops.CreateChannel))
	mux.Handle("PATCH /api/admin/channels/{id}", s.authed(mw.PermChannelWrite, s.ops.UpdateChannel))
	mux.Handle("POST /api/admin/channels/{id}/disable", s.authed(mw.PermChannelWrite, s.ops.DisableChannel))
	mux.Handle("DELETE /api/admin/channels/{id}", s.authed(mw.PermChannelWrite, s.ops.DeleteChannel))

	// T40-c: manual upstream bill import (OI-15, MANUAL_UPLOAD). channel.write is
	// the narrowest existing capability that covers writing a channel's billing
	// data; FINANCE holds audit.read but not channel.write, and no read-only
	// permission would be enough for a write.
	mux.Handle("POST /api/admin/upstream-bills/import", s.authed(mw.PermChannelWrite, s.ops.ImportUpstreamBills))

	// Operator model master data. Reads keep admin.read because this view
	// includes upstream cost prices; model.read stays the console catalog's
	// permission. FINANCE legitimately reads this view (its pricing page picks
	// models by name), and admin.read is the narrowest permission that covers
	// "operator may see the cost sheet" without also granting the account
	// directory (user.read) or the upstream channels (channel.read).
	mux.Handle("GET /api/admin/models", s.authed(mw.PermAdminRead, s.ops.ListModels))
	mux.Handle("GET /api/admin/models/{id}", s.authed(mw.PermAdminRead, s.ops.GetModel))
	mux.Handle("POST /api/admin/models", s.authed(mw.PermModelWrite, s.ops.CreateModel))
	mux.Handle("PATCH /api/admin/models/{id}", s.authed(mw.PermModelWrite, s.ops.UpdateModel))
	mux.Handle("POST /api/admin/models/{id}/listing-state", s.authed(mw.PermModelWrite, s.ops.SetListingState))
	mux.Handle("POST /api/admin/models/{id}/recommend-weight", s.authed(mw.PermModelWrite, s.ops.SetRecommendWeight))
	// Aliases are part of model master data (F22-2). Reads follow the admin
	// read permission; mutations require model.write.
	mux.Handle("GET /api/admin/models/{id}/aliases", s.authed(mw.PermAdminRead, s.ops.ListAliases))
	mux.Handle("POST /api/admin/models/{id}/aliases", s.authed(mw.PermModelWrite, s.ops.CreateAlias))
	mux.Handle("DELETE /api/admin/models/{id}/aliases/{alias}", s.authed(mw.PermModelWrite, s.ops.DeleteAlias))

	// T15 second segment: pricing, groups, announcements, risk, users and the
	// runtime config whitelist. Reads of /api/admin/users use user.read, the
	// operator-wide directory scope: the list returns every account's status,
	// plan and disable reason, so no customer role may hold it. (The console's
	// own overview is account.read — see the /api/console/overview route
	// above.) FINANCE deliberately does not hold user.read: orders, invoices
	// and redeem batches are its surface, and none of them needs the account
	// directory.
	mux.Handle("GET /api/admin/prices", s.authed(mw.PermPriceWrite, s.ops.ListPrices))
	mux.Handle("POST /api/admin/prices", s.authed(mw.PermPriceWrite, s.ops.CreatePrice))
	mux.Handle("PATCH /api/admin/prices/{id}", s.authed(mw.PermPriceWrite, s.ops.UpdatePrice))

	mux.Handle("GET /api/admin/groups", s.authed(mw.PermGroupWrite, s.ops.ListGroups))
	mux.Handle("PATCH /api/admin/groups/{id}", s.authed(mw.PermGroupWrite, s.ops.UpdateGroup))

	mux.Handle("GET /api/admin/announcements", s.authed(mw.PermNoticeWrite, s.ops.ListAnnouncements))
	mux.Handle("POST /api/admin/announcements", s.authed(mw.PermNoticeWrite, s.ops.CreateAnnouncement))
	mux.Handle("PATCH /api/admin/announcements/{id}", s.authed(mw.PermNoticeWrite, s.ops.UpdateAnnouncement))
	mux.Handle("POST /api/admin/announcements/{id}/publish", s.authed(mw.PermNoticeWrite, s.ops.PublishAnnouncement))
	mux.Handle("POST /api/admin/announcements/{id}/offline", s.authed(mw.PermNoticeWrite, s.ops.OfflineAnnouncement))

	mux.Handle("GET /api/admin/risk/rules", s.authed(mw.PermRiskWrite, s.ops.ListRiskRules))
	mux.Handle("POST /api/admin/risk/rules", s.authed(mw.PermRiskWrite, s.ops.CreateRiskRule))
	mux.Handle("PATCH /api/admin/risk/rules/{id}", s.authed(mw.PermRiskWrite, s.ops.UpdateRiskRule))
	mux.Handle("DELETE /api/admin/risk/rules/{id}", s.authed(mw.PermRiskWrite, s.ops.DeleteRiskRule))
	mux.Handle("GET /api/admin/risk/hits", s.authed(mw.PermRiskWrite, s.ops.ListRiskHits))
	mux.Handle("POST /api/admin/risk/users/{id}/score", s.authed(mw.PermRiskWrite, s.ops.SetUserRiskScore))

	mux.Handle("GET /api/admin/users", s.authed(mw.PermUserRead, s.ops.ListUsers))
	mux.Handle("GET /api/admin/users/{id}", s.authed(mw.PermUserRead, s.ops.GetUser))
	mux.Handle("POST /api/admin/users/{id}/disable", s.authed(mw.PermUserWrite, s.ops.DisableUser))
	mux.Handle("POST /api/admin/users/{id}/enable", s.authed(mw.PermUserWrite, s.ops.EnableUser))
	// Operator account provisioning. Both routes are gated by admin.all — the
	// one permission only SUPER_ADMIN holds — and deliberately NOT by user.write.
	//
	// user.write is OPS's permission. Creating an account is not "editing an
	// account": POST /api/admin/users takes a role from the body, so a caller
	// who may create could create a SUPER_ADMIN and then sign in as it, and
	// POST /api/admin/users/{id}/role could promote any account (its own
	// included) the same way. Sharing user.write here would turn "may disable a
	// customer" into "may mint a super admin" — a privilege escalation with two
	// requests. The rest of the user surface keeps user.write; only who may
	// decide *what an account is* moves up to admin.all.
	mux.Handle("POST /api/admin/users", s.authed(mw.PermAdminAll, s.ops.CreateOperatorUser))
	mux.Handle("POST /api/admin/users/{id}/role", s.authed(mw.PermAdminAll, s.ops.ChangeUserRole))
	// T24 balance adjustment: user.write submits, fund.approve decides. The
	// document never changes money on submission; approval posts a ledger entry.
	mux.Handle("POST /api/admin/users/{id}/balance-adjust", s.authed(mw.PermUserWrite, s.reviews.SubmitBalanceAdjust))

	mux.Handle("GET /api/admin/config", s.authed(mw.PermConfigWrite, s.ops.GetConfig))
	mux.Handle("PUT /api/admin/config", s.authed(mw.PermConfigWrite, s.ops.PutConfig))

	// T33 integration deep links. The five rows are seeded by 0026 and fixed
	// (D-30 Q3): PATCH addresses an existing tool_key and cannot create a
	// sixth. config.write already owns operator-level settings, so no new
	// permission constant is introduced.
	mux.Handle("GET /api/admin/integrations", s.authed(mw.PermConfigWrite, s.ops.ListIntegrations))
	mux.Handle("PATCH /api/admin/integrations/{toolKey}", s.authed(mw.PermConfigWrite, s.ops.UpdateIntegration))

	// T16 ticket centre. The account console holds ticket.read/ticket.write and
	// sees only its own rows; the operator console answers through ticket.reply.
	// OPS deliberately does not hold ticket.read: the two consoles never share
	// a permission (E-31/E-33).
	mux.Handle("POST /api/console/tickets", s.authed(mw.PermTicketWrite, s.tickets.Create))
	mux.Handle("GET /api/console/tickets", s.authed(mw.PermTicketRead, s.tickets.List))
	mux.Handle("GET /api/console/tickets/{id}", s.authed(mw.PermTicketRead, s.tickets.Detail))
	mux.Handle("POST /api/console/tickets/{id}/close", s.authed(mw.PermTicketWrite, s.tickets.Close))
	mux.Handle("GET /api/console/request-ids", s.authed(mw.PermTicketRead, s.tickets.Picker))

	mux.Handle("GET /api/admin/tickets", s.authed(mw.PermTicketReply, s.tickets.ListAdmin))
	// T24 operator detail: any user's ticket, with description and replies
	// (including internal notes). The account console Detail stays own-only.
	mux.Handle("GET /api/admin/tickets/{id}", s.authed(mw.PermTicketReply, s.tickets.DetailAdmin))
	mux.Handle("POST /api/admin/tickets/{id}/reply", s.authed(mw.PermTicketReply, s.tickets.Reply))
	mux.Handle("POST /api/admin/tickets/{id}/state", s.authed(mw.PermTicketReply, s.tickets.State))

	// T16 review centre. fund.approve is the money-approval capability: OPS
	// may edit prices but deliberately cannot approve a refund, and PERSONAL
	// holds fund.write only for its own purchases. There is no batch endpoint
	// (V1.0): one document, one reason, one decision.
	mux.Handle("GET /api/admin/reviews", s.authed(mw.PermFundApprove, s.reviews.List))
	mux.Handle("POST /api/admin/reviews/{id}/approve", s.authed(mw.PermFundApprove, s.reviews.Approve))
	mux.Handle("POST /api/admin/reviews/{id}/reject", s.authed(mw.PermFundApprove, s.reviews.Reject))

	// Operator fund surface: the human half of the recharge loop (B-46 A 档)
	// and redeem-code issuance (B-44 A 档).
	//
	// The reads are gated by fund.approve, not fund.read, and deliberately so:
	// fund.read is account-scoped in the T15 matrix (PERSONAL holds it for
	// /api/console/orders, where every handler filters by session uid). Sharing
	// it here would let any customer list every account's orders and every
	// redeem batch (the E-31/E-33 pattern). GET /api/admin/prices is gated the
	// same way for the same reason: an operator-wide read names its narrowest
	// holder, and only FINANCE and SUPER_ADMIN hold fund.approve. OPS holds
	// neither permission, so an operations account can neither read the money
	// queue nor record a payment.
	mux.Handle("GET /api/admin/orders", s.authed(mw.PermFundApprove, s.fund.ListAdminOrders))
	mux.Handle("POST /api/admin/orders/{id}/confirm-payment", s.authed(mw.PermFundApprove, s.fund.ConfirmOrderPayment))
	mux.Handle("POST /api/admin/orders/{id}/close", s.authed(mw.PermFundApprove, s.fund.CloseOrder))
	mux.Handle("GET /api/admin/redeem-codes", s.authed(mw.PermFundApprove, s.fund.ListRedeemBatches))
	mux.Handle("POST /api/admin/redeem-codes/batch", s.authed(mw.PermFundApprove, s.fund.CreateRedeemBatch))

	// T14 request logs. Read-only by design: PRD §10.2 requires retention, so
	// there is no delete route. The list defaults to seven days and refuses a
	// from older than 365 days with "use archive export"; the body endpoint
	// applies its own 180-day window.
	mux.Handle("GET /api/console/logs", s.authed(mw.PermLogRead, s.logs.List))
	mux.Handle("GET /api/console/logs/{request_id}", s.authed(mw.PermLogRead, s.logs.Detail))
	mux.Handle("GET /api/console/logs/{request_id}/body", s.authed(mw.PermLogRead, s.logs.Body))

	// T14 second segment: usage, monthly bill, live-vs-settled metrics and the
	// asynchronous CSV export. usage.read is account-scoped like log.read; the
	// export download additionally verifies an HMAC token whose uid must equal
	// the session uid.
	mux.Handle("GET /api/console/usage", s.authed(mw.PermUsageRead, s.usage.Query))
	mux.Handle("GET /api/console/bills", s.authed(mw.PermUsageRead, s.usage.Bill))
	mux.Handle("GET /api/console/usage/metrics", s.authed(mw.PermUsageRead, s.usage.Metrics))
	mux.Handle("POST /api/console/usage/export", s.authed(mw.PermUsageRead, s.usage.StartExport))
	mux.Handle("GET /api/console/exports/{token}", s.authed(mw.PermUsageRead, s.usage.ExportToken))

	// Unknown /api paths return the shared error envelope instead of net/http's
	// plain-text 404.
	mux.Handle("/api/", s.public(func(w http.ResponseWriter, r *http.Request) {
		mw.WriteError(w, r, apierr.New(apierr.CodeUnknownEndpoint))
	}))
}

// public wraps a handler with panic recovery only.
func (s *Server) public(h http.HandlerFunc) http.Handler {
	return mw.Recover(s.d.Logger)(h)
}

// authed builds Recover → Session → CSRF → Require(perm) → handler. An empty
// perm is the "logged in, no capability" case (me/logout/password).
//
// CSRF sits inside the session middleware because the expected token is
// derived from the loaded sid. Public routes (login/register/payment callback)
// never enter this chain, so their exemption is structural rather than a list
// that a future route can forget to join.
func (s *Server) authed(perm mw.Permission, h http.HandlerFunc) http.Handler {
	var chain http.Handler = h
	if perm != "" {
		chain = mw.Require(perm)(chain)
	}
	chain = s.d.Sessions.CSRF(chain)
	chain = s.d.Sessions.Middleware()(chain)
	return mw.Recover(s.d.Logger)(chain)
}

// ---------------------------------------------------------------------------
// GET /api/admin/whoami — admin.all, read-only diagnostic.
// ---------------------------------------------------------------------------

type whoamiResponse struct {
	UID         string          `json:"uid"`
	Role        string          `json:"role"`
	Permissions []mw.Permission `json:"permissions"`
}

func (s *Server) handleAdminWhoami(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, whoamiResponse{
		UID:         strconv.FormatInt(sess.UID, 10),
		Role:        sess.Role,
		Permissions: mw.PermissionsFor(sess.Role),
	})
}
