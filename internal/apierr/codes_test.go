package apierr

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureRow is a hand-copied snapshot of PRD V1.2 appendix E.1. It is
// intentionally independent of `specs`: if the registry drifts, this test
// fails instead of agreeing with itself.
type fixtureRow struct {
	code  string
	typ   string
	http  int
	retry int
	class RetryClass
	owner Owner
}

var prdE1Fixture = []fixtureRow{
	// E.1.1 鉴权与账号 (9; E-21 added invalid_credentials + session_expired)
	{CodeInvalidAPIKey, TypeAuthentication, 401, 0, RetryNo, OwnerBusiness},
	{CodeInvalidCredentials, TypeAuthentication, 401, 0, RetryNo, OwnerBusiness},
	{CodeSessionExpired, TypeAuthentication, 401, 0, RetryNo, OwnerBusiness},
	{CodeKeyExpired, TypeAuthentication, 401, 0, RetryNo, OwnerBusiness},
	{CodeKeyDisabled, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeAccountDisabled, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeAccountFrozen, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeIPNotAllowed, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeConflictingAuthHeaders, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},

	// E.1.2 模型可见性 (4)
	{CodeModelNotFound, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeModelNotAvailable, TypeUpstream, 503, 30, RetryYes, OwnerBusiness},
	{CodeModelNotInGroup, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeModelNotAuthorized, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},

	// E.1.3 请求校验 (13; E-21 added invalid_query_parameter, T50 added invalid_request_id)
	{CodeInvalidRequestBody, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidQueryParameter, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidRequestID, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidMessages, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidTemperature, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeMaxTokensExceeded, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidTools, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidImageCount, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidImageSize, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeInvalidImageURL, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeRequestTooLarge, TypeInvalidRequest, 413, 0, RetryNo, OwnerBusiness},
	{CodeRequestReadTimeout, TypeInvalidRequest, 408, 5, RetryYes, OwnerBusiness},
	{CodeUnknownEndpoint, TypeInvalidRequest, 404, 0, RetryNo, OwnerBusiness},

	// E.1.4 计费与余额 (3)
	{CodeInsufficientCredits, TypeBilling, 402, 0, RetryNo, OwnerBusiness},
	{CodeAccountFrozenDueToOverdraft, TypeBilling, 402, 0, RetryNo, OwnerBusiness},
	{CodePriceNotConfigured, TypeInternal, 500, 0, RetryNo, OwnerPlatform},

	// E.1.5 限流 (5)
	{CodeRPMLimitExceeded, TypeRateLimit, 429, 60, RetryDuration, OwnerBusiness},
	{CodeTPMLimitExceeded, TypeRateLimit, 429, 60, RetryDuration, OwnerBusiness},
	{CodeConcurrencyLimitExceeded, TypeRateLimit, 429, 3, RetryDuration, OwnerBusiness},
	{CodeDailyQuotaExceeded, TypeRateLimit, 429, 86400, RetryDuration, OwnerBusiness},
	{CodeRateLimiterUnavailable, TypeServiceUnavailable, 503, 5, RetryYes, OwnerPlatform},

	// E.1.6 幂等 (5)
	{CodeRequestInFlight, TypeConflict, 409, 1, RetryDuration, OwnerBusiness},
	{CodeIdempotencyKeyReused, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeIdempotentReplayUnsupported, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeDuplicateRequestInFlight, TypeConflict, 409, 1, RetryDuration, OwnerBusiness},
	{CodeDuplicateRequestID, TypeConflict, 409, 0, RetryNo, OwnerBusiness},

	// E.1.7 上游与平台 (13)
	{CodeNoAvailableUpstream, TypeUpstream, 503, 30, RetryYes, OwnerConfig},
	{CodeNoAvailableUpstreamForGroup, TypeUpstream, 503, 0, RetryNo, OwnerConfig},
	{CodeUpstreamCapacityExceeded, TypeUpstream, 503, 30, RetryYes, OwnerUpstream},
	{CodeUpstreamTimeout, TypeUpstream, 504, 30, RetryYes, OwnerUpstream},
	{CodeUpstreamDNSFailure, TypeUpstream, 502, 0, RetryNo, OwnerUpstream},
	{CodeUpstreamTLSFailure, TypeUpstream, 502, 0, RetryNo, OwnerUpstream},
	{CodeUpstreamAuthFailed, TypeUpstream, 502, 0, RetryNo, OwnerPlatform},
	{CodeUpstreamModelNotFound, TypeUpstream, 502, 0, RetryNo, OwnerPlatform},
	{CodeUpstreamInvalidRequest, TypeUpstream, 400, 0, RetryNo, OwnerBusiness},
	{CodeUpstreamInvalidResponse, TypeUpstream, 502, 10, RetryYes, OwnerUpstream},
	{CodeUpstreamEmptyResponse, TypeUpstream, 502, 0, RetryNo, OwnerUpstream},
	{CodeUpstreamKeyUnavailable, TypeUpstream, 503, 60, RetryYes, OwnerPlatform},
	{CodeUpstreamAllFailed, TypeUpstream, 502, 30, RetryYes, OwnerUpstream},

	// E.1.8 生命周期与背压 (8)
	{CodeClientAborted, TypeClient, 499, 0, RetryNotApplicable, OwnerBusiness},
	{CodeConvertFailed, TypeInternal, 500, 0, RetryNo, OwnerPlatform},
	{CodeSettlementUnavailable, TypeServiceUnavailable, 503, 5, RetryYes, OwnerPlatform},
	{CodeBackpressureStreamRejected, TypeServiceUnavailable, 503, 5, RetryYes, OwnerPlatform},
	{CodeBackpressureRejected, TypeServiceUnavailable, 503, 2, RetryYes, OwnerPlatform},
	{CodeConfigUnavailable, TypeServiceUnavailable, 503, 10, RetryYes, OwnerPlatform},
	{CodeAuthUnavailable, TypeServiceUnavailable, 503, 5, RetryYes, OwnerPlatform},
	{CodeInternalError, TypeInternal, 500, 0, RetryNo, OwnerPlatform},

	// E.1.9 控制面 (15; E-21 added account_exists, T11 added login_locked,
	// T12 added key_limit_reached, T15 added the four state/entitlement conflict
	// codes and resource_not_found)
	{CodeKeyNotFound, TypeInvalidRequest, 404, 0, RetryNo, OwnerBusiness},
	{CodeKeyLimitReached, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeAccountExists, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeLoginLocked, TypeRateLimit, 429, 900, RetryDuration, OwnerBusiness},
	{CodeAccountNotVerified, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodePermissionDenied, TypeAuthorization, 403, 0, RetryNo, OwnerBusiness},
	{CodeChannelNotFound, TypeInvalidRequest, 404, 0, RetryNo, OwnerBusiness},
	{CodeChannelInUse, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeResourceNotFound, TypeInvalidRequest, 404, 0, RetryNo, OwnerBusiness},
	{CodeConfigItemNotFound, TypeInvalidRequest, 404, 0, RetryNo, OwnerBusiness},
	{CodeConfigValueInvalid, TypeInvalidRequest, 400, 0, RetryNo, OwnerBusiness},
	{CodeResourceStateConflict, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeTrialAlreadyClaimed, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodeInvoiceAlreadyIssued, TypeConflict, 409, 0, RetryNo, OwnerBusiness},
	{CodePriceConflict, TypeConflict, 409, 0, RetryNo, OwnerBusiness},

	// E.1.10 登记但不实现 (1)
	{CodeAccountRPMExceeded, TypeRateLimit, 429, 60, RetryDuration, OwnerBusiness},

	// E.1.11 内容安全 (1)
	{CodeContentPolicyViolation, TypeInvalidRequest, 403, 0, RetryNo, OwnerBusiness},
}

