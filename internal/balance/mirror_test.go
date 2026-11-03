package balance

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.Setup(m)) }

func flushRedis(t *testing.T) {
	t.Helper()
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func newClient(t *testing.T) *redisx.Client {
	t.Helper()
	client := redistest.NewClient(0)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type fakeSource struct {
	mu    sync.Mutex
	micro int64
	found bool
	err   error
	calls int
}

func (s *fakeSource) LoadBalance(_ context.Context, _ int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.micro, s.found, s.err
}

func (s *fakeSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestA9MissLoadsOnceAndRefills(t *testing.T) {
	flushRedis(t)
	source := &fakeSource{micro: 5000, found: true}
	mirror := New(newClient(t), source, 60*time.Second, nil)

	got, err := mirror.Get(t.Context(), 1)
	if err != nil || got != 5000 {
		t.Fatalf("first get = %d err=%v", got, err)
	}
	if source.callCount() != 1 {
		t.Fatalf("source calls = %d want 1", source.callCount())
	}
	if cached, err := redistest.Client().Get(t.Context(), redisx.BalanceKey(1)).Int64(); err != nil || cached != 5000 {
		t.Fatalf("mirror not refilled: value=%d err=%v", cached, err)
	}
	if ttl := redistest.Client().PTTL(t.Context(), redisx.BalanceKey(1)).Val(); ttl < 55*time.Second || ttl > 60*time.Second {
		t.Fatalf("mirror TTL = %v want ~60s", ttl)
	}

	got, err = mirror.Get(t.Context(), 1)
	if err != nil || got != 5000 {
		t.Fatalf("cached get = %d err=%v", got, err)
	}
	if source.callCount() != 1 {
		t.Fatalf("cache hit called the source again: %d", source.callCount())
	}
}

func TestA9MissingSourceIs503NotZero(t *testing.T) {
	flushRedis(t)
	source := &fakeSource{found: false}
	mirror := New(newClient(t), source, 60*time.Second, nil)
	got, err := mirror.Get(t.Context(), 2)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v want ErrUnavailable", err)
	}
	if got != 0 {
		t.Fatalf("value = %d; a missing account must not be reported as a balance", got)
	}
	if source.callCount() != 1 {
		t.Fatalf("source calls = %d want exactly one fallback", source.callCount())
	}
	if exists := redistest.Client().Exists(t.Context(), redisx.BalanceKey(2)).Val(); exists != 0 {
		t.Fatal("a miss was cached as a value")
	}
}

type recordingHook struct {
	mu    sync.Mutex
	names []string
}

func (h *recordingHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *recordingHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		h.record(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *recordingHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		for _, cmd := range cmds {
			h.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (h *recordingHook) record(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.names = append(h.names, name)
}

func (h *recordingHook) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.names...)
}

// dataCommands drops the connection handshake issued by go-redis on first
// use (HELLO / SELECT / CLIENT). Those are not K26 operations.
func dataCommands(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		switch name {
		case "hello", "select", "client", "auth", "ping", "info":
			continue
		default:
			out = append(out, name)
		}
	}
	return out
}

func TestA9InvalidateEmitsOnlyDEL(t *testing.T) {
	flushRedis(t)
	client := newClient(t)
	hook := &recordingHook{}
	client.Raw().AddHook(hook)
	mirror := New(client, &fakeSource{micro: 1, found: true}, 60*time.Second, nil)

	if err := mirror.Invalidate(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	commands := dataCommands(hook.snapshot())
	if len(commands) != 1 || commands[0] != "del" {
		t.Fatalf("invalidate commands = %v want [del]; K26 must never be SET", commands)
	}
	t.Logf("A9 invalidate commands: %v", commands)
}

func TestRedisDownFallsBackToSource(t *testing.T) {
	bad := redisx.New(redisx.Options{
		Addr:           "127.0.0.1:1",
		DB:             15,
		DialTimeout:    50 * time.Millisecond,
		CommandTimeout: 50 * time.Millisecond,
		LuaTimeout:     100 * time.Millisecond,
	})
	defer bad.Close()
	source := &fakeSource{micro: 7000, found: true}
	mirror := New(bad, source, 60*time.Second, nil)
	got, err := mirror.Get(t.Context(), 4)
	if err != nil || got != 7000 {
		t.Fatalf("get with Redis down = %d err=%v", got, err)
	}
}
