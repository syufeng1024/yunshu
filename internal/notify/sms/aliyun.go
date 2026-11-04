package sms

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/platform/clock"
)

// Aliyun Dysmsapi (RPC style) constants.
//
// The protocol is implemented by hand — crypto/hmac, sort and net/http are the
// whole dependency list. A vendor SDK would drag in an entire transitive tree
// plus its own credential handling, and the signature itself is a pure function
// that can be pinned by a golden test.
const (
	aliyunDefaultEndpoint = "https://dysmsapi.aliyuncs.com"
	aliyunAction          = "SendSms"
	aliyunAPIVersion      = "2017-05-25"
	aliyunRegionID        = "cn-hangzhou"
	aliyunSignatureMethod = "HMAC-SHA1"
	aliyunSignatureVer    = "1.0"
	aliyunFormat          = "JSON"
	aliyunOKCode          = "OK"
	aliyunTimeout         = 10 * time.Second
	aliyunTimestampLayout = "2006-01-02T15:04:05Z"
	// maxResponseBytes bounds the vendor response. A well-behaved reply is a few
	// hundred bytes; anything else is a captive portal or a wrong endpoint.
	maxResponseBytes = 64 * 1024
)

// AliyunProvider sends verification codes through Aliyun's Dysmsapi.
//
// The signature is RPC-style: every parameter (public and business) is sorted
// by key, percent-encoded and joined with '&'; that string is percent-encoded
// again into StringToSign, and the signature is
// base64(HMAC-SHA1(AccessKeySecret + "&", StringToSign)).
type AliyunProvider struct {
	AccessKeyID     string
	AccessKeySecret string
	SignName        string
	TemplateCode    string
	// Endpoint allows a test stub. Empty means the production host.
	Endpoint string

	// Now and Nonce are injectable so the golden test can fix both inputs.
	// Nil means the real clock and a random nonce.
	Now   clock.Clock
	Nonce func() (string, error)
	// HTTP is optional; nil builds a client with a 10s budget.
	HTTP *http.Client
}

// Name is the driver name used in the startup line and in errors.
func (p *AliyunProvider) Name() string { return "aliyun" }

// String renders the provider without its secret, so %v of a wrapped error or a
// log line can never leak the AccessKeySecret.
func (p *AliyunProvider) String() string {
	if p == nil {
		return "aliyun(nil)"
	}
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = aliyunDefaultEndpoint
	}
	return fmt.Sprintf("aliyun{endpoint=%s sign=%s template=%s ak=%s}",
		endpoint, p.SignName, p.TemplateCode, maskAccessKeyID(p.AccessKeyID))
}

// GoString is what %#v calls; without it %#v prints the struct verbatim.
func (p *AliyunProvider) GoString() string { return p.String() }

// maskAccessKeyID keeps an AccessKeyId recognisable without printing it whole.
// The id is not a secret, but there is no reason to copy it into a log line.
func maskAccessKeyID(id string) string {
	if len(id) <= 4 {
		return "***"
	}
	return id[:4] + "***"
}

func (p *AliyunProvider) now() clock.Clock {
	if p.Now != nil {
		return p.Now
	}
	return clock.Real{}
}

func (p *AliyunProvider) nonce() (string, error) {
	if p.Nonce != nil {
		return p.Nonce()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sms: random nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (p *AliyunProvider) httpClient() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: aliyunTimeout}
}

func (p *AliyunProvider) endpoint() string {
	if e := strings.TrimSpace(p.Endpoint); e != "" {
		return e
	}
	return aliyunDefaultEndpoint
}

// requestParams builds the full parameter set for one SendSms call, before the
// signature is added. It is a pure function of its inputs so the golden test can
// pin the exact bytes.
func (p *AliyunProvider) requestParams(phone, code string, ts time.Time, nonce string) (map[string]string, error) {
	// TemplateParam is JSON in the query string; building it through jsonx keeps
	// the escaping rule in one place (and keeps encoding/json out of this
	// package, per the repository-wide rule).
	param, err := jsonx.Marshal(map[string]string{"code": code})
	if err != nil {
		return nil, fmt.Errorf("sms: aliyun template param: %w", err)
	}
	return map[string]string{
		// Public parameters.
		"AccessKeyId":      p.AccessKeyID,
		"Action":           aliyunAction,
		"Format":           aliyunFormat,
		"RegionId":         aliyunRegionID,
		"SignatureMethod":  aliyunSignatureMethod,
		"SignatureNonce":   nonce,
		"SignatureVersion": aliyunSignatureVer,
		"Timestamp":        ts.UTC().Format(aliyunTimestampLayout),
		"Version":          aliyunAPIVersion,
		// Business parameters. The template's only variable is ${code}, which is
		// why the supplier-side template has to be registered with that name.
		"PhoneNumbers":  phone,
		"SignName":      p.SignName,
		"TemplateCode":  p.TemplateCode,
		"TemplateParam": string(param),
	}, nil
}