func TestCodeCountIs77(t *testing.T) {
	if got := len(Codes()); got != 77 {
		t.Fatalf("len(Codes()) = %d, want 77", got)
	}
	if got := len(Specs()); got != 77 {
		t.Fatalf("len(Specs()) = %d, want 77", got)
	}
	if got := len(prdE1Fixture); got != 77 {
		t.Fatalf("fixture rows = %d, want 77", got)
	}
	rows := strings.Split(strings.TrimSpace(MarkdownTable()), "\n")
	if len(rows) != 2+77 {
		t.Fatalf("markdown rows = %d, want 2 header + 77 = 79", len(rows))
	}
}

// countCodeConstants parses codes.go rather than reading the registry. A code
// added as a constant but never entered specs (or vice versa) is invisible to
// len(Specs()) but visible here.
func countCodeConstants(t *testing.T) int {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(thisFile), "codes.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Code") {
					count++
				}
			}
		}
	}
	return count
}

func TestCodeConstantsCountIs77(t *testing.T) {
	if count := countCodeConstants(t); count != 77 {
		t.Fatalf("Code* constants = %d, want 77", count)
	}
}

// T21 A6: the three numbers must agree, whatever they are. The card's old "65"
// is not asserted: the registry, the constants and the generated markdown are
// the sources, and a disagreement fails with all three values printed.
func TestCodeCountMatchesDocsTable(t *testing.T) {
	constants := countCodeConstants(t)
	specs := len(Specs())
	codes := len(Codes())

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	docsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "error-codes.md")
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read docs/error-codes.md: %v", err)
	}
	docsRows := 0
	// The code table is only the first table in the file; everything from the
	// 平台可用性白名单 heading down is a different table (the §9.3 numerator,
	// checked by TestErrorCodesDocListsWhitelist) and must not be counted here.
	table := string(raw)
	if idx := strings.Index(table, "## 平台可用性白名单"); idx >= 0 {
		table = table[:idx]
	}
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, "| `") && !strings.Contains(line, "retry_after_seconds") {
			docsRows++
		}
	}
	t.Logf("error codes: constants=%d Specs=%d Codes=%d docs-rows=%d", constants, specs, codes, docsRows)
	if constants != specs || specs != codes || codes != docsRows {
		t.Fatalf("error-code counts disagree: constants=%d Specs=%d Codes=%d docs-rows=%d",
			constants, specs, codes, docsRows)
	}
}

