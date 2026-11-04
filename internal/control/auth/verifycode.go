package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/redisx"
)

// Verification-code policy. These are package constants, not t_config_item
// keys: the 45-key table is frozen, and a per-deployment knob here would let a
// compromised deployment widen its own brute-force budget.
const (
	verificationCodeTTL     = 10 * time.Minute
	verificationResendAfter = 60 * time.Second
	verificationDailyLimit  = 20
	verificationMaxAttempts = 5
	verificationCodeDigits  = 6
	// Per source address, per hour. A normal user does not ask for ten codes in
	// an hour; a script that rotates recipient addresses does.
	verificationIPHourlyLimit = 10

	// Password recovery is a low-frequency action, so its per-address daily
	// budget is half the login/register one: enabling it must not raise a
	// single address's daily sending ceiling from 40 to 60.
	resetDailyLimit = 10
	// Binding a login mailbox is likewise low-frequency. The address budget
	// bounds "how many codes may be sent to this mailbox"; the per-account
	// budget below bounds "how many mailboxes one signed-in account may probe".
	bindDailyLimit = 10
)

// Verification modes for AUTH_VERIFICATION_MODE.
//
//	off   no code is required anywhere; the send endpoint answers 400 and the
//	      register/login paths behave exactly as they did before this feature
//	log   the code is generated and stored, and printed to the process log
//	      instead of being e-mailed. Development and CI only.
//	smtp  the code is e-mailed through the configured SMTP relay.
const (
	VerificationOff  = "off"
	VerificationLog  = "log"
	VerificationSMTP = "smtp"
)

// SMS modes for AUTH_SMS_MODE. It is deliberately a second variable: e-mail and
// text messages go through different suppliers and cannot be switched on the
// same day. The default is "off", the opposite of AUTH_VERIFICATION_MODE's
// default, because every message costs money.
const (
	SMSOff      = "off"
	SMSLog      = "log"
	SMSProvider = "provider"
)

// Verification purposes. They are separate Redis namespaces so a code obtained
// for one flow cannot be replayed into another:
//
//	login    sign in without a password
//	register create an account
//	reset    reset a forgotten password
//	bind     prove ownership of a mailbox before writing it to t_user.email
//
// The bind/reset pair is the one that matters most: sharing a namespace would
// let an attacker bind a target address to their own account, then replay the
// same unused code to reset that account's password.
const (
	PurposeLogin    = "login"
	PurposeRegister = "register"
	PurposeReset    = "reset"
	PurposeBind     = "bind"
)

// Sender is the outbound e-mail seam. It matches fund.Mailer and ticket.Mailer
// method-for-method so control.Deps can inject one implementation for all
// three (internal/notify/mail).
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// SMSSender is the outbound text-message seam.
//
// It is declared here, on the consumer side, exactly like Sender — and it has
// no subject on purpose. Filling a subject with "" so both seams look alike
// would make every call site read Send(ctx, phone, "", text), and a reviewer
// could no longer tell which argument the code went into.
type SMSSender interface {
	Send(ctx context.Context, to, body string) error
}

// smsUnavailableHint is what a caller gets when the handset channel is off. It
// names the next action instead of only the refusal: the account may well have
// a mailbox, and binding one is the only self-service way out.
const smsUnavailableHint = "短信验证码暂未开放，请使用邮箱；若账号未绑定邮箱，请登录后在「账户设置」绑定"

// verificationRequest is the send-code body. purpose is explicit rather than
// inferred: the same address needs a different code to register than to log
// in, and guessing the intent from "does the account exist" would let the
// endpoint be used to enumerate accounts.
type verificationRequest struct {
	Identifier string `json:"identifier"`
	Purpose    string `json:"purpose"`
}

type verificationResponse struct {
	Channel     string `json:"channel"`
	ExpiresIn   int    `json:"expires_in"`
	ResendAfter int    `json:"resend_after"`
}

// verificationMode resolves the configured mode, defaulting to "log".
//
// The default is deliberately not "off": this feature exists because a code is
// required at registration, and a silent no-op default would ship an endpoint
// that answers 200 without ever sending anything. "log" is loud (the code lands
// in the process log and the daemon prints a startup warning) and requires no
// credentials, so a fresh checkout can complete the flow end to end.
func (s *Service) verificationMode() string {
	switch strings.ToLower(strings.TrimSpace(s.VerificationMode)) {
	case VerificationOff:
		return VerificationOff
	case VerificationSMTP:
		return VerificationSMTP
	default:
		return VerificationLog
	}
}

