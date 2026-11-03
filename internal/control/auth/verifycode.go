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

// Verification purposes. They are separate Redis namespaces so a code obtained
// for "register" cannot be replayed to log in (and vice versa).
const (
	PurposeLogin    = "login"
	PurposeRegister = "register"
)

// Sender is the outbound e-mail seam. It matches fund.Mailer and ticket.Mailer
// method-for-method so control.Deps can inject one implementation for all
// three (internal/notify/mail).
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

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

// SendVerificationCode implements POST /api/v1/auth/verification-code.
//
// Response shape is the same whether or not the account exists: the endpoint
// must not become an account oracle. The only case that is distinguishable is
// "register" for an address that already has an account — that is already
// public knowledge from POST /register (409 account_exists), and telling the
// user early is a better experience than letting them fill the whole form.
func (s *Service) SendVerificationCode(w http.ResponseWriter, r *http.Request) {
	var req verificationRequest
	if _, apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	purpose := strings.ToLower(strings.TrimSpace(req.Purpose))
	if purpose != PurposeLogin && purpose != PurposeRegister {
		e := mw.InvalidField("purpose")
		e = e.WithHint("purpose 只能是 login 或 register")
		mw.WriteError(w, r, e)
		return
	}
	ident, err := NormalizeIdentifier(req.Identifier)
	if err != nil {
		mw.WriteError(w, r, mw.InvalidField("identifier"))
		return
	}
	if s.verificationMode() == VerificationOff {
		e := mw.InvalidField("purpose")
		e = e.WithHint("本部署未启用验证码，请直接使用密码登录或注册")
		mw.WriteError(w, r, e)
		return
	}
	// SMS is implemented but deliberately not switched on: delivery is billed
	// per message and the product has not signed a provider yet. Answering an
	// explicit "not available" is the point — silently doing nothing would look
	// like a delivery failure.
	if ident.Kind != "email" {
		e := mw.InvalidField("identifier")
		e = e.WithHint("短信验证码暂未开放，请使用邮箱")
		mw.WriteError(w, r, e)
		return
	}
	if purpose == PurposeRegister {
		if _, findErr := s.findUser(r.Context(), ident); findErr == nil {
			mw.WriteError(w, r, apierr.New(apierr.CodeAccountExists).
				WithHint("该邮箱已注册，请直接登录"))
			return
		}
	}

	code, apiErr := s.issueVerificationCode(r.Context(), ident, purpose)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if mode := s.verificationMode(); mode == VerificationSMTP {
		if s.Mailer == nil {
			s.errorLog(r.Context(), "verification_mail_unconfigured: SMTP mode without a mailer")
			mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).
				WithHint("邮件通道未配置，请联系管理员"))
			return
		}
		if err := s.Mailer.Send(r.Context(), ident.Value, "云枢验证码", verificationMailBody(code)); err != nil {
			// A stored-but-unsent code is unusable by the user and would block
			// their next attempt behind the cooldown, so drop it.
			s.dropVerificationCode(r.Context(), ident, purpose)
			s.errorLog(r.Context(), "verification_mail_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeAuthUnavailable).
				WithHint("验证码邮件发送失败，请稍后重试"))
			return
		}
	} else {
		// log mode: the operator reads the code from the process log. It is
		// printed as the message text, not as a log field, so it cannot leak
		// into a structured sink that ships off-box by accident.
		s.warn(r.Context(), fmt.Sprintf("verification_code_log_mode purpose=%s identifier=%s code=%s",
			purpose, maskIdentifier(ident.Kind, ident.Value), code))
	}

	_ = mw.WriteJSON(w, http.StatusOK, verificationResponse{
		Channel:     "email",
		ExpiresIn:   int(verificationCodeTTL / time.Second),
		ResendAfter: int(verificationResendAfter / time.Second),
	})
}

// issueVerificationCode enforces the cooldown and the daily quota, then stores
// the code. The code itself is stored as a SHA-256 digest: a Redis dump must
// not be a list of working login credentials.
func (s *Service) issueVerificationCode(ctx context.Context, ident Identifier, purpose string) (string, *apierr.APIError) {
	rdb, err := s.verifyRedis()
	if err != nil {
		s.errorLog(ctx, "verification_redis_unavailable: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
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
	// window does not slide on every send: 20 messages per rolling day is a
	// budget, not a rate.
	n, err := rdb.Raw().Incr(ctx, quotaKey).Result()
	if err != nil {
		s.errorLog(ctx, "verification_quota_failed: "+err.Error())
		return "", apierr.New(apierr.CodeAuthUnavailable)
	}
	if n == 1 {
		_ = rdb.Raw().Expire(ctx, quotaKey, 24*time.Hour).Err()
	}
	if n > verificationDailyLimit {
		return "", apierr.Newf(apierr.CodeRPMLimitExceeded, verificationDailyLimit, int((24 * time.Hour).Seconds()))
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

func verificationMailBody(code string) string {
	return "你的验证码是 " + code + "，10 分钟内有效。\n\n" +
		"如果不是你本人操作，请忽略这封邮件，不要把这个验证码告诉任何人。\n\n" +
		"—— 云枢"
}