func TestEveryCodeMatchesPRDE1(t *testing.T) {
	seen := make(map[string]bool, len(prdE1Fixture))
	for _, row := range prdE1Fixture {
		seen[row.code] = true
		spec, ok := Lookup(row.code)
		if !ok {
			t.Errorf("%s: fixture code missing from registry", row.code)
			continue
		}
		if spec.Type != row.typ {
			t.Errorf("%s: type = %q, PRD E.1 = %q", row.code, spec.Type, row.typ)
		}
		if spec.HTTPStatus != row.http {
			t.Errorf("%s: HTTP = %d, PRD E.1 = %d", row.code, spec.HTTPStatus, row.http)
		}
		if spec.RetryAfterSeconds != row.retry {
			t.Errorf("%s: retry_after_seconds = %d, PRD E.1 = %d",
				row.code, spec.RetryAfterSeconds, row.retry)
		}
		if spec.Retry != row.class {
			t.Errorf("%s: retry class = %q, PRD E.1 = %q", row.code, spec.Retry, row.class)
		}
		if spec.Owner != row.owner {
			t.Errorf("%s: owner = %q, PRD E.1 = %q", row.code, spec.Owner, row.owner)
		}
	}
	for _, code := range Codes() {
		if !seen[code] {
			t.Errorf("%s: registry code missing from PRD E.1 fixture", code)
		}
	}
}

func TestEveryCodeHasOwner(t *testing.T) {
	owners := map[Owner]bool{
		OwnerPlatform: true, OwnerUpstream: true, OwnerConfig: true, OwnerBusiness: true,
	}
	for _, s := range Specs() {
		if s.Owner == "" || !owners[s.Owner] {
			t.Errorf("%s: owner = %q, not one of PLATFORM/UPSTREAM/CONFIG/BUSINESS", s.Code, s.Owner)
		}
		if s.Stage == "" {
			t.Errorf("%s: trigger stage is empty", s.Code)
		}
	}
}

