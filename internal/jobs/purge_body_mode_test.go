package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/platform/clock"
)

// seedBodyRow plants one t_request_body row. The ciphertext is opaque to this
// task (it only deletes), so a short placeholder is enough.
func seedBodyRow(t *testing.T, uid int64, requestID string, createdAt time.Time) {
	t.Helper()
	_, err := testDB.ExecContext(context.Background(), `
		INSERT INTO t_request_body
			(request_id, user_id, ciphertext, body_key_id, created_at, expires_at)
		VALUES (?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE ciphertext = VALUES(ciphertext)`,
		requestID, uid, []byte("v1:placeholder"), "body-v1", createdAt, createdAt.AddDate(0, 0, 180))
	if err != nil {
		t.Fatalf("seed body %s: %v", requestID, err)
	}
}

func deleteBodyRows(t *testing.T) {
	t.Helper()
	if _, err := testDB.Exec(`DELETE FROM t_request_body`); err != nil {
		t.Fatalf("clear bodies: %v", err)
	}
}

func countBodyRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(`SELECT COUNT(*) FROM t_request_body`).Scan(&n); err != nil {
		t.Fatalf("count bodies: %v", err)
	}
	return n
}

func countAuditRows(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM t_admin_audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func clearAuditRows(t *testing.T, action string) {
	t.Helper()
	if _, err := testDB.Exec(`DELETE FROM t_admin_audit_log WHERE action = ?`, action); err != nil {
		t.Fatalf("clear audit rows: %v", err)
	}
}

// newBodyPurgeTask builds the task with a fixed mode, so the test never depends
// on t_config_item's contents.
func newBodyPurgeTask(t *testing.T, mode string, modeErr error) *PurgeBodiesOnMetaOnly {
	t.Helper()
	task := NewPurgeBodiesOnMetaOnly(testDB, NewPurgeLog(testDB, nil, clock.Real{}),
		&audit.Writer{DB: testDB, Now: clock.Real{}}, nil, clock.Real{})
	task.BodyMode = func(context.Context) (string, error) {
		if modeErr != nil {
			return "", modeErr
		}
		return mode, nil
	}
	return task
}

// TestPurgeBodiesOnMetaOnlyDeletesEverything is the acceptance case for G2:
// with log.body_mode=meta_only every retained body is a broken promise
// ("用户关闭即删"), so the run must remove all of them — regardless of
// expires_at, which is what the 180-day sweep keys on.
func TestPurgeBodiesOnMetaOnlyDeletesEverything(t *testing.T) {
	deleteBodyRows(t)
	clearAuditRows(t, PurgeBodyModeAction)
	t.Cleanup(func() {
		deleteBodyRows(t)
		clearAuditRows(t, PurgeBodyModeAction)
	})

	now := time.Now().UTC()
	// Fresh rows: nowhere near their retention date. The point of the task is
	// that they must still go.
	seedBodyRow(t, 90001, jobRID("g2a", 1), now)
	seedBodyRow(t, 90001, jobRID("g2a", 2), now)
	seedBodyRow(t, 90002, jobRID("g2a", 3), now.AddDate(0, 0, -30))
	if got := countBodyRows(t); got != 3 {
		t.Fatalf("seeded %d bodies, want 3", got)
	}

	task := newBodyPurgeTask(t, config.EnumMetaOnly, nil)
	res, err := task.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Skipped {
		t.Fatalf("run reported Skipped under meta_only: %+v", res)
	}
	if res.Deleted != 3 {
		t.Fatalf("Deleted = %d, want 3 (%+v)", res.Deleted, res)
	}
	if got := countBodyRows(t); got != 0 {
		t.Fatalf("%d bodies survived a meta_only run; the promise is delete-on-close", got)
	}

	// One audit row, SYSTEM, with the count as the result.
	var (
		role   string
		module string
		result string
		target string
	)
	if err := testDB.QueryRow(`
		SELECT operator_role, module, result, target_type
		  FROM t_admin_audit_log WHERE action = ? ORDER BY id DESC LIMIT 1`,
		PurgeBodyModeAction).Scan(&role, &module, &result, &target); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if role != "SYSTEM" || module != PurgeBodyModeAuditModule || target != "t_request_body" {
		t.Fatalf("audit row role=%q module=%q target=%q; want SYSTEM/%s/t_request_body",
			role, module, target, PurgeBodyModeAuditModule)
	}
	if result != "n=3" {
		t.Fatalf("audit result = %q; want the deleted count (n=3)", result)
	}
	if got := countAuditRows(t, PurgeBodyModeAction); got != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 for one run", got)
	}
	t.Logf("meta_only run: deleted=%d batches=%d audit=%q", res.Deleted, res.Batches, result)
}