func (s *Service) verificationRequired() bool { return s.verificationMode() != VerificationOff }

// smsMode resolves AUTH_SMS_MODE, defaulting to "off". Unknown values also mean
// "off": cmd/controld refuses to start on an unknown value, so reaching this
// branch means the value was built in code, and failing closed is the only safe
// reading of "somebody typed something I do not understand" for a billed channel.
func (s *Service) smsMode() string {
	switch strings.ToLower(strings.TrimSpace(s.SMSMode)) {
	case SMSLog:
		return SMSLog
	case SMSProvider:
		return SMSProvider
	default:
		return SMSOff
	}
}

// channelAvailable reports whether the identifier's delivery channel is open.
//
// E-mail is always "available" at this point — whether a relay exists is a
// delivery question answered with 503, not a request question answered with
// 400; the endpoint must not tell an anonymous caller anything about our SMTP
// configuration. The handset channel is different: it is a product decision
// (billed per message, supplier not signed), so refusing early with a next step
// is more useful than a 503 that looks like an outage.
func (s *Service) channelAvailable(kind string) bool {
	if kind == KindPhone {
		return s.smsMode() != SMSOff
	}
	return true
}

// channelName is the wire name of a delivery channel. It is not identical to
// Identifier.Kind ("phone" is reported as "sms") because the response describes
// how the code travelled, not which column it was looked up in.
func channelName(kind string) string {
	if kind == KindPhone {
		return "sms"
	}
	return "email"
}

// SendVerificationCode implements POST /api/v1/auth/verification-code.
//
// Response shape is the same whether or not the account exists: the endpoint
// must not become an account oracle. Two deliberate orderings implement that:
//
//   - The rate limits run before the account lookup, so a query that is slower
//     for existing accounts cannot be timed, and probing still costs budget.
//   - The code is stored before the account is consulted, and dropped again when
//     the account turns out to be unusable, so a code minted by a probe can
//     never become a working reset credential if the address is registered
//     later.
func (s *Service) SendVerificationCode(w http.ResponseWriter, r *http.Request) {
	var req verificationRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	purpose := strings.ToLower(strings.TrimSpace(req.Purpose))
	// "bind" is not accepted here by design: binding a mailbox must be done by a
	// signed-in caller (POST /api/v1/auth/email/code). Accepting it on the
	// anonymous endpoint would turn this into "send a code to any address".
	if purpose != PurposeLogin && purpose != PurposeRegister && purpose != PurposeReset {
		e := mw.InvalidField("purpose")
		e = e.WithHint("purpose 只能是 login、register 或 reset")
		mw.WriteError(w, r, e)
		return
	}
	ident, err := NormalizeIdentifier(req.Identifier)
	if err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier"))
		return
	}
	// A username has no mailbox and no handset: there is no address to deliver a
	// code to, so this is a malformed request rather than an unavailable
	// channel. Answered before the mode check so the caller learns the real
	// reason instead of "verification is not enabled".
	if err := RequireEmailOrPhone(ident); err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier").
			WithHint("用户名仅用于运营账号，请使用邮箱或手机号"))
		return
	}
	if s.verificationMode() == VerificationOff {
		e := mw.InvalidField("purpose")
		e = e.WithHint("本部署未启用验证码，请直接使用密码登录或注册")
		mw.WriteError(w, r, e)
		return
	}
	// SMS is billed per message and no supplier is signed yet, so the handset
	// channel is a separate switch (AUTH_SMS_MODE). Answering an explicit "not
	// available, here is what to do instead" is the point — silently doing
	// nothing would look like a delivery failure.
	if !s.channelAvailable(ident.Kind) {
		mw.WriteError(w, r, mw.InvalidField("identifier").WithHint(smsUnavailableHint))
		return
	}
	// "register" for an address that already has an account is the only
	// distinguishable case, and deliberately so: it is already public knowledge
	// from POST /register (409 account_exists), and telling the user early is
	// better than letting them fill the whole form.
	if purpose == PurposeRegister {
		if _, findErr := s.findUser(r.Context(), ident); findErr == nil {
			mw.WriteError(w, r, apierr.New(apierr.CodeAccountExists).
				WithHint("该邮箱已注册，请直接登录"))
			return
		}
	}

	code, apiErr := s.issueVerificationCode(r.Context(), ident, purpose, clientIP(r))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp := verificationResponse{
		Channel:     channelName(ident.Kind),
		ExpiresIn:   int(verificationCodeTTL / time.Second),
		ResendAfter: int(verificationResendAfter / time.Second),
	}

	// Password recovery is the one purpose whose answer must be byte-identical
	// for a usable and an unusable account. An account is unusable when the
	// address is unknown, soft-deleted, not NORMAL, or matches more than one row
	// (t_user.phone has no unique key). None of that is reported: the code is
	// dropped and the same 200 body goes out, with no message sent.
	if purpose == PurposeReset {
		if _, findErr := s.findResetAccount(r.Context(), ident); findErr != nil {
			if !isAccountUnavailable(findErr) && !isNotFound(findErr) {
				s.errorLog(r.Context(), "verification_reset_lookup_failed: "+findErr.Error())
			}
			s.dropVerificationCode(r.Context(), ident, purpose)
			_ = mw.WriteJSON(w, http.StatusOK, resp)
			return
		}
	}

	if apiErr := s.deliverCode(r.Context(), ident, purpose, code); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, resp)
}