func TestTypeSetHasTenValues(t *testing.T) {
	allowed := map[string]bool{
		TypeAuthentication: true, TypeAuthorization: true, TypeInvalidRequest: true,
		TypeBilling: true, TypeRateLimit: true, TypeConflict: true,
		TypeUpstream: true, TypeServiceUnavailable: true, TypeClient: true, TypeInternal: true,
	}
	seen := make(map[string]bool)
	for _, s := range Specs() {
		if !allowed[s.Type] {
			t.Errorf("%s: type %q is not one of the ten PRD types", s.Code, s.Type)
		}
		seen[s.Type] = true
	}
	if len(seen) != 10 {
		t.Fatalf("types in use = %d, want 10: %v", len(seen), seen)
	}
}

func TestRetryNoAlwaysHasZeroSeconds(t *testing.T) {
	for _, s := range Specs() {
		if s.Retry == RetryNo && s.RetryAfterSeconds != 0 {
			t.Errorf("%s: retry=no but retry_after_seconds=%d (E-1)",
				s.Code, s.RetryAfterSeconds)
		}
		if s.Retry == RetryNotApplicable && s.RetryAfterSeconds != 0 {
			t.Errorf("%s: retry=not_applicable but retry_after_seconds=%d",
				s.Code, s.RetryAfterSeconds)
		}
	}
}

func TestPlatformHintsDoNotSuggestUpgrade(t *testing.T) {
	for _, s := range Specs() {
		if s.Owner != OwnerPlatform {
			continue
		}
		if strings.Contains(s.Hint, "升级套餐") || strings.Contains(s.Hint, "申请提额") {
			t.Errorf("%s: platform-side hint suggests a user quota action: %q", s.Code, s.Hint)
		}
	}
}

func TestRequiredTemplatesArePresent(t *testing.T) {
	for _, s := range Specs() {
		if s.Message == "" {
			t.Errorf("%s: message template is empty", s.Code)
		}
		if s.Hint == "" {
			t.Errorf("%s: hint template is empty", s.Code)
		}
	}
}

// T12 P0: the free-tier ceiling is a state conflict, not a malformed body. The
// diagnostic must tell the caller what to do (release a slot or upgrade), so
// neither message nor hint may mention JSON: editing the body cannot help.
func TestKeyLimitReachedIsStateConflict(t *testing.T) {
	e := Newf(CodeKeyLimitReached, 3, 3, 1)
	if e.HTTPStatus != 409 {
		t.Fatalf("HTTP = %d, want 409", e.HTTPStatus)
	}
	if e.Type != TypeConflict {
		t.Fatalf("type = %q, want %q", e.Type, TypeConflict)
	}
	if e.RetryAfterSeconds != 0 {
		t.Fatalf("retry_after_seconds = %d, want 0 (retrying cannot lift a quota)", e.RetryAfterSeconds)
	}
	if !strings.Contains(e.Message, "免费档最多 3 个 Key，升级套餐可扩容") {
		t.Fatalf("message must carry the T12 hard constraint 2 copy: %q", e.Message)
	}
	if strings.Contains(e.Message, "JSON") {
		t.Fatalf("message must not mention JSON: %q", e.Message)
	}
	if strings.Contains(e.Hint, "JSON") {
		t.Fatalf("hint must not mention JSON: %q", e.Hint)
	}
	// The ceiling comes from quota.free_max_active_keys, so the template must
	// interpolate it instead of spelling 3.
	if raised := Newf(CodeKeyLimitReached, 5, 5, 1); !strings.Contains(raised.Message, "免费档最多 5 个 Key") {
		t.Fatalf("limit is not interpolated: %q", raised.Message)
	}
}