// TestPurgeBodiesOnMetaOnlySkipsOptIn is the other half of the mode predicate:
// a tenant that asked us to keep bodies must keep them.
func TestPurgeBodiesOnMetaOnlySkipsOptIn(t *testing.T) {
	deleteBodyRows(t)
	clearAuditRows(t, PurgeBodyModeAction)
	t.Cleanup(func() {
		deleteBodyRows(t)
		clearAuditRows(t, PurgeBodyModeAction)
	})
	seedBodyRow(t, 90003, jobRID("g2b", 1), time.Now().UTC())

	task := newBodyPurgeTask(t, config.EnumOptIn, nil)
	res, err := task.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Skipped || res.Deleted != 0 {
		t.Fatalf("opt_in run = %+v; want Skipped with nothing deleted", res)
	}
	if got := countBodyRows(t); got != 1 {
		t.Fatalf("%d bodies left after an opt_in run, want 1", got)
	}
	if got := countAuditRows(t, PurgeBodyModeAction); got != 0 {
		t.Fatalf("audit rows = %d after a skipped run, want 0", got)
	}
}

// TestPurgeBodiesOnMetaOnlyFailSafeOnConfigError: an unreadable mode must never
// be read as meta_only. Deleting a body the product promised to keep is
// unrecoverable; keeping it a minute longer is not.
func TestPurgeBodiesOnMetaOnlyFailSafeOnConfigError(t *testing.T) {
	deleteBodyRows(t)
	t.Cleanup(func() { deleteBodyRows(t) })
	seedBodyRow(t, 90004, jobRID("g2c", 1), time.Now().UTC())

	task := newBodyPurgeTask(t, "", errors.New("config store unavailable"))
	res, err := task.Run(t.Context())
	if err == nil {
		t.Fatalf("run with an unreadable log.body_mode returned no error: %+v", res)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted %d rows with an unreadable mode", res.Deleted)
	}
	if got := countBodyRows(t); got != 1 {
		t.Fatalf("%d bodies left, want 1 (fail-safe: do not delete)", got)
	}
}

