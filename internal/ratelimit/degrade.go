package ratelimit

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yunshu.dev/core/internal/redisx"
)

// Mode is the value of ratelimit.degrade.mode. V1.0 has exactly two states;
// there is no mode that relaxes RPM/TPM/daily (T06 hard constraint 7).
type Mode string

const (
	ModeOff             Mode = "off"
	ModeConcurrencyOnly Mode = "concurrency_only"
)

// EnvDegradeMode selects the mode at startup. The name follows the GW_ prefix
// and the GW_POLICY_* convention: the value is read once, at boot, and a value
// that is not understood is a startup error rather than a silent fallback — a
// typo must not quietly turn concurrency counting back into the ZSET path.
const EnvDegradeMode = "GW_RATELIMIT_DEGRADE_MODE"

// DefaultMode is what an unset EnvDegradeMode means: off.
//
// G31 made the concurrency fallback reachable: Replicas() is now a cached
// last-known value refreshed in the background, so concurrency_only no longer
// pays two extra Redis reads per request or returns 503 just because the
// divisor cannot be read. The default still stays off for two reasons:
//
//  1. concurrency_only relaxes only the concurrency dimension; RPM/TPM/daily
//     still fail closed, so the availability gain is partial by design.
//  2. Turning it on rewrites the meaning of gw_ratelimit_degraded_total{dim=
//     "concurrency"}, which would then count admitted requests instead of
//     outages, blurring the outage signal.
//
// The switch is read from EnvDegradeMode; an unknown value stops the process
// rather than being ignored. TDS :3073 lists concurrency_only as V1's only
// value; that statement assumes the path works. Until an operator explicitly
// opts in, this default keeps the deviation explicit (see errata E-38).
const DefaultMode = ModeOff

// ModeFromEnv reads EnvDegradeMode. Empty means DefaultMode; anything else must
// be one of the two known values.
func ModeFromEnv() (Mode, error) {
	raw := strings.TrimSpace(os.Getenv(EnvDegradeMode))
	if raw == "" {
		return DefaultMode, nil
	}
	switch Mode(raw) {
	case ModeOff, ModeConcurrencyOnly:
		return Mode(raw), nil
	default:
		return "", fmt.Errorf("ratelimit: %s must be %q or %q, got %q",
			EnvDegradeMode, ModeOff, ModeConcurrencyOnly, raw)
	}
}

// ConcurrencyMode describes which counter admitted the request.
type ConcurrencyMode string

const (
	// ConcurrencyRedis means K3 answered; the lease must be released by ZREM.
	ConcurrencyRedis ConcurrencyMode = "redis"
	// ConcurrencyLocal means the node-local counter answered; the release
	// must decrement it instead of touching K3.
	ConcurrencyLocal ConcurrencyMode = "local"
)

// nodeStaleMS is the heartbeat horizon for K18'. A node that has not written
// a heartbeat for 60s is removed before ZCARD, so a crashed replica stops
// inflating the divisor.
const nodeStaleMS = int64(60_000)

// NodeHeartbeatInterval is the K18' refresh cadence (TDS §2.1).
const NodeHeartbeatInterval = 20 * time.Second

// PerKeyLocalLimit is ceil(quota / replicas) (AD-09). Ceiling matters: floor
// would make a quota of 5 across 3 replicas admit only 3 in total, silently
// shrinking every plan at the first scale-out.
func PerKeyLocalLimit(quota, replicas int64) int64 {
	if quota <= 0 {
		return 0
	}
	if replicas <= 1 {
		return quota
	}
	return (quota + replicas - 1) / replicas
}

// Degrader owns the per-node concurrency counter and the K18' membership
// used to derive the divisor. It only ever affects the concurrency dimension.
type Degrader struct {
	client *redisx.Client
	clock  *redisx.Clock
	nodeID string
	mode   Mode
	local  *localTable
	seen   *seenSet
	// replicas caches the last known K18' member count. It is read on the hot
	// path (no Redis) and refreshed by RefreshReplicas in the heartbeat loop,
	// so a Redis outage keeps the last divisor instead of blocking admission.
	replicas atomic.Int64
}

// NewDegrader builds a degrader. nodeID must be stable for the process
// lifetime (the container/pod name); a new value on every restart would keep
// stale members in K18' until their heartbeat expires.
func NewDegrader(client *redisx.Client, clock *redisx.Clock, nodeID string, mode Mode, maxLocalKeys int) *Degrader {
	if maxLocalKeys <= 0 {
		maxLocalKeys = 200_000
	}
	d := &Degrader{
		client: client,
		clock:  clock,
		nodeID: nodeID,
		mode:   mode,
		local:  newLocalTable(maxLocalKeys),
		seen:   newSeenSet(maxLocalKeys),
	}
	// Floor 1: before the first refresh the divisor assumes a single replica,
	// which errs toward admitting more per node (the safe direction for the
	// concurrency dimension) rather than rejecting everything.
	d.replicas.Store(1)
	return d
}

// Enabled reports whether concurrency is locally counted.
func (d *Degrader) Enabled() bool { return d != nil && d.mode == ModeConcurrencyOnly }

