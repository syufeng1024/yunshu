package settle

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/platform/clock"
)

// rulesYAML is the alarm-rule file that repeats the derived threshold. PromQL
// cannot import a Go constant, so the literal lives in both places and this
// test is what keeps them equal (G24). Path is relative to internal/settle.
func rulesYAML(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "obs", "alert", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	return string(raw)
}

// TestBacklogWarnThresholdMatchesAlertRule fails when someone moves
// settle.BacklogWarnThreshold without moving the PromQL threshold, or the other
// way round. Without it the alert silently fires late (or never).
func TestBacklogWarnThresholdMatchesAlertRule(t *testing.T) {
	if BacklogWarnThreshold >= BacklogThreshold {
		t.Fatalf("BacklogWarnThreshold = %d; it must sit strictly below the backpressure line %d, "+
			"otherwise the rule repeats the lagging signal it is meant to preempt",
			BacklogWarnThreshold, BacklogThreshold)
	}
	if want := BacklogThreshold * 9 / 10; BacklogWarnThreshold != want {
		t.Fatalf("BacklogWarnThreshold = %d; the documented derivation is 90%% of %d = %d",
			BacklogWarnThreshold, BacklogThreshold, want)
	}

	yaml := rulesYAML(t)
	rule := alertBlock(t, yaml, "SettleBacklogOverThreshold")
	wantExpr := "max(gw_settle_backlog_tasks) > " + strconv.Itoa(BacklogWarnThreshold)
	expr := ruleExpr(t, rule)
	if !strings.Contains(expr, wantExpr) {
		t.Fatalf("rule SettleBacklogOverThreshold expr = %q; want it to contain %q so the gauge "+
			"and the threshold stay in sync (G24). Rule block:\n%s", expr, wantExpr, rule)
	}
	// The old rule keyed on the backpressure counter, which only moves *after*
	// the queue has already crossed 5000. Keeping it in the expression would
	// make this alert lagging again without any test noticing.
	if strings.Contains(expr, "gw_backpressure_rejected_total") {
		t.Fatalf("SettleBacklogOverThreshold expr = %q still keys on the backpressure counter; "+
			"that signal only becomes true after the queue is already shedding traffic", expr)
	}

	if !strings.Contains(strings.ToLower(rule), "settle.pendingcountsql") {
		// Provenance only; the SQL itself is asserted below. A failure here
		// means someone lost the "single query" note from the rule.
		t.Logf("note: rule no longer mentions settle.PendingCountSQL provenance")
	}
}

// TestPendingCountIsTheOnlyBacklogQuery guards the "one definition, no second
// barrel" rule: the gateway gate and the jobsd gauge must execute
// PendingCountSQL, not their own copies of the statement.
func TestPendingCountIsTheOnlyBacklogQuery(t *testing.T) {
	files := map[string]string{
		"backlog.go":       "internal/settle/backlog.go",
		"obs_sample.go":    "internal/jobs/obs_sample.go",
		"gatewayd_main.go": "cmd/gatewayd/main.go",
	}
	// The literal statement may appear exactly once in non-test Go code: in the
	// PendingCountSQL constant.
	hits := 0
	for name, path := range files {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		hits += strings.Count(string(raw), "SELECT COUNT(*) FROM t_settle_task WHERE state = ?")
	}
	if hits != 1 {
		t.Fatalf("the PENDING-count statement appears %d times in %v; it must appear exactly "+
			"once (settle.PendingCountSQL) so the gauge and the capacity gate cannot drift", hits, files)
	}

	// And the producer must go through the shared helper.
	sample, err := os.ReadFile(filepath.Join("..", "..", "internal", "jobs", "obs_sample.go"))
	if err != nil {
		t.Fatalf("read obs_sample.go: %v", err)
	}
	if !strings.Contains(string(sample), "settle.CountPending(") {
		t.Fatalf("jobs.ObsSample does not call settle.CountPending; it must not own a second query")
	}
}

// TestCountPendingMatchesBacklogReader proves the two call sites return the
// same number against the real schema, so the gauge cannot report a different
// backlog than the one that sheds traffic.
func TestCountPendingMatchesBacklogReader(t *testing.T) {
	ctx := t.Context()
	before, err := CountPending(ctx, testDB)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}

	rid := newBacklogTask(t, ctx)
	defer func() {
		if _, err := testDB.ExecContext(ctx, `DELETE FROM t_settle_task WHERE request_id = ?`, rid); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}()

	after, err := CountPending(ctx, testDB)
	if err != nil {
		t.Fatalf("CountPending after insert: %v", err)
	}
	if after != before+1 {
		t.Fatalf("CountPending = %d after one PENDING insert; want %d", after, before+1)
	}

	// The gateway's cached reader must agree on the same instant.
	backlog := NewBacklog(testDB, clock.Real{})
	cached, err := backlog.SettleBacklog(ctx)
	if err != nil {
		t.Fatalf("SettleBacklog: %v", err)
	}
	if cached != after {
		t.Fatalf("SettleBacklog = %d; CountPending = %d — the alert and the gate disagree", cached, after)
	}
	t.Logf("backlog: before=%d after=%d cached=%d (same statement)", before, after, cached)
}

// newBacklogTask inserts one PENDING settlement task under a unique request id.
func newBacklogTask(t *testing.T, ctx context.Context) string {
	t.Helper()
	rid := newRequestID(t)
	now := time.Now().UTC()
	_, err := testDB.ExecContext(ctx, `
		INSERT INTO t_settle_task (request_id, user_id, task_type, payload, state,
		                           next_retry_at, created_at, updated_at)
		VALUES (?, 1, ?, JSON_OBJECT(), ?, ?, ?, ?)`,
		rid, TaskTypeSettle, TaskStatePending, now, now, now)
	if err != nil {
		t.Fatalf("insert settle task: %v", err)
	}
	return rid
}

// alertBlock returns the text of one rule, from its "- alert:" line up to the
// next rule or the end of the file. Deliberately textual: the file is a
// human-maintained artifact and this test must not depend on a YAML library
// the module does not ship.
func alertBlock(t *testing.T, yaml, name string) string {
	t.Helper()
	start := strings.Index(yaml, "- alert: "+name)
	if start < 0 {
		t.Fatalf("rule %s not found in rules.yaml", name)
	}
	rest := yaml[start+1:]
	if next := strings.Index(rest, "\n      - alert: "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// ruleExpr extracts the expr line of a rule block. PromQL may be folded over
// several lines (the ">-" form), so the value is joined and whitespace-collapsed.
func ruleExpr(t *testing.T, block string) string {
	t.Helper()
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "expr:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "expr:"))
		// A folded/literal block scalar continues on more-indented lines.
		if value == ">-" || value == "|" || value == ">" {
			value = ""
			for _, cont := range lines[i+1:] {
				if strings.TrimSpace(cont) == "" {
					break
				}
				// Continuation lines are indented deeper than "expr:".
				if len(cont)-len(strings.TrimLeft(cont, " ")) <= 8 {
					break
				}
				value += " " + strings.TrimSpace(cont)
			}
		}
		return strings.Join(strings.Fields(value), " ")
	}
	t.Fatalf("no expr found in rule block:\n%s", block)
	return ""
}

