package jobs

import (
	"context"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

func newTestElection(t *testing.T, instance string) *Election {
	t.Helper()
	rdb := redistest.NewClient(0)
	t.Cleanup(func() { _ = rdb.Close() })
	return NewElection(testDB, rdb, instance, nil, clock.Real{})
}

// Two instances contend for the same job; exactly one owns the lease. This is
// the property jobsd relies on: a task body would execute once cluster-wide.
func TestElectionTwoInstancesOnlyOneAcquires(t *testing.T) {
	ctx := context.Background()
	job := randomJob(t)
	t.Cleanup(func() { _, _ = testDB.Exec(`DELETE FROM t_job_lock WHERE job_name = ?`, job) })

	e1 := newTestElection(t, "instance-1")
	e2 := newTestElection(t, "instance-2")

	ok1, err := e1.Acquire(ctx, job)
	if err != nil || !ok1 {
		t.Fatalf("instance-1 acquire = %v, %v; want true, nil", ok1, err)
	}
	ok2, err := e2.Acquire(ctx, job)
	if err != nil {
		t.Fatalf("instance-2 acquire: %v", err)
	}
	if ok2 {
		t.Fatal("instance-2 acquired a lease instance-1 still holds")
	}

	owner, leaseUntil, released, err := e1.Holder(ctx, job)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	if owner != "instance-1" {
		t.Fatalf("owner = %q, want instance-1", owner)
	}
	if released.Valid {
		t.Fatal("released_at set on an owned lease")
	}
	if remaining := time.Until(leaseUntil); remaining < 60*time.Second {
		t.Fatalf("lease remaining = %s, want >= 60s for a live owner", remaining)
	}

	// A non-owner must not renew.
	if ok, err := e2.Renew(ctx, job); err != nil || ok {
		t.Fatalf("instance-2 renew = %v, %v; want false, nil", ok, err)
	}
	if ok, err := e1.Renew(ctx, job); err != nil || !ok {
		t.Fatalf("instance-1 renew = %v, %v; want true, nil", ok, err)
	}
	t.Logf("election job=%s acquire: instance-1=%t instance-2=%t owner=%s remaining=%s",
		job, ok1, ok2, owner, time.Until(leaseUntil).Truncate(time.Second))
}

// The owner stops renewing; once less than TakeoverMargin remains (the 60s
// silence point), another instance may take over.
func TestElectionTakeoverAfterSilence(t *testing.T) {
	ctx := context.Background()
	job := randomJob(t)
	t.Cleanup(func() { _, _ = testDB.Exec(`DELETE FROM t_job_lock WHERE job_name = ?`, job) })

	e1 := newTestElection(t, "instance-1")
	e2 := newTestElection(t, "instance-2")
	if ok, err := e1.Acquire(ctx, job); err != nil || !ok {
		t.Fatalf("instance-1 acquire = %v, %v", ok, err)
	}

	// Live lease: no takeover.
	if ok, err := e2.Acquire(ctx, job); err != nil || ok {
		t.Fatalf("takeover on a live lease = %v, %v; want false, nil", ok, err)
	}

	// 60s of silence leaves 30s; at that boundary a second instance may take
	// over. The owner's next renew then reports "lost".
	if _, err := testDB.Exec(
		`UPDATE t_job_lock SET lease_until = ?, updated_at = ? WHERE job_name = ?`,
		time.Now().UTC().Add(10*time.Second), time.Now().UTC(), job,
	); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	ok, err := e2.Acquire(ctx, job)
	if err != nil {
		t.Fatalf("instance-2 takeover: %v", err)
	}
	if !ok {
		t.Fatal("instance-2 did not take over an aged lease")
	}
	if ok, err := e1.Renew(ctx, job); err != nil || ok {
		t.Fatalf("old owner renew = %v, %v; want false, nil", ok, err)
	}
	owner, _, _, err := e2.Holder(ctx, job)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	if owner != "instance-2" {
		t.Fatalf("owner = %q, want instance-2", owner)
	}
}

func TestElectionReleaseLetsTheOtherInstanceAcquire(t *testing.T) {
	ctx := context.Background()
	job := randomJob(t)
	t.Cleanup(func() { _, _ = testDB.Exec(`DELETE FROM t_job_lock WHERE job_name = ?`, job) })

	e1 := newTestElection(t, "instance-1")
	e2 := newTestElection(t, "instance-2")
	if ok, err := e1.Acquire(ctx, job); err != nil || !ok {
		t.Fatalf("instance-1 acquire = %v, %v", ok, err)
	}
	if err := e1.Release(ctx, job); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := e2.Acquire(ctx, job); err != nil || !ok {
		t.Fatalf("instance-2 acquire after release = %v, %v", ok, err)
	}
}

func TestHeartbeatWritesK25WithTTL(t *testing.T) {
	ctx := context.Background()
	e := newTestElection(t, "instance-hb")
	if err := e.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	key := redisx.JobHeartbeatKey("instance-hb")
	ttl, err := e.Redis.Raw().TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > DefaultHeartbeatTTL {
		t.Fatalf("heartbeat TTL = %s, want (0, %s]", ttl, DefaultHeartbeatTTL)
	}
}