// TestPurgeBodiesOnMetaOnlyNoAuditOnZeroRows keeps the trail readable: a run
// that deletes nothing must not append a "deleted 0" row every minute.
func TestPurgeBodiesOnMetaOnlyNoAuditOnZeroRows(t *testing.T) {
	deleteBodyRows(t)
	clearAuditRows(t, PurgeBodyModeAction)
	t.Cleanup(func() { clearAuditRows(t, PurgeBodyModeAction) })

	task := newBodyPurgeTask(t, config.EnumMetaOnly, nil)
	for i := 0; i < 2; i++ {
		if _, err := task.Run(t.Context()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := countAuditRows(t, PurgeBodyModeAction); got != 0 {
		t.Fatalf("audit rows = %d after two empty runs, want 0", got)
	}
}

// TestPurgeBodiesOnMetaOnlyReusesTheSharedBatchLoop pins the "no second batched
// delete loop" requirement: a backlog is deleted in PurgeLog.purge slices, so
// one run cannot issue a single unbounded DELETE against the primary.
func TestPurgeBodiesOnMetaOnlyReusesTheSharedBatchLoop(t *testing.T) {
	deleteBodyRows(t)
	t.Cleanup(func() { deleteBodyRows(t) })

	const rows = 12
	now := time.Now().UTC()
	for i := 0; i < rows; i++ {
		seedBodyRow(t, 90010, jobRID("g2d", i), now)
	}

	task := newBodyPurgeTask(t, config.EnumMetaOnly, nil)
	task.BatchSize = 5
	res, err := task.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Deleted != rows {
		t.Fatalf("Deleted = %d, want %d", res.Deleted, rows)
	}
	if res.Batches != 3 {
		t.Fatalf("Batches = %d over %d rows at batch=5; want 3 (5+5+2, stop on the short batch)",
			res.Batches, rows)
	}
	if res.Truncated {
		t.Fatalf("Truncated = true; 3 batches is below MaxBatches")
	}
	if got := countBodyRows(t); got != 0 {
		t.Fatalf("%d bodies left, want 0", got)
	}
	// One audit row for the whole run, not one per batch.
	if got := countAuditRows(t, PurgeBodyModeAction); got != 1 {
		t.Fatalf("audit rows = %d for a batched run, want 1", got)
	}
}

// TestPurgeBodyModeActionFitsColumn keeps the audit action inside
// t_admin_audit_log.action (varchar(32)). An overlong value would be truncated
// by MySQL and the trail would stop matching the code.
func TestPurgeBodyModeActionFitsColumn(t *testing.T) {
	if len(PurgeBodyModeAction) > 32 {
		t.Fatalf("action %q is %d chars; the column is varchar(32)", PurgeBodyModeAction, len(PurgeBodyModeAction))
	}
}

// TestEffectiveBodyModeReadsTheRegistry proves the production seam resolves
// log.body_mode through the same registry defaults the gateway uses, so an
// unrelated t_config_item (or an empty one) still yields a value.
func TestEffectiveBodyModeReadsTheRegistry(t *testing.T) {
	mode, err := effectiveBodyMode(t.Context(), testDB)
	if err != nil {
		// The jobs schema is migrated, so this is a real failure, not a skip.
		t.Fatalf("effectiveBodyMode: %v", err)
	}
	if mode != config.EnumMetaOnly && mode != config.EnumOptIn {
		t.Fatalf("effective log.body_mode = %q; want one of the two enum values", mode)
	}
	t.Logf("effective log.body_mode in the jobs schema = %q", mode)
}

// TestPurgeBodyModeRegisteredAsLeaderTask: deleting bodies is a write, so the
// task must not run on every replica.
func TestPurgeBodyModeRegisteredAsLeaderTask(t *testing.T) {
	reg := NewRegistry()
	for _, task := range TaskTable() {
		if err := reg.Register(task); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	task, ok := reg.Lookup(PurgeBodyModeTaskName)
	if !ok {
		t.Fatalf("%s is not registered", PurgeBodyModeTaskName)
	}
	if !task.RequiresLeader {
		t.Fatalf("%s must require the leader lease: it deletes rows", PurgeBodyModeTaskName)
	}
	if task.Period <= 0 || task.Cron != "" {
		t.Fatalf("%s must be interval-scheduled, got period=%s cron=%q", PurgeBodyModeTaskName, task.Period, task.Cron)
	}
	// Within a minute of the operator switching the mode off, the bodies are
	// gone; anything slower is a promise we cannot describe to a customer.
	if task.Period > time.Minute {
		t.Fatalf("%s period = %s; want <= 1m", PurgeBodyModeTaskName, task.Period)
	}
	t.Logf("%s: period=%s leader=%t", PurgeBodyModeTaskName, task.Period, task.RequiresLeader)
}

// TestPurgeBodiesOnMetaOnlyRequiresAudit is the "no silent deletion" guard: a
// task configured without an audit sink must refuse to run rather than delete
// rows nobody can attribute.
func TestPurgeBodiesOnMetaOnlyRequiresAudit(t *testing.T) {
	deleteBodyRows(t)
	t.Cleanup(func() { deleteBodyRows(t) })
	seedBodyRow(t, 90020, jobRID("g2e", 1), time.Now().UTC())

	task := NewPurgeBodiesOnMetaOnly(testDB, NewPurgeLog(testDB, nil, clock.Real{}), nil, nil, clock.Real{})
	task.BodyMode = func(context.Context) (string, error) { return config.EnumMetaOnly, nil }
	if _, err := task.Run(t.Context()); err == nil {
		t.Fatalf("task without an audit writer ran; a deletion with no trail must be refused")
	}
	if got := countBodyRows(t); got != 1 {
		t.Fatalf("%d bodies left, want 1 (refused before deleting)", got)
	}
}
