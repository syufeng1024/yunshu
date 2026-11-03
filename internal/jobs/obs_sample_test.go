package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/settle"
)

// samplePrometheus renders the registry the way /metrics would, so the test
// reads the same bytes the alert rule will see.
func samplePrometheus(reg *obsmetrics.Registry) string {
	return string(reg.PrometheusText())
}

// gaugeValue returns the exported value of a label-less gauge, or -1 when the
// series does not exist. "Series missing" is a distinct state on purpose: it
// means no sample has succeeded yet, which must not read as zero (G24).
func gaugeValue(text, name string) int64 {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, name+" ") {
			var v int64
			for i := len(name) + 1; i < len(line); i++ {
				c := line[i]
				if c < '0' || c > '9' {
					break
				}
				v = v*10 + int64(c-'0')
			}
			return v
		}
	}
	return -1
}

// insertPendingTask plants one PENDING settlement task so the sampler has a
// known backlog to count. The jobs package owns yunshu_jobs_test, so the count
// is only affected by this package's fixtures.
func insertPendingTask(t *testing.T, rid string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := testDB.Exec(`
		INSERT INTO t_settle_task (request_id, user_id, task_type, payload, state,
		                           next_retry_at, created_at, updated_at)
		VALUES (?, 1, ?, JSON_OBJECT(), ?, ?, ?, ?)`,
		rid, settle.TaskTypeSettle, settle.TaskStatePending, now, now, now); err != nil {
		t.Fatalf("insert settle task: %v", err)
	}
}

func deletePendingTask(t *testing.T, rid string) {
	t.Helper()
	if _, err := testDB.Exec(`DELETE FROM t_settle_task WHERE request_id = ?`, rid); err != nil {
		t.Fatalf("cleanup settle task: %v", err)
	}
}

// TestObsSampleExportsSettleBacklog drives the producer that jobsd runs and
// reads the number back out of the Prometheus export. Without this the rule
// gw_settle_backlog_tasks > 4500 would be another "data source does not exist"
// and stay green forever (G24).
func TestObsSampleExportsSettleBacklog(t *testing.T) {
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	sampler := NewObsSample(testDB, rec, nil)

	rid := jobRID("obsB", 1)
	deletePendingTask(t, rid)
	t.Cleanup(func() { deletePendingTask(t, rid) })

	// Baseline. A successful sample writes the gauge even when it is zero:
	// "measured zero" and "never measured" must be distinguishable, otherwise a
	// broken sampler looks like an empty queue.
	before, err := settle.CountPending(t.Context(), testDB)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if err := sampler.RunSettleBacklog(t.Context()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	text := samplePrometheus(reg)
	if got := gaugeValue(text, "gw_settle_backlog_tasks"); got != before {
		t.Fatalf("gw_settle_backlog_tasks = %d; want %d (the shared COUNT(*))\n%s", got, before, text)
	}

	insertPendingTask(t, rid)
	if err := sampler.RunSettleBacklog(t.Context()); err != nil {
		t.Fatalf("sample after insert: %v", err)
	}
	text = samplePrometheus(reg)
	if got, want := gaugeValue(text, "gw_settle_backlog_tasks"), before+1; got != want {
		t.Fatalf("gw_settle_backlog_tasks = %d after one PENDING insert; want %d\n%s", got, want, text)
	}
	t.Logf("gw_settle_backlog_tasks: %d -> %d (settle.PendingCountSQL)", before, before+1)
}

// TestObsSampleKeepsLastValueOnFailure is the fail-safe half: a DB that cannot
// be read must not publish zero. Zero would clear the capacity alert during
// exactly the incident it exists for.
func TestObsSampleKeepsLastValueOnFailure(t *testing.T) {
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	sampler := NewObsSample(testDB, rec, nil)

	if err := sampler.RunSettleBacklog(t.Context()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	good := gaugeValue(samplePrometheus(reg), "gw_settle_backlog_tasks")
	if good < 0 {
		t.Fatalf("gauge was not exported by a successful sample")
	}

	// A cancelled context makes the COUNT fail deterministically, whatever
	// host/schema the test DSN points at.
	deadCtx, cancel := context.WithCancel(t.Context())
	cancel()
	broken := NewObsSample(testDB, rec, nil)
	if err := broken.RunSettleBacklog(deadCtx); err == nil {
		t.Fatalf("sampler with an unusable DB returned no error; the value would have been overwritten with 0")
	}
	if got := gaugeValue(samplePrometheus(reg), "gw_settle_backlog_tasks"); got != good {
		t.Fatalf("gauge changed from %d to %d after a failed sample; it must keep the last known value", good, got)
	}
}

// TestObsSampleWithoutMetricsIsANoOp documents that the sampler never panics on
// a partially wired process (metrics optional, same convention as the recorder).
func TestObsSampleWithoutMetricsIsANoOp(t *testing.T) {
	sampler := NewObsSample(testDB, nil, nil)
	if err := sampler.RunSettleBacklog(t.Context()); err != nil {
		t.Fatalf("sample without metrics: %v", err)
	}
	var nilSampler *ObsSample
	if err := nilSampler.RunSettleBacklog(t.Context()); err != nil {
		t.Fatalf("nil sampler: %v", err)
	}
}
