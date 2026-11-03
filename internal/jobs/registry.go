// Package jobs is the background-task runtime shared by jobsd and the T09
// reconciliation tasks.
//
// The registry is metadata only: name, schedule, leader requirement, per-run
// timeout and concurrency policy. Task bodies arrive in T09's second segment.
// Every task that writes business data MUST set RequiresLeader; a task that
// reads only may run without the lease (T09 hard constraint 15).
package jobs

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Concurrency policies. SERIAL is the only policy V1.0 implements; a second
// policy must be added deliberately, not inferred.
const (
	ConcurrencySerial = "SERIAL"
)

// Task is one registered job.
type Task struct {
	Name string
	// Cron is the schedule expression when Period is zero (e.g. "0 5 0 * * *").
	Cron string
	// Period is the interval schedule. Exactly one of Period / Cron is set.
	Period time.Duration
	// RequiresLeader must be false only for a pure-read task.
	RequiresLeader bool
	// Timeout bounds one execution. Zero means DefaultTaskTimeout.
	Timeout time.Duration
	// Concurrency is SERIAL by default.
	Concurrency string
}

// DefaultTaskTimeout bounds a task that did not declare one.
const DefaultTaskTimeout = 5 * time.Minute

// Registry stores tasks by name.
type Registry struct {
	mu    sync.RWMutex
	order []string
	tasks map[string]Task
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{tasks: map[string]Task{}}
}

// Register validates and adds one task. Re-registering the same name is an
// error: two definitions of one job would let one silently shadow the other.
func (r *Registry) Register(t Task) error {
	if r == nil {
		return errors.New("jobs: nil registry")
	}
	if t.Name == "" {
		return errors.New("jobs: task name required")
	}
	if t.Period <= 0 && t.Cron == "" {
		return fmt.Errorf("jobs: task %s needs a period or cron", t.Name)
	}
	if t.Period > 0 && t.Cron != "" {
		return fmt.Errorf("jobs: task %s must use period or cron, not both", t.Name)
	}
	if t.Timeout < 0 {
		return fmt.Errorf("jobs: task %s timeout must be positive", t.Name)
	}
	if t.Timeout == 0 {
		t.Timeout = DefaultTaskTimeout
	}
	if t.Concurrency == "" {
		t.Concurrency = ConcurrencySerial
	}
	if t.Concurrency != ConcurrencySerial {
		return fmt.Errorf("jobs: task %s unknown concurrency %q", t.Name, t.Concurrency)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tasks[t.Name]; exists {
		return fmt.Errorf("jobs: task %s already registered", t.Name)
	}
	r.tasks[t.Name] = t
	r.order = append(r.order, t.Name)
	return nil
}

// List returns the tasks in registration order.
func (r *Registry) List() []Task {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Task, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tasks[name])
	}
	return out
}

// Names returns the registered names in registration order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Sorted returns the tasks sorted by name, for deterministic logging.
func (r *Registry) Sorted() []Task {
	out := r.List()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns one task.
func (r *Registry) Lookup(name string) (Task, bool) {
	if r == nil {
		return Task{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[name]
	return t, ok
}

// Len returns the number of registered tasks.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// TaskTable is the T09 second-segment job table. Every task has a side effect,
// so every task requires the t_job_lock lease (T09 hard constraint 15).
//
// The one exception is the gauge sampler below: it only reads two tables and
// exports the result, so it must run on every replica rather than on whichever
// one currently holds the lease. A leader-only sampler would disappear from
// /metrics whenever the lease moved, i.e. exactly during the failover an
// operator is watching (G24/G25).
//
// Cron fields are second minute hour day-of-month month day-of-week, evaluated
// in UTC+8. purge_log is offset from daily_reconcile so the two heavy scans do
// not overlap. daily_quota runs after rollup and before daily_reconcile: R-05
// reads the rows it writes, so it must be done by 03:00.
func TaskTable() []Task {
	return []Task{
		{Name: "prehold_release", Period: 60 * time.Second, RequiresLeader: true, Timeout: 5 * time.Minute},
		{Name: "daily_reconcile", Cron: "0 0 3 * * *", RequiresLeader: true, Timeout: 30 * time.Minute},
		{Name: "expire_batch", Cron: "0 5 0 * * *", RequiresLeader: true, Timeout: 10 * time.Minute},
		{Name: "purge_log", Cron: "0 10 3 * * *", RequiresLeader: true, Timeout: 60 * time.Minute},
		{Name: "rollup", Cron: "0 20 0 * * *", RequiresLeader: true, Timeout: 30 * time.Minute},
		{Name: "daily_quota", Cron: "0 30 0 * * *", RequiresLeader: true, Timeout: 30 * time.Minute},
		// A 30-minute payment window has to be swept far more often than the
		// nightly tasks: the finance queue is read all day, and an order that
		// expired at 09:31 must not still look payable at 09:40.
		{Name: "close_expired_order", Period: 5 * time.Minute, RequiresLeader: true, Timeout: 5 * time.Minute},
		// 「用户关闭即删」: once log.body_mode is meta_only, every retained body
		// is a broken promise (TDS 4318 / B-67). It is a write, so it needs the
		// lease; 60s keeps the window between "operator switched it off" and
		// "bodies are gone" inside a minute, while a run that finds nothing
		// costs one index probe.
		{Name: PurgeBodyModeTaskName, Period: 60 * time.Second, RequiresLeader: true, Timeout: 10 * time.Minute},
		// Gauge samplers. 30s matches the alert group's evaluation interval,
		// so a sample is never staler than one rule evaluation; both queries
		// are covered by an index and cost less than the scrape itself.
		{Name: SettleBacklogTaskName, Period: 30 * time.Second, RequiresLeader: false, Timeout: time.Minute},
		{Name: TicketSLATaskName, Period: 30 * time.Second, RequiresLeader: false, Timeout: time.Minute},
	}
}
