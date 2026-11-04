package fund

// QA addition for INC-phone-claim (task C11): the fail-closed branch must be
// observable, not just non-zero. A 500 with no log token is indistinguishable
// from any other 500 in the dashboard, and the runbook step it points at
// ("backfill has not run") would never be reached.

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

func TestQATrialMissingClaimLogsErrorToken(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	// phone set, ledger row absent: the state only a manual edit or a backfill
	// that has not run can produce.
	setUserPhoneWithoutClaim(t, uid, "8613800990123")

	var buf bytes.Buffer
	svc.Logger = obslog.NewWithWriter("controld", "dev", &buf, clock.Real{})

	rec := callWithRemote(t, svc.ClaimTrial, http.MethodPost, "/api/console/trial/claim",
		map[string]any{"fingerprint": "device-qa-missing-claim"}, uid, "192.0.2.140:4111", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 (fail closed)", rec.Code, rec.Body.String())
	}
	if code := errorBody(t, rec).Error.Code; code != "internal_error" {
		t.Fatalf("code=%q, want internal_error", code)
	}

	out := buf.String()
	if !strings.Contains(out, "trial_phone_claim_missing") {
		t.Fatalf("500 carries no identifiable token: %s", strings.TrimSpace(out))
	}
	if !strings.Contains(out, `"level":"error"`) {
		t.Fatalf("fail-closed is not logged at error level: %s", strings.TrimSpace(out))
	}
	// The token names the account but never the number or its hash.
	if strings.Contains(out, "8613800990123") {
		t.Fatalf("log leaks the plaintext number: %s", strings.TrimSpace(out))
	}
	if bal := balanceOf(t, uid); bal.Total != 0 {
		t.Fatalf("a refused claim granted %d micro", bal.Total)
	}
}
