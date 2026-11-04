package sms

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yunshu.dev/core/internal/jsonx"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// fixedClock pins the timestamp the way a recorded fixture would: the golden
// signature below is only meaningful when the Timestamp parameter is fixed.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func fixedProvider(endpoint string) *AliyunProvider {
	return &AliyunProvider{
		AccessKeyID:     "test-ak-id",
		AccessKeySecret: "test-ak-secret",
		SignName:        "云枢",
		TemplateCode:    "SMS_123456",
		Endpoint:        endpoint,
		Now:             fixedClock{t: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)},
		Nonce:           func() (string, error) { return "fixed-nonce-0123456789abcdef", nil },
	}
}

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// TestFromEnvMatrix pins the three documented modes plus the empty default.
func TestFromEnvMatrix(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "empty means off", env: map[string]string{}, want: ModeOff},
		{name: "explicit off", env: map[string]string{EnvMode: "off"}, want: ModeOff},
		{name: "log", env: map[string]string{EnvMode: "log"}, want: ModeLog},
		{name: "case and space folded", env: map[string]string{EnvMode: " LOG "}, want: ModeLog},
		{name: "unknown mode is an error", env: map[string]string{EnvMode: "bogus"}, wantErr: true},
		{
			name: "provider with every credential",
			env: map[string]string{
				EnvMode:                  ModeProvider,
				EnvAliyunAccessKeyID:     "id",
				EnvAliyunAccessKeySecret: "secret",
				EnvAliyunSignName:        "sign",
				EnvAliyunTemplateCode:    "SMS_1",
			},
			want: ModeProvider,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := fromEnv(nil, envMap(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mode %q accepted, want an error", tc.env[EnvMode])
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v, want mode %s", err, tc.want)
			}
			if client.Mode != tc.want {
				t.Fatalf("mode=%q, want %q", client.Mode, tc.want)
			}
		})
	}
}

// TestFromEnvProviderMissingCredentialNamesTheVariableNotItsValue is the whole
// point of failing at boot: the operator has to learn which variable is empty,
// and the log must never contain a half-filled credential.
func TestFromEnvProviderMissingCredentialNamesTheVariableNotItsValue(t *testing.T) {
	env := map[string]string{
		EnvMode:                  ModeProvider,
		EnvAliyunAccessKeyID:     "should-not-leak-id",
		EnvAliyunAccessKeySecret: "",
		EnvAliyunSignName:        "should-not-leak-sign",
		EnvAliyunTemplateCode:    "SMS_1",
	}
	_, err := fromEnv(nil, envMap(env))
	if err == nil {
		t.Fatalf("provider mode without a secret started successfully")
	}
	msg := err.Error()
	if !strings.Contains(msg, EnvAliyunAccessKeySecret) {
		t.Fatalf("error %q does not name %s", msg, EnvAliyunAccessKeySecret)
	}
	for _, forbidden := range []string{"should-not-leak-id", "should-not-leak-sign", "SMS_1"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("error %q leaks the configured value %q", msg, forbidden)
		}
	}
}

// TestLogModeNeverTouchesTheNetwork: a log-mode deployment must not call any
// supplier. The counter is the assertion — a message that went out anyway would
// cost money and prove the mode check is on the wrong side of delivery.
func TestLogModeNeverTouchesTheNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"Code":"OK"}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	logger := obslog.NewWithWriter("test", "test", &out, clock.Real{})
	client, err := fromEnv(logger, envMap(map[string]string{
		EnvMode:           ModeLog,
		EnvAliyunEndpoint: srv.URL,
	}))
	if err != nil {
		t.Fatalf("fromEnv: %v", err)
	}
	if err := client.Send(context.Background(), "13800001234", "你的验证码是 123456，10 分钟内有效。"); err != nil {
		t.Fatalf("log mode send: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("log mode made %d network calls", n)
	}
	// The code is allowed to appear here — and only here. It is printed as the
	// body of one line, never as a structured field.
	line := out.String()
	if !strings.Contains(line, "123456") {
		t.Fatalf("log-mode line %q does not carry the code", line)
	}
	if !strings.Contains(line, "138****34") {
		t.Fatalf("log-mode line %q does not mask the handset", line)
	}
}

// TestOffModeRefusesToSend: off is the default, and a caller that reaches Send
// with the channel switched off must not be told the message went out.
func TestOffModeRefusesToSend(t *testing.T) {
	client, err := fromEnv(nil, envMap(nil))
	if err != nil {
		t.Fatalf("fromEnv: %v", err)
	}
	if err := client.Send(context.Background(), "13800001234", "code 123456"); err == nil {
		t.Fatalf("off mode reported a successful send")
	}
}

