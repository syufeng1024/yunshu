package idemstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

// TestMain opens the real MySQL schema for the durable-leg tests and the real
// Redis database for the fast-path tests. The Redis fixture runs m and owns the
// cross-binary lock, so the MySQL setup/teardown brackets it.
func TestMain(m *testing.M) {
	if err := setupMySQL(); err != nil {
		fmt.Fprintf(os.Stderr, "idemstore tests: %v\n", err)
		os.Exit(1)
	}
	code := redistest.Setup(m)
	if testDB != nil {
		_ = testDB.Close()
	}
	os.Exit(code)
}

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T, client *redisx.Client, clock *redisx.Clock, db DB) *Store {
	t.Helper()
	if clock == nil {
		clock = redisx.NewStaticClock(testNow)
	}
	return New(client, clock, db, DefaultOptions(), nil)
}

func newClient(t *testing.T) *redisx.Client {
	t.Helper()
	client := redistest.NewClient(0)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func flushRedis(t *testing.T) {
	t.Helper()
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

type memDB struct {
	mu   sync.Mutex
	rows map[string]*Record
}

func newMemDB() *memDB { return &memDB{rows: make(map[string]*Record)} }

func memKey(scope string, apiKeyID int64, idemKey string) string {
	return fmt.Sprintf("%s|%d|%s", scope, apiKeyID, idemKey)
}

func cloneRecord(rec *Record) *Record {
	out := *rec
	out.ResponseBody = append([]byte(nil), rec.ResponseBody...)
	return &out
}

func (d *memDB) Begin(_ context.Context, rec Record) (*Record, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := memKey(rec.Scope, rec.APIKeyID, rec.IdemKey)
	if existing, ok := d.rows[key]; ok {
		return cloneRecord(existing), false, nil
	}
	stored := rec
	d.rows[key] = &stored
	return nil, true, nil
}

func (d *memDB) Get(_ context.Context, scope string, apiKeyID int64, idemKey string) (*Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.rows[memKey(scope, apiKeyID, idemKey)]
	if !ok {
		return nil, ErrRecordNotFound
	}
	return cloneRecord(existing), nil
}

func (d *memDB) Complete(_ context.Context, rec Record, state State, body []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.rows[memKey(rec.Scope, rec.APIKeyID, rec.IdemKey)]
	if !ok {
		return ErrRecordNotFound
	}
	if existing.FenceToken != rec.FenceToken || existing.RequestID != rec.RequestID {
		return ErrFenceInvalid
	}
	existing.State = state
	existing.ResponseBody = append([]byte(nil), body...)
	return nil
}

func (d *memDB) Renew(_ context.Context, rec Record, until time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.rows[memKey(rec.Scope, rec.APIKeyID, rec.IdemKey)]
	if !ok {
		return ErrRecordNotFound
	}
	if existing.FenceToken != rec.FenceToken || existing.RequestID != rec.RequestID {
		return ErrFenceInvalid
	}
	existing.LeaseExpiresAt = until
	return nil
}

func (d *memDB) Takeover(_ context.Context, rec Record, oldFence string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := memKey(rec.Scope, rec.APIKeyID, rec.IdemKey)
	existing, ok := d.rows[key]
	if !ok || existing.FenceToken != oldFence {
		return false, nil
	}
	stored := rec
	d.rows[key] = &stored
	return true, nil
}

func beginInput(requestID, bodyHash, fence string) BeginInput {
	return BeginInput{
		Scope: "idem", APIKeyID: 7, UserID: 70, IdemKey: "client-key",
		BodyHash: bodyHash, RequestID: requestID, FenceToken: fence,
	}
}

func TestA10ReplayWaitAndReuse(t *testing.T) {
	flushRedis(t)
	store := newStore(t, newClient(t), nil, newMemDB())

	first, err := store.Begin(t.Context(), beginInput("r1", "h1", "f1"))
	if err != nil || first.Action != ActionNew {
		t.Fatalf("first begin = %+v err=%v", first, err)
	}
	second, err := store.Begin(t.Context(), beginInput("r2", "h1", "f2"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != ActionWait {
		t.Fatalf("second in-flight begin = %+v want WAIT", second)
	}
	if got := DecideReplay(second, false); got.Status != 409 || got.Code != "request_in_flight" {
		t.Fatalf("wait decision = %+v", got)
	}

	if err := store.Complete(t.Context(), *first.Record, StateDone, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Begin(t.Context(), beginInput("r3", "h1", "f3"))
	if err != nil || replay.Action != ActionReplay {
		t.Fatalf("replay begin = %+v err=%v", replay, err)
	}
	decision := DecideReplay(replay, false)
	if decision.Status != 200 || string(decision.Body) != `{"ok":true}` {
		t.Fatalf("non-stream replay = %+v", decision)
	}
	if streamDecision := DecideReplay(replay, true); streamDecision.Status != 409 {
		t.Fatalf("stream replay = %+v want 409", streamDecision)
	}

	reused, err := store.Begin(t.Context(), beginInput("r4", "h2", "f4"))
	if err != nil || reused.Action != ActionKeyReused {
		t.Fatalf("different body = %+v err=%v", reused, err)
	}
	if got := DecideReplay(reused, false); got.Status != 400 || got.Code != "idempotency_key_reused" {
		t.Fatalf("reuse decision = %+v", got)
	}
	t.Logf("A10: first=NEW second=WAIT replay=200 stream=409 reused=400")
}

func TestA10ConcurrentSameKey(t *testing.T) {
	flushRedis(t)
	store := newStore(t, newClient(t), nil, newMemDB())

	start := make(chan struct{})
	results := make(chan Action, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := store.Begin(context.TODO(), beginInput(fmt.Sprintf("r%d", i), "h1", fmt.Sprintf("f%d", i)))
			if err != nil {
				errs <- err
				return
			}
			results <- result.Action
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	counts := map[Action]int{}
	for action := range results {
		counts[action]++
	}
	if counts[ActionNew] != 1 || counts[ActionWait] != 1 {
		t.Fatalf("actions = %v want exactly one NEW and one WAIT", counts)
	}
}

func TestRedisFailureOnlyDegrades(t *testing.T) {
	bad := redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		DB:             15,
		DialTimeout:    50 * time.Millisecond,
		CommandTimeout: 50 * time.Millisecond,
		LuaTimeout:     100 * time.Millisecond,
	})
	defer bad.Close()
	db := newMemDB()
	store := newStore(t, bad, nil, db)
	result, err := store.Begin(t.Context(), beginInput("r1", "h1", "f1"))
	if err != nil {
		t.Fatalf("Redis down must not fail Begin: %v", err)
	}
	if result.Action != ActionNew {
		t.Fatalf("action = %v want NEW", result.Action)
	}
	if err := store.Complete(t.Context(), *result.Record, StateDone, []byte("body")); err != nil {
		t.Fatalf("Redis down must not fail Complete: %v", err)
	}
	rec, err := db.Get(t.Context(), "idem", 7, "client-key")
	if err != nil || rec.State != StateDone {
		t.Fatalf("DB not updated: %+v err=%v", rec, err)
	}
}

func TestFenceRejectsLateWriter(t *testing.T) {
	flushRedis(t)
	clock, setNow := redisx.NewAdjustableClock(testNow)
	store := newStore(t, newClient(t), clock, newMemDB())

	first, err := store.Begin(t.Context(), beginInput("r1", "h1", "f1"))
	if err != nil || first.Action != ActionNew {
		t.Fatalf("first begin = %+v err=%v", first, err)
	}

	setNow(testNow.Add(20 * time.Second)) // DB lease TTL is 15s
	// Redis TTLs follow real time, not the fake clock. Drop K7 to represent
	// the same 15s lease expiring.
	lockKey := redisx.IdemLockKey(70, redisx.IdemHash("idem", 7, "client-key"))
	if err := redistest.Client().Del(t.Context(), lockKey).Err(); err != nil {
		t.Fatal(err)
	}
	taken, err := store.Begin(t.Context(), beginInput("r2", "h1", "f2"))
	if err != nil {
		t.Fatal(err)
	}
	if taken.Action != ActionStaleLock {
		t.Fatalf("takeover = %+v want STALE_LOCK", taken)
	}

	if err := store.Complete(t.Context(), *first.Record, StateDone, []byte("late")); !errors.Is(err, ErrFenceInvalid) {
		t.Fatalf("late writer error = %v want ErrFenceInvalid", err)
	}
	raw, err := redistest.Client().Get(t.Context(), "idem:70:"+redisx.IdemHash("idem", 7, "client-key")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "|r2|") {
		t.Fatalf("late writer changed the Redis owner: %q", raw)
	}
	if err := store.Complete(t.Context(), *taken.Record, StateDone, []byte("new")); err != nil {
		t.Fatalf("current owner could not complete: %v", err)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	flushRedis(t)
	clock, setNow := redisx.NewAdjustableClock(testNow)
	store := newStore(t, newClient(t), clock, newMemDB())
	first, err := store.Begin(t.Context(), beginInput("r1", "h1", "f1"))
	if err != nil {
		t.Fatal(err)
	}
	setNow(testNow.Add(6 * time.Second))
	if err := store.Renew(t.Context(), *first.Record); err != nil {
		t.Fatal(err)
	}
	key := redisx.IdemLockKey(70, redisx.IdemHash("idem", 7, "client-key"))
	ttl := redistest.Client().TTL(t.Context(), key).Val()
	if ttl < 13*time.Second {
		t.Fatalf("renewed lock TTL = %v want about 15s", ttl)
	}
	cacheKey := redisx.IdemKey(70, redisx.IdemHash("idem", 7, "client-key"))
	if cacheTTL := redistest.Client().TTL(t.Context(), cacheKey).Val(); cacheTTL < 23*time.Hour {
		t.Fatalf("renew shortened the idempotency cache to %v", cacheTTL)
	}
}

func TestRetryAfterFailureRotatesTheRow(t *testing.T) {
	flushRedis(t)
	store := newStore(t, newClient(t), nil, newMemDB())

	first, err := store.Begin(t.Context(), beginInput("r1", "h1", "f1"))
	if err != nil || first.Action != ActionNew {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	if err := store.Complete(t.Context(), *first.Record, StateFailed, nil); err != nil {
		t.Fatal(err)
	}
	retry, err := store.Begin(t.Context(), beginInput("r2", "h1", "f2"))
	if err != nil {
		t.Fatal(err)
	}
	if retry.Action != ActionRetryPrevFailed {
		t.Fatalf("retry = %+v want RETRY_PREV_FAILED", retry)
	}
	if got := DecideReplay(retry, false); got.Status != 0 {
		t.Fatalf("retry must continue the pipeline, got %+v", got)
	}
	second, err := store.Begin(t.Context(), beginInput("r3", "h1", "f3"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != ActionWait {
		t.Fatalf("third begin = %+v want WAIT (r2 is now in flight)", second)
	}
	if err := store.Complete(t.Context(), *retry.Record, StateDone, []byte("retried")); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Begin(t.Context(), beginInput("r4", "h1", "f4"))
	if err != nil || replay.Action != ActionReplay {
		t.Fatalf("replay after retry = %+v err=%v", replay, err)
	}
}
