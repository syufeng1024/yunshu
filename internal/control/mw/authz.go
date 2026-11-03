package mw

import (
	"net/http"
	"sort"
)

// Permission is one server-side capability. Every route declares exactly one;
// hiding a menu item is never a substitute (T11 hard constraint 1).
type Permission string

// The full T15 vocabulary. Names are 资源.动作; a write permission always means
// "create, update, disable and delete that resource".
//
// Every read permission names one *resource scope*, not just one resource. The
// same table row seen by two consoles is two permissions when the two consoles
// see different columns of it:
//
//   - account.read — customer self-service. Reads your own account row through
//     /api/console/overview; every aggregate is filtered by the session uid.
//   - user.read / channel.read — operator scope. Reads *any* account or
//     upstream channel, so the row carries another customer's disable reason,
//     base_url and key mask.
//
// The two names must stay separate. Reusing user.read for the console overview
// would mean PERSONAL holds an "operator read any account" capability, and
// GET /api/admin/users would answer every customer with the whole account
// list (the E-31/E-33 pattern). Reusing it the other way round — letting
// account.read open /api/admin/users — has the same effect, because the
// handler does not scope by uid. channel.read has no console counterpart at
// all: a customer never sees an upstream channel, so only OPS and SUPER_ADMIN
// hold it, and FINANCE (which legitimately reads the money queue and the model
// cost sheet) must not.
//
// admin.all remains a super-admin shortcut for cross-cutting diagnostics such
// as /api/admin/whoami, but no business route may rely on it: each admin
// endpoint declares the concrete capability it needs so a future role split
// does not silently grant everything.
const (
	// PermAccountRead opens the account console's own overview (balance, plan,
	// quota, today's counters). Held by PERSONAL and by SUPER_ADMIN for support
	// diagnostics. It is never an operator-wide read: swapping it for user.read
	// is a cross-account disclosure, see above.
	PermAccountRead Permission = "account.read"
	// PermAccountWrite is the customer editing their *own* profile (nickname).
	// It is deliberately separate from user.write: that one is the operator
	// scope and can change any account's status, plan and disable reason.
	PermAccountWrite Permission = "account.write"
	// PermUserRead is the operator scope: read any account, including each
	// row's status, plan and disable reason. OPS and SUPER_ADMIN only. FINANCE
	// deliberately does not hold it — finance works from orders, invoices and
	// redeem batches, none of which needs the account directory.
	PermUserRead   Permission = "user.read"
	PermUserWrite  Permission = "user.write"
	PermKeyRead    Permission = "key.read"
	PermKeyWrite   Permission = "key.write"
	PermModelRead  Permission = "model.read"
	PermModelWrite Permission = "model.write"
	PermPriceWrite Permission = "price.write"
	PermGroupWrite Permission = "group.write"
	// PermChannelRead is the operator scope for upstream channels: the rows
	// expose base_url and the key mask, so no customer and no money-only role
	// may hold it.
	PermChannelRead  Permission = "channel.read"
	PermChannelWrite Permission = "channel.write"
	PermNoticeWrite  Permission = "notice.write"
	PermRiskWrite    Permission = "risk.write"
	PermConfigWrite  Permission = "config.write"
	PermFundRead     Permission = "fund.read"
	PermFundWrite    Permission = "fund.write"
	PermFundApprove  Permission = "fund.approve"
	PermTicketRead   Permission = "ticket.read"
	PermTicketWrite  Permission = "ticket.write"
	PermTicketReply  Permission = "ticket.reply"
	PermAuditRead    Permission = "audit.read"
	// log.read opens the account console's own request logs. Only PERSONAL
	// and SUPER_ADMIN hold it: the operator consoles have no route that
	// serves /api/console/logs, and a shared capability would let a customer
	// surface an operator view (E-31/E-33 pattern).
	PermLogRead Permission = "log.read"
	// usage.read opens the account console's own usage, bills, live metrics
	// and CSV export. Same family as log.read: account-scoped, PERSONAL and
	// SUPER_ADMIN only. The export download token is an additional check, not
	// a replacement for this permission.
	PermUsageRead Permission = "usage.read"
	// admin.read gates the operator console's remaining shared read routes:
	// the overview and the model master data (which includes upstream cost
	// prices, B-48). Model reads cannot move to model.read: PERSONAL holds
	// model.read for /api/console/models, so that would hand every customer
	// the cost sheet. The per-resource operator reads are narrower and live in
	// user.read / channel.read, so a role that may read models does not
	// automatically read the account directory or the upstream channels.
	PermAdminRead Permission = "admin.read"
	PermAdminAll  Permission = "admin.all"
)

