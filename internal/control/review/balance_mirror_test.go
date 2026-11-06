package review

import (
	"context"
	"net/http"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/fund"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// B15: approving a manual adjustment must invalidate the K26 balance mirror.
//
// The ledger commit is the truth and it was already correct, but bal:{uid} is
// what the gateway reads at admission and it lives for 60s. Paying through the
// fund callback invalidates it; the manual-adjustment path did not, so an
// approved increase stayed invisible (and a decrease stayed spendable) for up
// to a minute.
func TestBalanceAdjustApproveInvalidatesMirror(t *testing.T) {
	env, err := redistest.Open()
	if err != nil {
		t.Skipf("B15 needs the real Redis fixture: %v", err)
	}
	defer func() { _ = env.Client.Close() }()
	// A private client so the flush of another package's fixture cannot change
	// this test's own key between the SET and the assertion.
	rdb := redistest.NewClientAt(env.Addr, env.DB, 0)
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFixedClock(testBase)
	auditWriter := &audit.Writer{DB: testDB, Now: clk}
	funds := fund.New(testDB, rdb, nil, auditWriter, clk, nil)
	svc := New(testDB, auditWriter, clk, nil, funds)

	applicant := newTestUser(t, "OPS")
	reviewer := newTestUser(t, "FINANCE")
	target := newTestUser(t, "PERSONAL")
	seedBalanceAccount(t, target)
	if _, err := svc.Ledger.Grant(context.Background(), ledger.GrantParams{
		UserID:          target,
		RechargeMicro:   100000,
		BizType:         ledger.BizTypeRecharge,
		IdemKeyRecharge: "seed-b15-" + itoa(target),
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	ctx := context.Background()
	key := redisx.BalanceKey(target)
	// No FlushDB: the fixture keyspace is shared with the other control test
	// binaries, and this test only ever touches its own freshly allocated
	// user's key.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rdb.Raw().Del(cleanupCtx, key).Err()
	})
	// Pre-populate K26 with the pre-approval value, which is exactly what a
	// gateway admission in the last 60s would have cached.
	if err := rdb.Raw().Set(ctx, key, 100000, 60*time.Second).Err(); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if got, err := rdb.Raw().Get(ctx, key).Int64(); err != nil || got != 100000 {
		t.Fatalf("mirror before approve = %d (%v), want 100000", got, err)
	}

	rec := submitBalance(t, svc, applicant, target,
		balanceBody(BalanceDirectionIncrease, 50000, "B15 镜像失效测试"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created balanceAdjustSubmitResponse
	decodeBody(t, rec, &created)

	rec = callReview(t, svc.Approve, http.MethodPost,
		"/api/admin/reviews/1/approve", itoa(created.ReviewTaskID), nil, reviewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The money must have moved...
	if got := balanceOf(t, target); got != 150000 {
		t.Fatalf("balance after approve = %d, want 150000", got)
	}
	// ...and the cached view must be gone, not corrected in place: a DEL is the
	// only safe invalidation (see internal/balance).
	if got, err := rdb.Raw().Get(ctx, key).Int64(); err != goredis.Nil {
		t.Fatalf("mirror after approve = %d (%v), want redis.Nil (deleted)", got, err)
	}
	// The next read repopulates it from the source.
	if err := rdb.Raw().Set(ctx, key, 150000, 60*time.Second).Err(); err != nil {
		t.Fatalf("refill mirror: %v", err)
	}
	t.Logf("B15: adjust=%d approved, balance=150000, K26 %s deleted", created.BalanceAdjust.ID, key)
}

// TestBalanceRedisNilIsSafe pins the no-Fund / no-Redis shape: invalidation is
// best-effort and must never panic or fail a committed request.
func TestBalanceRedisNilIsSafe(t *testing.T) {
	svc := &Service{}
	if got := svc.balanceRedis(); got != nil {
		t.Fatalf("balanceRedis on an empty Service = %v, want nil", got)
	}
	svc.invalidateBalanceMirror(context.Background(), 42) // must not panic
}