// indepPercentEncode is the test's own encoder. It deliberately does not call
// percentEncode: a regression in the implementation has to fail this test, not
// be validated by the same code it is checking.
func indepPercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c)
		case c >= 'a' && c <= 'z':
			b.WriteByte(c)
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// indepCanonical sorts with the standard library and encodes with the test's own
// encoder, so [ordering + encoding] is checked from the outside.
func indepCanonical(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, indepPercentEncode(k)+"="+indepPercentEncode(params[k]))
	}
	return strings.Join(parts, "&")
}

func TestPercentEncode(t *testing.T) {
	cases := map[string]string{
		"abcXYZ019-_.~":    "abcXYZ019-_.~",
		"a b":              "a%20b",
		"a*b":              "a%2Ab",
		"{\"code\":\"1\"}": "%7B%22code%22%3A%221%22%7D",
		"云枢":               "%E4%BA%91%E6%9E%A2",
		"a+b&c=d":          "a%2Bb%26c%3Dd",
		"":                 "",
	}
	for in, want := range cases {
		if got := percentEncode(in); got != want {
			t.Fatalf("percentEncode(%q)=%q, want %q", in, got, want)
		}
		if got := indepPercentEncode(in); got != want {
			t.Fatalf("the test's own encoder disagrees for %q: %q", in, got)
		}
	}
}

// TestCanonicalQueryGolden pins the exact bytes for a small, hand-checkable
// parameter set: byte-order sorting (upper-case keys first), '=' and '&'
// encoding, space as %20 and '*' as %2A.
func TestCanonicalQueryGolden(t *testing.T) {
	params := map[string]string{
		"a": "1",
		"b": "a b*c~",
		"B": "x",
		"c": "云枢",
	}
	const want = "B=x&a=1&b=a%20b%2Ac~&c=%E4%BA%91%E6%9E%A2"
	if got := canonicalQuery(params); got != want {
		t.Fatalf("canonicalQuery=%q, want %q", got, want)
	}
	if got := indepCanonical(params); got != want {
		t.Fatalf("independent canonical=%q, want %q", got, want)
	}
}

// TestSignatureIsRecomputedIndependently fixes nonce and timestamp, asserts the
// canonical query byte for byte, and recomputes the signature with crypto/hmac
// rather than calling the implementation a second time.
func TestSignatureIsRecomputedIndependently(t *testing.T) {
	const nonce = "fixed-nonce-0123456789abcdef"
	const code = "123456"
	p := fixedProvider("")
	ts := p.Now.Now().UTC()

	params, err := p.requestParams("13800001234", code, ts, nonce)
	if err != nil {
		t.Fatalf("requestParams: %v", err)
	}
	if params["Timestamp"] != "2026-03-04T05:06:07Z" {
		t.Fatalf("timestamp=%q, want the fixed UTC instant", params["Timestamp"])
	}

	cqs := canonicalQuery(params)
	if want := indepCanonical(params); cqs != want {
		t.Fatalf("canonical query\n got: %s\nwant: %s", cqs, want)
	}
	// Byte-for-byte golden. Every segment below is a rule this package must keep:
	// parameters sorted by key, '/' -> %2F, '"' -> %22, '{' -> %7B, ':' -> %3A.
	const wantCQS = "AccessKeyId=test-ak-id&Action=SendSms&Format=JSON&PhoneNumbers=13800001234&RegionId=cn-hangzhou&SignName=%E4%BA%91%E6%9E%A2&SignatureMethod=HMAC-SHA1&SignatureNonce=fixed-nonce-0123456789abcdef&SignatureVersion=1.0&TemplateCode=SMS_123456&TemplateParam=%7B%22code%22%3A%22123456%22%7D&Timestamp=2026-03-04T05%3A06%3A07Z&Version=2017-05-25"
	if cqs != wantCQS {
		t.Fatalf("canonical query\n got: %s\nwant: %s", cqs, wantCQS)
	}

	// Recomputed from scratch: StringToSign is "GET&%2F&" + encoded CQS, and the
	// HMAC key is the secret with a trailing '&'.
	stringToSign := "GET&%2F&" + indepPercentEncode(cqs)
	mac := hmac.New(sha1.New, []byte(p.AccessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	wantSignature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got := signature(p.AccessKeySecret, cqs); got != wantSignature {
		t.Fatalf("signature=%q, want %q", got, wantSignature)
	}

	query, err := p.signedQuery("13800001234", code, ts, nonce)
	if err != nil {
		t.Fatalf("signedQuery: %v", err)
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("signed query is not a valid query string: %v", err)
	}
	if values.Get("Signature") != wantSignature {
		t.Fatalf("SignedQuery Signature=%q, want %q", values.Get("Signature"), wantSignature)
	}
}

// TestSendSmsHappyPath puts the request on the wire and inspects it: the
// Signature parameter exists, the handset is correct and TemplateParam is a JSON
// object (not a Go-printed map).
func TestSendSmsHappyPath(t *testing.T) {
	var gotQuery url.Values
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"OK","Message":"OK","BizId":"900","RequestId":"r1"}`))
	}))
	defer srv.Close()

	p := fixedProvider(srv.URL)
	if err := p.SendCode(context.Background(), "13800001234", "123456"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if method != http.MethodGet {
		t.Fatalf("method=%s, want GET", method)
	}
	if gotQuery.Get("Signature") == "" {
		t.Fatalf("request carries no Signature: %v", gotQuery)
	}
	if got := gotQuery.Get("PhoneNumbers"); got != "13800001234" {
		t.Fatalf("PhoneNumbers=%q", got)
	}
	if got := gotQuery.Get("TemplateParam"); got != `{"code":"123456"}` {
		t.Fatalf("TemplateParam=%q, want a JSON object", got)
	}
	if got := gotQuery.Get("Action"); got != "SendSms" {
		t.Fatalf("Action=%q", got)
	}
}

// TestSendSmsProviderErrorIsSanitised: the supplier rejected the message, and
// the error string is what reaches the process log. It must name the supplier
// code and must not contain the secret or the verification code.
func TestSendSmsProviderErrorIsSanitised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Code":"isv.BUSINESS_LIMIT_CONTROL","Message":"触发分钟级流控"}`))
	}))
	defer srv.Close()

	p := fixedProvider(srv.URL)
	err := p.SendCode(context.Background(), "13800001234", "654321")
	if err == nil {
		t.Fatalf("a rejected send returned nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "isv.BUSINESS_LIMIT_CONTROL") {
		t.Fatalf("error %q does not carry the supplier code", msg)
	}
	if strings.Contains(msg, p.AccessKeySecret) {
		t.Fatalf("error %q leaks the AccessKeySecret", msg)
	}
	if strings.Contains(msg, "654321") {
		t.Fatalf("error %q leaks the verification code", msg)
	}
	if strings.Contains(msg, "13800001234") {
		t.Fatalf("error %q leaks the handset number", msg)
	}
}

