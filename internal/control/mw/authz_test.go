package mw

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// T15: the matrix is keyed by the PRD §6.3 four-role enum. This table states
// each cell explicitly instead of deriving it from rolePermissions, so a typo
// in the matrix fails here rather than agreeing with itself.
func TestRolePermissionMatrix(t *testing.T) {
	all := []Permission{
		PermAccountRead, PermUserRead, PermUserWrite, PermKeyRead, PermKeyWrite,
		PermModelRead, PermModelWrite, PermPriceWrite, PermGroupWrite,
		PermChannelRead, PermChannelWrite, PermNoticeWrite, PermRiskWrite,
		PermConfigWrite,
		PermFundRead, PermFundWrite, PermFundApprove,
		PermTicketRead, PermTicketWrite, PermTicketReply,
		PermAuditRead, PermLogRead, PermUsageRead, PermAdminRead, PermAdminAll,
	}
	want := map[string]map[Permission]bool{
		"PERSONAL": {
			PermAccountRead: true, PermKeyRead: true, PermKeyWrite: true,
			PermModelRead: true, PermFundRead: true, PermFundWrite: true,
			PermTicketRead: true, PermTicketWrite: true, PermLogRead: true,
			PermUsageRead: true,
		},
		"OPS": {
			PermUserRead: true, PermUserWrite: true, PermModelRead: true,
			PermModelWrite: true, PermPriceWrite: true, PermGroupWrite: true,
			PermChannelRead: true, PermChannelWrite: true, PermNoticeWrite: true,
			PermRiskWrite: true, PermConfigWrite: true, PermTicketReply: true,
			PermAuditRead: true, PermAdminRead: true,
		},
		"FINANCE": {
			PermFundRead: true, PermFundApprove: true, PermAuditRead: true,
			PermModelRead: true, PermAdminRead: true,
		},
	}
	for _, p := range all {
		want["SUPER_ADMIN"] = setOn(want["SUPER_ADMIN"], p)
	}

	for _, role := range []string{"PERSONAL", "OPS", "FINANCE", "SUPER_ADMIN"} {
		for _, p := range all {
			if got := Allows(role, p); got != want[role][p] {
				t.Errorf("Allows(%q, %q) = %v, want %v", role, p, got, want[role][p])
			}
		}
	}

	// The separation that protects money: OPS cannot approve fund movements,
	// FINANCE cannot change prices or channels.
	if Allows("OPS", PermFundApprove) || Allows("OPS", PermFundWrite) {
		t.Error("OPS must not hold fund.approve / fund.write")
	}
	if Allows("FINANCE", PermPriceWrite) || Allows("FINANCE", PermChannelWrite) {
		t.Error("FINANCE must not hold price.write / channel.write")
	}
	if Allows("OPS", PermAdminAll) || Allows("FINANCE", PermAdminAll) {
		t.Error("admin.all is SUPER_ADMIN only; concrete permissions must guard admin routes")
	}
	// The ticket consoles are split: OPS answers users through ticket.reply but
	// must not read or write the account-scoped console surface; PERSONAL must
	// not reach the operator queue.
	if Allows("OPS", PermTicketRead) || Allows("OPS", PermTicketWrite) {
		t.Error("OPS holds ticket.reply only; ticket.read/ticket.write belong to the account console")
	}
	if Allows("PERSONAL", PermTicketReply) {
		t.Error("PERSONAL must not hold ticket.reply")
	}
	if Allows("FINANCE", PermTicketRead) || Allows("FINANCE", PermTicketWrite) || Allows("FINANCE", PermTicketReply) {
		t.Error("FINANCE has no ticket permission")
	}
	// log.read is account-scoped. No operator role may hold it, otherwise an
	// operator could open a customer's request log through the console route.
	if Allows("OPS", PermLogRead) || Allows("FINANCE", PermLogRead) {
		t.Error("log.read must stay with PERSONAL and SUPER_ADMIN only")
	}
	// usage.read has the same account scope as log.read: the operator
	// consoles have no usage route, so no operator role may hold it.
	if Allows("OPS", PermUsageRead) || Allows("FINANCE", PermUsageRead) {
		t.Error("usage.read must stay with PERSONAL and SUPER_ADMIN only")
	}

	// The read scopes are split by resource scope, not by resource name. These
	// assertions are the regression guard for the cross-account read defect:
	// while PERSONAL held user.read, GET /api/admin/users was open to every
	// customer because the handler does not scope by uid.
	if Allows("PERSONAL", PermUserRead) {
		t.Error("PERSONAL must not hold user.read: /api/admin/users would enumerate every account")
	}
	if Allows("PERSONAL", PermChannelRead) {
		t.Error("PERSONAL must not hold channel.read: upstream base_url and key masks are operator data")
	}
	if Allows("FINANCE", PermUserRead) || Allows("FINANCE", PermChannelRead) {
		t.Error("FINANCE must not hold user.read / channel.read: the money surface needs neither")
	}
	for _, p := range []Permission{PermUserRead, PermChannelRead} {
		if !Allows("OPS", p) || !Allows("SUPER_ADMIN", p) {
			t.Errorf("%q must be held by OPS and SUPER_ADMIN", p)
		}
	}
	// account.read is the customer self-service capability. FINANCE and OPS
	// have no console account route, so they must not hold it either: one name
	// per console keeps a future route swap from widening silently.
	if !Allows("PERSONAL", PermAccountRead) || !Allows("SUPER_ADMIN", PermAccountRead) {
		t.Error("account.read must be held by PERSONAL and SUPER_ADMIN (console overview)")
	}
	if Allows("OPS", PermAccountRead) || Allows("FINANCE", PermAccountRead) {
		t.Error("account.read is the account console's scope; no operator-only role holds it")
	}
	// The three read scopes must be three distinct names, and no role may hold
	// both halves of the account split.
	if PermAccountRead == PermUserRead || PermUserRead == PermChannelRead || PermAccountRead == PermChannelRead {
		t.Error("account.read / user.read / channel.read must be distinct permission names")
	}
	if Allows("PERSONAL", PermAccountRead) && Allows("PERSONAL", PermUserRead) {
		t.Error("a role holding both account.read and user.read puts the two consoles back on one name")
	}

	// An unknown role grants nothing.
	for _, p := range all {
		if Allows("", p) || Allows("NOT_A_ROLE", p) {
			t.Errorf("unknown role granted %q", p)
		}
	}
}

// setOn is a test helper: mark one permission true in a fresh map copy.
func setOn(m map[Permission]bool, p Permission) map[Permission]bool {
	if m == nil {
		m = make(map[Permission]bool)
	}
	m[p] = true
	return m
}

func TestRequireWithoutSessionIs401(t *testing.T) {
	h := Require(PermAdminAll)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not run without a session")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/admin/whoami", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireWrongRoleIs403(t *testing.T) {
	h := Require(PermAdminAll)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not run for a PERSONAL account")
	}))
	r := httptest.NewRequest("GET", "/api/admin/whoami", nil)
	r = r.WithContext(WithSession(r.Context(), &Session{UID: 1, Role: "PERSONAL"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRequireAllowedRole(t *testing.T) {
	called := false
	h := Require(PermAdminAll)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest("GET", "/api/admin/whoami", nil)
	r = r.WithContext(WithSession(r.Context(), &Session{UID: 1, Role: "SUPER_ADMIN"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called=%v status=%d", called, rec.Code)
	}
}
