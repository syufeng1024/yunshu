package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/control/ticket"
	obsmetrics "yunshu.dev/core/internal/obs/metrics"
	"yunshu.dev/core/internal/platform/clock"
)

// insertTicket plants one ticket with an explicit first-response deadline and
// state, so the sampler's predicate can be exercised in both directions.
func insertTicket(t *testing.T, no string, state string, dueOffset time.Duration, responded bool) {
	t.Helper()
	now := time.Now().UTC()
	due := now.Add(dueOffset)
	var respondedAt any
	if responded {
		respondedAt = now.Add(dueOffset - time.Hour)
	}
	if _, err := testDB.Exec(`
		INSERT INTO t_ticket (ticket_no, user_id, ticket_type, title, description, state,
		                      first_response_due_at, first_responded_at, created_at, updated_at)
		VALUES (?, 1, 'TECH', 'sla gauge fixture', 'fixture', ?, ?, ?, ?, ?)`,
		no, state, due, respondedAt, now, now); err != nil {
		t.Fatalf("insert ticket: %v", err)
	}
}

func deleteTicket(t *testing.T, no string) {
	t.Helper()
	if _, err := testDB.Exec(`DELETE FROM t_ticket WHERE ticket_no = ?`, no); err != nil {
		t.Fatalf("cleanup ticket: %v", err)
	}
}

// TestObsSampleExportsTicketSLAOverdue drives the jobsd producer and reads the
// gauge back out of the Prometheus export, then proves the predicate is the
// documented one by moving three tickets in and out of the alert zone (G25).
func TestObsSampleExportsTicketSLAOverdue(t *testing.T) {
	reg := obsmetrics.NewRegistry()
	rec := obsmetrics.NewRecorder(reg)
	sampler := NewObsSample(testDB, rec, nil)
	sampler.Clock = clock.Real{}

	noOpen := randomJob(t)
	noAnswered := randomJob(t)
	noOverdue := randomJob(t)
	for _, no := range []string{noOpen, noAnswered, noOverdue} {
		deleteTicket(t, no)
		t.Cleanup(func() { deleteTicket(t, no) })
	}

	// Inside the window: OPEN but due in the future.
	insertTicket(t, noOpen, "OPEN", 2*time.Hour, false)
	// Past due but already answered: must NOT count (the clock stopped).
	insertTicket(t, noAnswered, "RESPONDED", -time.Hour, true)
	// The only row that belongs in the alert zone.
	insertTicket(t, noOverdue, "OPEN", -time.Hour, false)

	if err := sampler.RunTicketSLA(t.Context()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	text := samplePrometheus(reg)
	got := gaugeValue(text, "gw_ticket_sla_overdue")
	want, err := ticket.CountFirstResponseOverdue(t.Context(), testDB, time.Now().UTC())
	if err != nil {
		t.Fatalf("CountFirstResponseOverdue: %v", err)
	}
	if got != want {
		t.Fatalf("gw_ticket_sla_overdue = %d; the shared ticket.CountFirstResponseOverdue says %d\n%s", got, want, text)
	}
	if got < 1 {
		t.Fatalf("gw_ticket_sla_overdue = %d; the past-due unanswered OPEN ticket was not counted\n%s", got, text)
	}

	// Answer the overdue ticket: the gauge must fall by exactly one. A counter
	// could not do this, which is why the metric is a gauge.
	if _, err := testDB.Exec(`UPDATE t_ticket SET first_responded_at = ? WHERE ticket_no = ?`,
		time.Now().UTC(), noOverdue); err != nil {
		t.Fatalf("answer ticket: %v", err)
	}
	if err := sampler.RunTicketSLA(t.Context()); err != nil {
		t.Fatalf("sample after answer: %v", err)
	}
	if after := gaugeValue(samplePrometheus(reg), "gw_ticket_sla_overdue"); after != got-1 {
		t.Fatalf("gw_ticket_sla_overdue = %d after answering one ticket; want %d", after, got-1)
	}
	t.Logf("gw_ticket_sla_overdue: %d -> %d after the reply (gauge, not counter)", got, got-1)
}

// TestTicketSLAOverdueRuleIsEnabled keeps the alert wired to the gauge. The
// rule used to be commented out with a "无埋点" note; re-commenting it (or
// renaming the metric) would silently retire the only automatic signal behind
// the 4-hour promise, and nothing else would fail.
func TestTicketSLAOverdueRuleIsEnabled(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "obs", "alert", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	yaml := string(raw)

	const name = "TicketSLAFirstResponseOverdue"
	lines := strings.Split(yaml, "\n")
	idx := -1
	for i, line := range lines {
		// The rule must be a real list item, not a comment: a commented-out
		// copy still contains "- alert: <name>" as a substring, which is
		// exactly the state this test exists to prevent.
		if strings.TrimSpace(line) == "- alert: "+name {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("TicketSLAFirstResponseOverdue is missing or still commented out; " +
			"the gauge would have no consumer (G25)")
	}
	block := strings.Join(lines[idx:], "\n")
	if next := strings.Index(block[1:], "\n      - alert: "); next >= 0 {
		block = block[:next+1]
	}
	if !strings.Contains(block, "max(gw_ticket_sla_overdue) > 0") {
		t.Fatalf("rule does not key on max(gw_ticket_sla_overdue) > 0:\n%s", block)
	}
	if n := strings.Count(yaml, name); n != 1 {
		t.Fatalf("%s appears %d times; a commented-out duplicate "+
			"would let the two copies drift", name, n)
	}
	if strings.Contains(yaml, "无埋点") {
		t.Logf("note: rules.yaml still mentions 无埋点 somewhere (expected only if a future rule needs it)")
	}
}

// TestFirstResponseOverdueSQLMatchesState guards the literal inside the shared
// statement: the SQL says state = 'OPEN' while Go says ticket.StateOpen.
func TestFirstResponseOverdueSQLMatchesState(t *testing.T) {
	if !strings.Contains(ticket.FirstResponseOverdueSQL, "state = '"+ticket.StateOpen+"'") {
		t.Fatalf("FirstResponseOverdueSQL does not filter state = '%s'; the alert zone would "+
			"silently include answered or closed tickets:\n%s", ticket.StateOpen, ticket.FirstResponseOverdueSQL)
	}
	if !strings.Contains(ticket.FirstResponseOverdueSQL, "first_responded_at IS NULL") ||
		!strings.Contains(ticket.FirstResponseOverdueSQL, "first_response_due_at < ?") {
		t.Fatalf("FirstResponseOverdueSQL lost part of the predicate:\n%s", ticket.FirstResponseOverdueSQL)
	}
}

// randomJob returns a 20-character unique id ("job_" + 16 hex), which is
// exactly the width of t_ticket.ticket_no.