// TestSendSmsHTTPErrorIsSanitised covers the non-200 branch: a captive portal or
// a wrong endpoint must not become a 200 with an unread body.
func TestSendSmsHTTPErrorIsSanitised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"Code":"InternalError","Message":"boom"}`))
	}))
	defer srv.Close()

	p := fixedProvider(srv.URL)
	err := p.SendCode(context.Background(), "13800001234", "654321")
	if err == nil {
		t.Fatalf("HTTP 500 with a JSON body was treated as success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Fatalf("error %q does not carry the HTTP status", msg)
	}
	if strings.Contains(msg, "654321") || strings.Contains(msg, p.AccessKeySecret) {
		t.Fatalf("error %q leaks a secret", msg)
	}
}

// TestExtractCode pins the seam between the plain-text body and the supplier
// template variable.
func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"你的验证码是 123456，10 分钟内有效。": "123456",
		"code: 000001":       "000001",
		"no digits here":     "",
		"13800001234 789012": "138000",
	}
	for body, want := range cases {
		got, err := extractCode(body)
		if want == "" {
			if err == nil {
				t.Fatalf("body %q produced code %q, want an error", body, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if got != want {
			t.Fatalf("body %q gave %q, want %q", body, got, want)
		}
	}
}

// TestStringHidesCredentials: %v and %#v of the client must never render the
// AccessKeySecret, because that is exactly what a wrapped error does.
func TestStringHidesCredentials(t *testing.T) {
	client, err := fromEnv(nil, envMap(map[string]string{
		EnvMode:                  ModeProvider,
		EnvAliyunAccessKeyID:     "ak-id-visible-ish",
		EnvAliyunAccessKeySecret: "super-secret-value",
		EnvAliyunSignName:        "云枢",
		EnvAliyunTemplateCode:    "SMS_1",
	}))
	if err != nil {
		t.Fatalf("fromEnv: %v", err)
	}
	for _, rendered := range []string{
		client.String(),
		client.GoString(),
		fmt.Sprintf("%v", client),
		fmt.Sprintf("%#v", client),
		fmt.Sprintf("%v", client.vendor),
		fmt.Sprintf("%#v", client.vendor),
	} {
		if strings.Contains(rendered, "super-secret-value") {
			t.Fatalf("rendered form leaks the secret: %s", rendered)
		}
	}
}

// TestJSONRoundTrip guards the response parser against a schema change: the
// vendor answers with Code/Message and the driver reads exactly those fields.
func TestJSONRoundTrip(t *testing.T) {
	var parsed aliyunResponse
	if err := jsonx.Unmarshal([]byte(`{"Code":"OK","Message":"OK","BizId":"1","RequestId":"2"}`), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Code != "OK" || parsed.BizID != "1" || parsed.RequestID != "2" {
		t.Fatalf("parsed=%+v", parsed)
	}
}