// rolePermissions is the T15 matrix, keyed by the PRD §6.3 role enum
// (PERSONAL / OPS / FINANCE / SUPER_ADMIN). T11/T15 cards mention a "10-role
// matrix", but PRD §6.3 enumerates exactly four; no other role name is
// implemented, because a name the product does not define would be dead code.
//
// The split that matters for money: OPS may edit prices and channels but not
// approve a refund; FINANCE may approve a refund but not edit prices or
// channels. SUPER_ADMIN is the only role holding admin.all.
//
// PERSONAL holds fund.read/fund.write because a user buys their own plan and
// redeems their own code; every fund handler still scopes by session uid.
//
// ticket.read / ticket.write are the account's own tickets only. OPS holds
// ticket.reply instead of either: the operator console has no business reading
// /api/console/tickets (every row there is scoped to one account), and sharing
// one permission across both consoles would let a customer open the operator
// queue (E-31/E-33 pattern).
//
// log.read is likewise account-scoped: PERSONAL (and SUPER_ADMIN for support
// diagnostics) may read only rows whose user_id equals their session, and no
// operator role holds the permission.
//
// The read scopes are split three ways, and the split is what keeps the account
// directory private:
//
//	account.read  PERSONAL + SUPER_ADMIN   own account only, handler scopes by uid
//	user.read     OPS + SUPER_ADMIN        any account (status, plan, disable reason)
//	channel.read  OPS + SUPER_ADMIN        any upstream channel (base_url, key mask)
//
// PERSONAL must not hold user.read even though it "only" renders the
// customer's own profile: /api/admin/users does not scope by uid, so one name
// for both consoles would let any customer enumerate every account, including
// the disable reason on each row. FINANCE keeps fund.approve and audit.read but
// gets neither user.read nor channel.read: the money queue, the invoice and the
// model cost sheet are all it needs, and the account directory is not.
var rolePermissions = map[string]map[Permission]bool{
	"PERSONAL": {
		PermAccountRead:  true,
		PermAccountWrite: true,
		PermKeyRead:     true,
		PermKeyWrite:    true,
		PermModelRead:   true,
		PermFundRead:    true,
		PermFundWrite:   true,
		PermTicketRead:  true,
		PermTicketWrite: true,
		PermLogRead:     true,
		PermUsageRead:   true,
	},
	"OPS": {
		PermUserRead:     true,
		PermUserWrite:    true,
		PermModelRead:    true,
		PermModelWrite:   true,
		PermPriceWrite:   true,
		PermGroupWrite:   true,
		PermChannelRead:  true,
		PermChannelWrite: true,
		PermNoticeWrite:  true,
		PermRiskWrite:    true,
		PermConfigWrite:  true,
		PermTicketReply:  true,
		PermAuditRead:    true,
		PermAdminRead:    true,
	},
	"FINANCE": {
		PermFundRead:    true,
		PermFundApprove: true,
		PermAuditRead:   true,
		PermModelRead:   true,
		PermAdminRead:   true,
	},
	"SUPER_ADMIN": {
		PermAccountRead:  true,
		PermAccountWrite: true,
		PermUserRead:     true,
		PermUserWrite:    true,
		PermKeyRead:      true,
		PermKeyWrite:     true,
		PermModelRead:    true,
		PermModelWrite:   true,
		PermPriceWrite:   true,
		PermGroupWrite:   true,
		PermChannelRead:  true,
		PermChannelWrite: true,
		PermNoticeWrite:  true,
		PermRiskWrite:    true,
		PermConfigWrite:  true,
		PermFundRead:     true,
		PermFundWrite:    true,
		PermFundApprove:  true,
		PermTicketRead:   true,
		PermTicketWrite:  true,
		PermTicketReply:  true,
		PermAuditRead:    true,
		PermLogRead:      true,
		PermUsageRead:    true,
		PermAdminRead:    true,
		PermAdminAll:     true,
	},
}

// PermissionsFor returns the role's permissions in stable order. An unknown
// role has none: a typo in t_user.role must not grant access.
func PermissionsFor(role string) []Permission {
	set := rolePermissions[role]
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Allows reports whether role holds p.
func Allows(role string, p Permission) bool {
	return rolePermissions[role][p]
}

// Require enforces one permission. It expects SessionReader to have run
// already; a missing session is a 401, a role without the permission is a 403.
func Require(p Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFrom(r.Context())
			if !ok || sess == nil {
				WriteError(w, r, SessionMissing())
				return
			}
			if !Allows(sess.Role, p) {
				WriteError(w, r, PermissionDenied())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