// deliverCode sends one issued code over the identifier's channel and maps any
// failure to 503 auth_unavailable with a channel-specific hint.
//
// On failure the stored code is dropped. A code that was stored but never
// delivered is unusable by the user and would additionally block their next
// attempt behind the 60s cooldown, so the state after a failed send must be
// "nothing happened" rather than "nothing arrived but the code exists".
func (s *Service) deliverCode(ctx context.Context, ident Identifier, purpose, code string) *apierr.APIError {
	if ident.Kind == KindPhone {
		if s.smsMode() == SMSOff {
			// Unreachable: channelAvailable already refused. Kept as the last
			// gate for a caller that skips that check.
			return apierr.New(apierr.CodeAuthUnavailable).WithHint(smsUnavailableHint)
		}
		if s.SMSSender == nil {
			s.errorLog(ctx, "verification_sms_unconfigured: SMS mode without a sender")
			return apierr.New(apierr.CodeAuthUnavailable).WithHint("短信通道未配置，请联系管理员")
		}
		if err := s.SMSSender.Send(ctx, ident.Value, verificationSMSBody(code)); err != nil {
			s.dropVerificationCode(ctx, ident, purpose)
			// The driver never puts the code or the full number in its error.
			s.errorLog(ctx, "verification_sms_failed: "+err.Error())
			return apierr.New(apierr.CodeAuthUnavailable).WithHint("验证码短信发送失败，请稍后重试")
		}
		return nil
	}

	if s.verificationMode() == VerificationLog {
		// log mode: the operator reads the code from the process log. It is
		// printed as the message text, not as a log field, so it cannot leak
		// into a structured sink that ships off-box by accident. This is the
		// ONLY place in the codebase where a code may reach a log.
		s.warn(ctx, fmt.Sprintf("verification_code_log_mode purpose=%s identifier=%s code=%s",
			purpose, maskIdentifier(ident.Kind, ident.Value), code))
		return nil
	}
	if s.Mailer == nil {
		s.errorLog(ctx, "verification_mail_unconfigured: SMTP mode without a mailer")
		return apierr.New(apierr.CodeAuthUnavailable).WithHint("邮件通道未配置，请联系管理员")
	}
	subject, body := verificationMail(code, purpose)
	if err := s.Mailer.Send(ctx, ident.Value, subject, body); err != nil {
		s.dropVerificationCode(ctx, ident, purpose)
		s.errorLog(ctx, "verification_mail_failed: "+err.Error())
		return apierr.New(apierr.CodeAuthUnavailable).WithHint("验证码邮件发送失败，请稍后重试")
	}
	return nil
}

// dailyLimitFor returns the per-address daily sending budget of one purpose.
// Unknown purposes get the ordinary budget; the switch is a closed set so a new
// purpose cannot silently inherit the loosest value.
func dailyLimitFor(purpose string) int {
	switch purpose {
	case PurposeReset:
		return resetDailyLimit
	case PurposeBind:
		return bindDailyLimit
	default:
		return verificationDailyLimit
	}
}