// T15: the four new codes are state/entitlement refusals, so none of them may
// tell the caller to edit a field. Retrying does not help either.
func TestStateConflictCodesAre409RetryNo(t *testing.T) {
	cases := []struct {
		code string
		args []any
		want string // fragment that must appear in the rendered message
	}{
		{CodeResourceStateConflict, []any{"订单当前状态为 CLOSED"}, "当前状态不允许该操作：订单当前状态为 CLOSED"},
		{CodeTrialAlreadyClaimed, nil, "该账号或设备已领取过体验额度"},
		{CodeInvoiceAlreadyIssued, nil, "该订单已开具发票，不可重复申请"},
		{CodePriceConflict, []any{"t_price_rule#1 / t_price_rule#9"}, "该模型存在多处配置且价格不一致：t_price_rule#1 / t_price_rule#9"},
	}
	for _, tc := range cases {
		e := Newf(tc.code, tc.args...)
		if e.HTTPStatus != 409 || e.Type != TypeConflict {
			t.Errorf("%s: HTTP/type = %d/%s, want 409/%s", tc.code, e.HTTPStatus, e.Type, TypeConflict)
		}
		if e.RetryAfterSeconds != 0 || e.Owner != OwnerBusiness {
			t.Errorf("%s: retry/owner = %d/%s, want 0/BUSINESS", tc.code, e.RetryAfterSeconds, e.Owner)
		}
		if !strings.Contains(e.Message, tc.want) {
			t.Errorf("%s: message = %q, want %q", tc.code, e.Message, tc.want)
		}
		if strings.Contains(e.Hint, "JSON") || strings.Contains(e.Hint, "请求体") {
			t.Errorf("%s: hint tells the caller to edit the body: %q", tc.code, e.Hint)
		}
	}
}

// E-27: invalid_request_body is shared by the JSON decoder and by field-value
// validation (count=11, an already-used redeem code, a bad callback signature).
// Most of those bodies are valid JSON, so the template must not claim a syntax
// error the caller would waste time hunting for.
func TestInvalidRequestBodyDoesNotClaimJSON(t *testing.T) {
	e := Newf(CodeInvalidRequestBody, "count")
	if strings.Contains(e.Message, "JSON") || strings.Contains(e.Hint, "JSON") {
		t.Fatalf("invalid_request_body still blames JSON: message=%q hint=%q", e.Message, e.Hint)
	}
	if !strings.Contains(e.Message, "count") {
		t.Fatalf("field path is not interpolated: %q", e.Message)
	}
	if !strings.Contains(e.Message, "请求体参数有误") {
		t.Fatalf("message does not say the body parameters are wrong: %q", e.Message)
	}
}

// A4: the two 401s must remain distinct codes with distinct hints.
func TestInvalidAPIKeyAndKeyExpiredAreDistinct(t *testing.T) {
	a := New(CodeInvalidAPIKey)
	b := Newf(CodeKeyExpired, "2026-10-01T00:00:00+08:00")
	if a.Code == b.Code {
		t.Fatalf("codes are equal: %q", a.Code)
	}
	if a.HTTPStatus != 401 || b.HTTPStatus != 401 {
		t.Fatalf("statuses = %d/%d, want both 401", a.HTTPStatus, b.HTTPStatus)
	}
	if a.Hint == b.Hint {
		t.Fatalf("hints must differ, both %q", a.Hint)
	}
	if !strings.Contains(b.Message, "2026-10-01") {
		t.Fatalf("key_expired message lacks the expiry time: %q", b.Message)
	}
}

func TestModelNotAvailableIs503WithNotice(t *testing.T) {
	e := Newf(CodeModelNotAvailable, "qwen-max")
	if e.HTTPStatus != 503 {
		t.Fatalf("HTTP = %d, want 503 (not 404/500)", e.HTTPStatus)
	}
	if e.RetryAfterSeconds != 30 {
		t.Fatalf("retry = %d, want 30", e.RetryAfterSeconds)
	}
	if !strings.Contains(e.Message, "恢复上架后有公告") {
		t.Fatalf("message must announce the recovery notice: %q", e.Message)
	}
}

func TestUpstreamCapacityIs503Not429(t *testing.T) {
	if e := New(CodeUpstreamCapacityExceeded); e.HTTPStatus != 503 {
		t.Fatalf("HTTP = %d, want 503; upstream 429 must never become client 429", e.HTTPStatus)
	}
}

