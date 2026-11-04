package sms

// Independent QA verification of the Aliyun RPC signature.
//
// The golden values below were NOT produced by this package: they come from a
// separate implementation (Python 3.13, `hmac` + `hashlib` + `urllib.parse.quote`
// with safe="-_.~"), fed the same fixed parameter set. That is the point — a
// regression in percentEncode, in the sort order or in the HMAC key derivation
// has to disagree with an outside oracle, not with a second copy of itself.
//
// Recorded from:
//
//	params = {AccessKeyId: test-ak-id, Action: SendSms, Format: JSON,
//	          RegionId: cn-hangzhou, SignatureMethod: HMAC-SHA1,
//	          SignatureNonce: fixed-nonce-0123456789abcdef,
//	          SignatureVersion: 1.0, Timestamp: 2026-03-04T05:06:07Z,
//	          Version: 2017-05-25, PhoneNumbers: 13800001234, SignName: 云枢,
//	          TemplateCode: SMS_123456, TemplateParam: {"code":"123456"}}
//	secret = test-ak-secret

import (
	"net/url"
	"testing"
	"time"
)

const (
	qaOracleCQS = "AccessKeyId=test-ak-id&Action=SendSms&Format=JSON&PhoneNumbers=13800001234&RegionId=cn-hangzhou&SignName=%E4%BA%91%E6%9E%A2&SignatureMethod=HMAC-SHA1&SignatureNonce=fixed-nonce-0123456789abcdef&SignatureVersion=1.0&TemplateCode=SMS_123456&TemplateParam=%7B%22code%22%3A%22123456%22%7D&Timestamp=2026-03-04T05%3A06%3A07Z&Version=2017-05-25"
	qaOracleSig = "D1JVN0FcYIcoO/2sh5only5E+vk="
	// Same parameter set with Timestamp ...:08Z, computed by the same外部 oracle.
	qaOracleSigLater = "ShGNN6Ze25Xv/IuwG+3STWm2AIk="
	qaNonce          = "fixed-nonce-0123456789abcdef"
	qaCode           = "123456"
	qaPhone          = "13800001234"
)

// TestQA_AliyunSignatureMatchesExternalOracle recomputes nothing inside the
// package: it compares the implementation against the recorded外部 values, and
// then proves the signature actually depends on the timestamp (a signature that
// ignores a parameter would still "look" stable and pass a sloppier golden test).
func TestQA_AliyunSignatureMatchesExternalOracle(t *testing.T) {
	p := fixedProvider("")
	ts := p.Now.Now().UTC()

	params, err := p.requestParams(qaPhone, qaCode, ts, qaNonce)
	if err != nil {
		t.Fatalf("requestParams: %v", err)
	}
	cqs := canonicalQuery(params)
	if cqs != qaOracleCQS {
		t.Fatalf("canonical query disagrees with the外部 oracle\n got: %s\nwant: %s", cqs, qaOracleCQS)
	}
	if got := signature(p.AccessKeySecret, cqs); got != qaOracleSig {
		t.Fatalf("signature=%q, want the外部 oracle value %q", got, qaOracleSig)
	}

	// The value that goes on the wire is the same one.
	query, err := p.signedQuery(qaPhone, qaCode, ts, qaNonce)
	if err != nil {
		t.Fatalf("signedQuery: %v", err)
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("signed query is not a valid query string: %v", err)
	}
	if got := values.Get("Signature"); got != qaOracleSig {
		t.Fatalf("wire Signature=%q, want %q", got, qaOracleSig)
	}

	// One second later the signature must change, and to the value the oracle
	// gives for that instant.
	later, err := p.requestParams(qaPhone, qaCode, ts.Add(time.Second), qaNonce)
	if err != nil {
		t.Fatalf("requestParams(later): %v", err)
	}
	laterCQS := canonicalQuery(later)
	if laterCQS == cqs {
		t.Fatalf("Timestamp does not participate in the canonical query")
	}
	laterSig := signature(p.AccessKeySecret, laterCQS)
	if laterSig == qaOracleSig {
		t.Fatalf("the signature did not change with the timestamp")
	}
	if laterSig != qaOracleSigLater {
		t.Fatalf("later signature=%q, want the外部 oracle value %q", laterSig, qaOracleSigLater)
	}
}