// issueVerificationCode enforces the per-IP hourly ceiling, the per-mailbox
// cooldown and the daily quota, then stores the code. The code itself is stored
// as a SHA-256 digest: a Redis dump must not be a list of working credentials.
//
// Order matters: the IP ceiling runs first. The two per-mailbox limits are
// keyed by address, so a script that walks a list of recipients gets a fresh
// cooldown and a fresh daily quota for every one of them — it would drain our
// sending quota and damage the sending domain's reputation long before any
// per-address limit fired.
//
// The IP counter is deliberately NOT split by purpose: adding three new
// purposes must not raise a single address's hourly sending ceiling from 10 to
// 40. That invariant is the reason this function has one shape for every
// purpose.
func (s *Service) issueVerificationCode(ctx context.Context, ident Identifier, purpose, ip string) (string, *apierr.APIError) {
	rdb, err := s.verifyRedis()
	if err != nil {
		s.errorLog(ctx, "verification_redis_unavailable: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
	}
	now := s.now().Now().UTC()
	if ip != "" {
		ipKey := redisx.VerifyCodeIPQuotaKey(identifierHash(ip), now.Format("2006010215"))
		n, incrErr := rdb.Raw().Incr(ctx, ipKey).Result()
		if incrErr != nil {
			s.errorLog(ctx, "verification_ip_quota_failed: "+incrErr.Error())
			return "", apierr.New(apierr.CodeAuthUnavailable)
		}
		if n == 1 {
			// Two hours so the key outlives its window even if the first hit
			// lands at :59; the key is per hour anyway, so extra TTL is free.
			_ = rdb.Raw().Expire(ctx, ipKey, 2*time.Hour).Err()
		}
		if n > verificationIPHourlyLimit {
			retry := int(time.Hour/time.Second) - now.Minute()*60 - now.Second()
			if retry <= 0 {
				retry = 60
			}
			return "", apierr.Newf(apierr.CodeRPMLimitExceeded, verificationIPHourlyLimit, retry).
				WithHint("同一网络地址每小时最多获取 10 次验证码，请稍后再试")
		}
	}
	hash := identifierHash(ident.Value)
	coolKey := redisx.VerifyCodeCooldownKey(purpose, hash)
	quotaKey := redisx.VerifyCodeQuotaKey(purpose, hash)

	// Cooldown: SETNX keeps this atomic, so two concurrent clicks cannot both
	// pass the check and send two messages.
	ok, err := rdb.Raw().SetNX(ctx, coolKey, "1", verificationResendAfter).Result()
	if err != nil {
		s.errorLog(ctx, "verification_cooldown_failed: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
	}
	if !ok {
		return "", apierr.Newf(apierr.CodeRPMLimitExceeded, 1, int(verificationResendAfter/time.Second))
	}

	// Daily quota. INCR then EXPIRE (not SET with EX on first increment) so the
	// window does not slide on every send: the budget is per rolling day, not a
	// rate.
	limit := dailyLimitFor(purpose)
	n, err := rdb.Raw().Incr(ctx, quotaKey).Result()
	if err != nil {
		s.errorLog(ctx, "verification_quota_failed: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
	}
	if n == 1 {
		_ = rdb.Raw().Expire(ctx, quotaKey, 24*time.Hour).Err()
	}
	if n > int64(limit) {
		return "", apierr.Newf(apierr.CodeRPMLimitExceeded, limit, int((24 * time.Hour).Seconds()))
	}

	code, err := randomDigits(verificationCodeDigits)
	if err != nil {
		s.errorLog(ctx, "verification_code_generate_failed: "+err.Error())
		return "", apierr.New(apierr.CodeInternalError)
	}
	if err := rdb.Raw().Set(ctx, redisx.VerifyCodeKey(purpose, hash), codeDigest(code), verificationCodeTTL).Err(); err != nil {
		s.errorLog(ctx, "verification_code_store_failed: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
	}
	return code, nil
}

// consumeVerificationCode validates and burns a code. Every failure mode maps
// to invalid_credentials with a hint: distinguishing "expired" from "wrong"
// from "no code was ever requested" tells an attacker which addresses have
// pending codes.
func (s *Service) consumeVerificationCode(ctx context.Context, ident Identifier, purpose, code string) *apierr.APIError {
	rdb, err := s.verifyRedis()
	if err != nil {
		s.errorLog(ctx, "verification_redis_unavailable: "+err.Error())
		return apierr.New(apierr.CodeAuthUnavailable)
	}
	hash := identifierHash(ident.Value)
	codeKey := redisx.VerifyCodeKey(purpose, hash)
	attemptsKey := codeKey + ":n"

	stored, err := rdb.Raw().Get(ctx, codeKey).Result()
	if err != nil {
		// Missing (never sent, already used, or expired) is the same answer.
		return verificationInvalid()
	}
	attempts, _ := rdb.Raw().Get(ctx, attemptsKey).Int()
	if attempts >= verificationMaxAttempts {
		s.dropVerificationCode(ctx, ident, purpose)
		e := verificationInvalid()
		return e.WithHint("验证码错误次数过多，请重新获取")
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(codeDigest(code))) != 1 {
		n, _ := rdb.Raw().Incr(ctx, attemptsKey).Result()
		if n == 1 {
			_ = rdb.Raw().Expire(ctx, attemptsKey, verificationCodeTTL).Err()
		}
		return verificationInvalid()
	}
	// One code, one use: delete both the code and its attempt counter.
	s.dropVerificationCode(ctx, ident, purpose)
	return nil
}

func (s *Service) dropVerificationCode(ctx context.Context, ident Identifier, purpose string) {
	rdb, err := s.verifyRedis()
	if err != nil {
		return
	}
	hash := identifierHash(ident.Value)
	codeKey := redisx.VerifyCodeKey(purpose, hash)
	_ = rdb.Raw().Del(ctx, codeKey, codeKey+":n").Err()
}

func (s *Service) verifyRedis() (*redisx.Client, error) {
	if s.Sessions == nil || s.Sessions.Redis == nil {
		return nil, fmt.Errorf("auth: no redis client")
	}
	return s.Sessions.Redis, nil
}

// verificationInvalid is the single answer for a wrong, expired, missing or
// exhausted code.
//
// The message is overridden on purpose: invalid_credentials defaults to
// "账号或密码不正确", which is misleading here — a code-only login or a
// registration has no password to be wrong. The code and 401 status stay the
// same so clients keep branching on `code` (G36).
func verificationInvalid() *apierr.APIError {
	return apierr.New(apierr.CodeInvalidCredentials).
		WithMessage("验证码不正确或已过期").
		WithHint("请确认 6 位数字是否输完整；已过期就重新获取")
}

// randomDigits returns a uniformly random n-digit string (zero-padded).
func randomDigits(n int) (string, error) {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	v, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", n, v), nil
}

// identifierHash keeps the address out of Redis key names. Truncated to 32 hex
// characters: collisions are irrelevant here because the code value itself
// still has to match, and a full digest makes keys needlessly long.
func identifierHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func codeDigest(code string) string {
	sum := sha256.Sum256([]byte("yunshu:vc:" + code))
	return hex.EncodeToString(sum[:])
}

// verificationMail picks the subject and body for one purpose.
//
// The binding mail must not reuse the login wording: one generic message that
// works as either code leaves phishing and accidents with no trace. A recipient
// who was not expecting to bind a mailbox has to be able to tell from the text.
func verificationMail(code, purpose string) (subject, body string) {
	if purpose == PurposeBind {
		return "云枢 · 绑定登录邮箱验证码", bindMailBody(code)
	}
	if purpose == PurposeReset {
		return "云枢 · 重置密码验证码", resetMailBody(code)
	}
	return "云枢验证码", verificationMailBody(code)
}

func verificationMailBody(code string) string {
	return "你的验证码是 " + code + "，10 分钟内有效。\n\n" +
		"如果不是你本人操作，请忽略这封邮件，不要把这个验证码告诉任何人。\n\n" +
		"—— 云枢"
}

func resetMailBody(code string) string {
	return "你正在重置云枢账号的登录密码，验证码是 " + code + "，10 分钟内有效。\n\n" +
		"如果不是你本人操作，说明有人知道你的登录邮箱；请忽略这封邮件，" +
		"并考虑更换邮箱密码。任何情况下都不要把这个验证码告诉别人。\n\n—— 云枢"
}

func bindMailBody(code string) string {
	return "你正在为云枢账号绑定登录邮箱，验证码是 " + code + "，10 分钟内有效。\n\n" +
		"绑定后，这个邮箱将成为你的登录名与找回密码的唯一自助渠道。\n" +
		"如果不是你本人操作，请忽略这封邮件：没有这个验证码，邮箱不会被绑定。\n\n—— 云枢"
}

// verificationSMSBody is the text of one message. It carries nothing but the
// code: a text message is billed per 70 characters and is read on a lock screen.
func verificationSMSBody(code string) string {
	return "【云枢】验证码 " + code + "，10 分钟内有效。非本人操作请忽略。"
}