func TestNoDuplicateCodeValues(t *testing.T) {
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if seen[s.Code] {
			t.Errorf("duplicate code %q", s.Code)
		}
		seen[s.Code] = true
	}
}

// A12: field sets for the ten type examples in E.6 (field order irrelevant).
func TestE6ExampleFieldSets(t *testing.T) {
	now := fixedNow()
	dailyReset := timeAtShanghai(now, 10, 31, 0)
	rpm := Newf(CodeTPMLimitExceeded, int64(630000), 23).
		WithLimit(630000).WithRemaining(0).WithResetAtUnix(dailyReset.Unix()).
		WithRetryAfter(23)
	frozen := NewFrozenDueToOverdraft(37, 50, 0)

	cases := []struct {
		name string
		e    *APIError
		want []string
	}{
		{"authentication_error", New(CodeInvalidAPIKey),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"authorization_error", Newf(CodeModelNotAuthorized, "qwen-max"),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"invalid_request_error", Newf(CodeInvalidTemperature, 2.5),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"billing_error", frozen,
			[]string{"type", "code", "message", "retry_after_seconds", "limit", "remaining", "hint", "request_id"}},
		{"rate_limit_exceeded", rpm,
			[]string{"type", "code", "message", "retry_after_seconds", "reset_at", "limit", "remaining", "hint", "request_id"}},
		{"conflict_error", New(CodeRequestInFlight),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"upstream_error", Newf(CodeUpstreamTimeout, 1),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"service_unavailable_error", New(CodeBackpressureRejected),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"client_error", New(CodeClientAborted),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
		{"internal_error", New(CodeInternalError),
			[]string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errorFieldSet(t, tc.e)
			if !sameSet(got, tc.want) {
				t.Fatalf("field set = %v, E.6 = %v", got, tc.want)
			}
		})
	}
}

// A7 + A12 for every code: required six are always present; optional three
// appear only on rate-limit codes and 402; 402 never has reset_at.
func TestEveryCodeBodyShape(t *testing.T) {
	required := []string{"type", "code", "message", "retry_after_seconds", "hint", "request_id"}
	for _, s := range Specs() {
		got := errorFieldSet(t, New(s.Code))
		for _, key := range required {
			if !got[key] {
				t.Errorf("%s: required field %q missing", s.Code, key)
			}
		}
		switch s.Optional {
		case OptionalNone:
			for _, key := range []string{"limit", "remaining", "reset_at"} {
				if got[key] {
					t.Errorf("%s: field %q must be absent for a non-rate-limit code", s.Code, key)
				}
			}
		case OptionalBilling:
			if !got["limit"] || !got["remaining"] {
				t.Errorf("%s: 402 must carry limit and remaining", s.Code)
			}
			if got["reset_at"] {
				t.Errorf("%s: 402 must not carry reset_at", s.Code)
			}
		case OptionalRateLimit:
			for _, key := range []string{"limit", "remaining", "reset_at"} {
				if !got[key] {
					t.Errorf("%s: 429 must carry %s", s.Code, key)
				}
			}
		}
	}
}

func errorFieldSet(t *testing.T, e *APIError) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	body, ok := envelope["error"]
	if !ok {
		t.Fatalf("body has no top-level error object: %s", raw)
	}
	out := make(map[string]bool, len(body))
	for k := range body {
		out[k] = true
	}
	return out
}

func sameSet(got map[string]bool, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}

// Write emits Retry-After for every code with a positive retry_after_seconds,
// independent of HTTP status. That is only safe while the two agree: a
// non-zero backoff on a "do not retry" code would tell clients to wait for
// something they must not retry.
func TestNonZeroRetryAfterImpliesRetryable(t *testing.T) {
	for _, spec := range specs {
		if spec.RetryAfterSeconds == 0 {
			continue
		}
		switch spec.Retry {
		case RetryYes, RetryDuration:
		default:
			t.Errorf("%s: retry_after_seconds=%d but retry=%v",
				spec.Code, spec.RetryAfterSeconds, spec.Retry)
		}
	}
}