// signedQuery returns the complete query string (signature included) for one
// message. Signature is itself a percent-encoded parameter, so the result is
// what actually goes on the wire.
func (p *AliyunProvider) signedQuery(phone, code string, ts time.Time, nonce string) (string, error) {
	params, err := p.requestParams(phone, code, ts, nonce)
	if err != nil {
		return "", err
	}
	cqs := canonicalQuery(params)
	params["Signature"] = signature(p.AccessKeySecret, cqs)
	return canonicalQuery(params), nil
}

// canonicalQuery sorts the parameters by key and joins "k=v" pairs with '&',
// percent-encoding both sides.
//
// Sorting is by raw byte order (sort.Strings), which is what the protocol means
// by lexicographic: it is not locale-aware and not case-insensitive.
func canonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(percentEncode(key))
		b.WriteByte('=')
		b.WriteString(percentEncode(params[key]))
	}
	return b.String()
}

// signature computes base64(HMAC-SHA1(secret+"&", StringToSign)).
func signature(secret, cqs string) string {
	stringToSign := "GET" + "&" + percentEncode("/") + "&" + percentEncode(cqs)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// percentEncode applies the RPC-style encoding rule. It is not url.QueryEscape:
//
//	A-Z a-z 0-9 - _ . ~   kept as-is
//	space                 %20   (QueryEscape would produce '+')
//	*                     %2A   (QueryEscape leaves it alone)
//	everything else       %XX   upper-case hex of each UTF-8 byte
//
// Getting '*' or ' ' wrong produces a signature the supplier rejects with
// "SignatureDoesNotMatch", which reads like a credential problem.
func percentEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// aliyunResponse is the subset of the SendSms reply this package reads.
type aliyunResponse struct {
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	BizID     string `json:"BizId"`
	RequestID string `json:"RequestId"`
}

// SendCode issues one SendSms call.
//
// Every error carries the supplier's Code/Message and the HTTP status only. It
// never carries the AccessKeySecret, the handset number or the verification
// code: an error string ends up in the process log, and a log line with a live
// code in it is a credential leak (see the 06 design §12 R8).
func (p *AliyunProvider) SendCode(ctx context.Context, phone, code string) error {
	if p == nil || strings.TrimSpace(p.AccessKeyID) == "" || p.AccessKeySecret == "" {
		return ErrNotConfigured
	}
	ts := p.now().Now().UTC()
	nonce, err := p.nonce()
	if err != nil {
		return err
	}
	query, err := p.signedQuery(phone, code, ts, nonce)
	if err != nil {
		return err
	}
	endpoint := p.endpoint()
	requestURL := strings.TrimRight(endpoint, "/") + "/?" + query

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("sms: aliyun build request: %w", err)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		// Deliberately not wrapping: *url.Error renders the whole request URL,
		// which carries TemplateParam (the code) and AccessKeyId. Only the host
		// and the error type survive, which is enough to tell a DNS failure from
		// a TLS failure.
		return fmt.Errorf("sms: aliyun SendSms request to %s failed (%s)", hostOf(endpoint), errorKind(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only cleanup

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("sms: aliyun SendSms read response (HTTP %d): %s", resp.StatusCode, errorKind(err))
	}
	var parsed aliyunResponse
	if err := jsonx.Unmarshal(payload, &parsed); err != nil {
		return fmt.Errorf("sms: aliyun SendSms unparseable response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sms: aliyun SendSms HTTP %d code=%s message=%s",
			resp.StatusCode, orDash(parsed.Code), orDash(parsed.Message))
	}
	if parsed.Code != aliyunOKCode {
		return fmt.Errorf("sms: aliyun SendSms rejected code=%s message=%s",
			orDash(parsed.Code), orDash(parsed.Message))
	}
	return nil
}

// hostOf returns the host of an endpoint URL for error messages. An unparseable
// value is echoed as-is: it is operator-supplied configuration, not user input.
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	return u.Host
}

// errorKind names the concrete error type without its message. net errors embed
// the address they failed on, and *url.Error embeds the URL.
func errorKind(err error) string {
	if err == nil {
		return "none"
	}
	return strconv.Quote(strings.TrimPrefix(fmt.Sprintf("%T", err), "*"))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