// Replicas returns the cached replica count, never below one. It never touches
// Redis, so the degraded concurrency path stays reachable during an outage:
// RPM/TPM/daily still fail-close in callAcquire, but the local divisor no
// longer depends on K18' being up.
func (d *Degrader) Replicas() int64 {
	n := d.replicas.Load()
	if n < 1 {
		return 1
	}
	return n
}

// RefreshReplicas cleans stale members from K18' and stores the live replica
// count. On success the cache is updated; on failure the previous value is kept
// and the error is returned for the caller to log (the request path does not
// call this directly).
func (d *Degrader) RefreshReplicas(ctx context.Context) error {
	cctx, cancel := d.client.CommandContext(ctx)
	defer cancel()
	now := d.clock.NowMS()
	if err := d.client.Raw().ZRemRangeByScore(cctx, redisx.NodeSetKey(),
		"-inf", strconv.FormatInt(now-nodeStaleMS, 10)).Err(); err != nil {
		return err
	}
	n, err := d.client.Raw().ZCard(cctx, redisx.NodeSetKey()).Result()
	if err != nil {
		return err
	}
	if n < 1 {
		n = 1
	}
	d.replicas.Store(n)
	return nil
}

// Heartbeat writes this node's K18' member with a millisecond score.
func (d *Degrader) Heartbeat(ctx context.Context) error {
	cctx, cancel := d.client.CommandContext(ctx)
	defer cancel()
	return d.client.Raw().Do(cctx, "ZADD", redisx.NodeSetKey(),
		d.clock.NowMS(), d.nodeID).Err()
}

// StartHeartbeat registers this node and refreshes the replica divisor now,
// then repeats both every 20s until the returned stop function is called.
func (d *Degrader) StartHeartbeat(ctx context.Context) func() {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Heartbeat(runCtx)
		_ = d.RefreshReplicas(runCtx)
		ticker := time.NewTicker(NodeHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = d.Heartbeat(runCtx)
				_ = d.RefreshReplicas(runCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// TryAcquire increments the per-key local counter when below limit.
func (d *Degrader) TryAcquire(keyID, limit int64) bool {
	if limit <= 0 {
		return false
	}
	entry := d.local.get(keyID)
	for {
		current := entry.count.Load()
		if current >= limit {
			return false
		}
		if entry.count.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// ReleaseLocal decrements the local counter once per request_id. A second
// defer for the same request is ignored, mirroring L2's ZREM idempotency;
// otherwise a duplicated release would mint one extra concurrency slot.
func (d *Degrader) ReleaseLocal(keyID int64, requestID string) {
	if !d.seen.mark(requestID) {
		return
	}
	entry := d.local.get(keyID)
	for {
		current := entry.count.Load()
		if current <= 0 {
			return
		}
		if entry.count.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// LocalCount exposes the current in-flight count for tests and metrics.
func (d *Degrader) LocalCount(keyID int64) int64 { return d.local.get(keyID).count.Load() }

// ResetLocal clears the local counters. It is called on recovery after the
// drain window: keeping stale counts while the ZSET resumes would double
// count the same requests.
func (d *Degrader) ResetLocal() { d.local.reset() }

type localEntry struct {
	key   int64
	count atomic.Int64
	elem  *list.Element
}

type localTable struct {
	mu  sync.Mutex
	m   map[int64]*localEntry
	lru *list.List
	max int
}

func newLocalTable(max int) *localTable {
	return &localTable{m: make(map[int64]*localEntry), lru: list.New(), max: max}
}

func (t *localTable) get(key int64) *localEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.m[key]
	if entry != nil {
		t.lru.MoveToFront(entry.elem)
		return entry
	}
	if len(t.m) >= t.max {
		t.evictLocked()
	}
	entry = &localEntry{key: key}
	entry.elem = t.lru.PushFront(entry)
	t.m[key] = entry
	return entry
}

func (t *localTable) evictLocked() {
	for elem := t.lru.Back(); elem != nil; elem = elem.Prev() {
		entry := elem.Value.(*localEntry)
		if entry.count.Load() == 0 {
			t.lru.Remove(elem)
			delete(t.m, entry.key)
			return
		}
	}
	// Every tracked key has work in flight. Drop the least recently used one;
	// in degraded mode this loosens one key's local ceiling until release,
	// which is the documented best-effort property of AD-09.
	if elem := t.lru.Back(); elem != nil {
		entry := elem.Value.(*localEntry)
		t.lru.Remove(elem)
		delete(t.m, entry.key)
	}
}

func (t *localTable) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = make(map[int64]*localEntry)
	t.lru = list.New()
}

// seenSet is a bounded FIFO of request ids whose local lease was released.
// 200k entries at ~32 bytes each is the same order as the counter table and
// only exists while concurrency is degraded.
type seenSet struct {
	mu   sync.Mutex
	m    map[string]*list.Element
	fifo *list.List
	max  int
}

func newSeenSet(max int) *seenSet {
	if max <= 0 {
		max = 200_000
	}
	return &seenSet{m: make(map[string]*list.Element), fifo: list.New(), max: max}
}

// mark returns true the first time id is seen.
func (s *seenSet) mark(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[id]; exists {
		return false
	}
	elem := s.fifo.PushBack(id)
	s.m[id] = elem
	if len(s.m) > s.max {
		oldest := s.fifo.Front()
		if oldest != nil {
			s.fifo.Remove(oldest)
			delete(s.m, oldest.Value.(string))
		}
	}
	return true
}
