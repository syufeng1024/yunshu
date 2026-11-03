package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
)

var wantTasks = []string{
	"close_expired_order",
	"daily_quota",
	"daily_reconcile",
	"expire_batch",
	"prehold_release",
	"purge_log",
	"rollup",
}

// wantReadOnlyTasks are the gauge samplers: pure reads, so they must NOT be
// gated on the t_job_lock lease (G24/G25). A leader-only sampler would stop
// exporting its gauge on whichever replica does not hold the lease.
var wantReadOnlyTasks = []string{
	SettleBacklogTaskName,
}

// Every T09 task has a side effect, so every row in TaskTable must require the
// t_job_lock lease and the runner must refuse to start without a body. The
// gauge samplers are the documented exception and are asserted the other way
// round: they must not require the lease.
func TestTaskTableWiring(t *testing.T) {
	reg := NewRegistry()
	for _, task := range TaskTable() {
		if err := reg.Register(task); err != nil {
			t.Fatalf("register %s: %v", task.Name, err)
		}
	}
	if got, want := reg.Len(), len(wantTasks)+len(wantReadOnlyTasks); got != want {
		t.Fatalf("task count = %d; want %d", got, want)
	}
	for _, name := range wantReadOnlyTasks {
		task, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("read-only task %s is not registered", name)
		}
		if task.RequiresLeader {
			t.Fatalf("task %s must NOT require the leader lease: it only reads and "+
				"exports a gauge, so gating it would silence the metric on followers", name)
		}
		if task.Period <= 0 || task.Cron != "" {
			t.Fatalf("task %s must be interval-scheduled, got period=%s cron=%q", name, task.Period, task.Cron)
		}
	}
	for _, name := range wantTasks {
		task, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("task %s is not registered", name)
		}
		if !task.RequiresLeader {
			t.Fatalf("task %s must RequiresLeader", name)
		}
		if task.Timeout <= 0 {
			t.Fatalf("task %s has no timeout", name)
		}
		if (task.Period > 0) == (task.Cron != "") {
			t.Fatalf("task %s must set exactly one of Period/Cron", name)
		}
	}
	// Daily tasks are CST-local: 03:00 reconcile, 03:10 purge, 00:05 expire,
	// 00:20 rollup, 00:30 daily quota (after rollup, before reconcile's R-05).
	for name, want := range map[string]string{
		"daily_reconcile": "0 0 3 * * *",
		"purge_log":       "0 10 3 * * *",
		"expire_batch":    "0 5 0 * * *",
		"rollup":          "0 20 0 * * *",
		"daily_quota":     "0 30 0 * * *",
	} {
		task, _ := reg.Lookup(name)
		if task.Cron != want {
			t.Fatalf("task %s cron = %q; want %q", name, task.Cron, want)
		}
	}

	runner := &Runner{
		Registry:  reg,
		Clock:     clock.Real{},
		Executors: map[string]TaskFunc{},
	}
	err := runner.Validate()
	if err == nil || !strings.Contains(err.Error(), "has no executor") {
		t.Fatalf("Validate without executors = %v; want missing-executor error", err)
	}
	for _, name := range append(append([]string{}, wantTasks...), wantReadOnlyTasks...) {
		runner.Executors[name] = func(context.Context, time.Time) error { return nil }
	}
	if err := runner.Validate(); err != nil {
		t.Fatalf("Validate with all executors: %v", err)
	}
	delete(runner.Executors, "purge_log")
	if err := runner.Validate(); err == nil || !strings.Contains(err.Error(), "purge_log") {
		t.Fatalf("Validate missing purge_log = %v; want error naming it", err)
	}
}

// Cron is evaluated in the business zone, not UTC. 00:20 CST is 16:20 UTC the
// previous day, so a UTC-based parser would fire eight hours late.
func TestCronFiresInBusinessZone(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 6, 10, 0, 0, 0, 0, cst)

	expr, err := parseCron("0 20 0 * * *")
	if err != nil {
		t.Fatalf("parse cron: %v", err)
	}
	next := expr.next(at)
	want := time.Date(2026, 6, 10, 0, 20, 0, 0, cst)
	if !next.Equal(want) {
		t.Fatalf("next = %s; want %s", next, want)
	}
	if next.UTC().Hour() != 16 || next.UTC().Day() != 9 {
		t.Fatalf("next UTC = %s; want 2026-06-09T16:20Z", next.UTC())
	}

	// A run at 00:20 must schedule tomorrow, not the same minute again.
	again := expr.next(want)
	if !again.Equal(want.AddDate(0, 0, 1)) {
		t.Fatalf("next after fire = %s; want %s", again, want.AddDate(0, 0, 1))
	}

	// Runner.nextRun uses Location; a UTC-located runner sees the same instant.
	r := &Runner{Clock: clock.NewFixed(at), Location: cst}
	if got := r.nextRun(Task{Cron: "0 20 0 * * *"}, at); !got.Equal(want) {
		t.Fatalf("runner next = %s; want %s", got, want)
	}
}

// Only one jobsd may hold a task lease. A run longer than the lease must keep
// renewing, otherwise the second instance takes over mid-execution.
func TestLongRunningTaskKeepsLease(t *testing.T) {
	ctx := context.Background()
	job := randomJob(t)
	cleanup := func() {
		if _, err := testDB.Exec(`DELETE FROM t_job_lock WHERE job_name = ?`, job); err != nil {
			t.Fatalf("cleanup lock: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	newElection := func(instance string) *Election {
		e := NewElection(testDB, nil, instance, nil, clock.Real{})
		e.LeaseTTL = time.Second
		e.RenewEvery = 100 * time.Millisecond
		e.TakeoverMargin = time.Millisecond
		return e
	}
	owner := newElection("owner-1")
	other := newElection("owner-2")

	task := Task{Name: job, RequiresLeader: true, Timeout: 10 * time.Second}
	runner := &Runner{Clock: clock.Real{}, Election: owner, Location: time.UTC}
	// leaderLoop acquires before execute; take the lease here so execute's
	// renewal goroutine has a row to extend.
	if ok, err := owner.Acquire(ctx, job); err != nil || !ok {
		t.Fatalf("owner acquire: %v %v", ok, err)
	}

	executed := false
	runner.execute(ctx, task, func(ctx context.Context, _ time.Time) error {
		executed = true
		// Outlive several renewal intervals; without in-task renewal the lease
		// expires and other could take over while this body is still running.
		time.Sleep(1500 * time.Millisecond)
		_, _, released, err := other.Holder(ctx, job)
		if err != nil {
			t.Errorf("holder during run: %v", err)
		}
		_ = released
		ok, err := other.Acquire(ctx, job)
		if err != nil {
			t.Errorf("second acquire during run: %v", err)
		}
		if ok {
			t.Errorf("second instance acquired %s while the first was still running", job)
		}
		return nil
	}, time.Now().UTC())

	if !executed {
		t.Fatal("executor did not run")
	}
	name, leaseUntil, _, err := owner.Holder(ctx, job)
	if err != nil {
		t.Fatalf("holder after run: %v", err)
	}
	if name != "owner-1" {
		t.Fatalf("owner after run = %q; want owner-1", name)
	}
	if !leaseUntil.After(time.Now().UTC()) {
		t.Fatalf("lease_until = %s; want in the future after renewal", leaseUntil)
	}
	t.Logf("lease owner=%s until=%s remaining=%s", name,
		leaseUntil.UTC().Format(time.RFC3339), time.Until(leaseUntil).Truncate(time.Millisecond))
}
