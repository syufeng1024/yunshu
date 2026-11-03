package lock

import (
	"os"
	"testing"
	"time"

	"yunshu.dev/core/internal/redisx"
	"yunshu.dev/core/internal/redisx/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.Setup(m)) }

func newLocker(t *testing.T) *Locker {
	t.Helper()
	client := redistest.NewClient(0)
	t.Cleanup(func() { _ = client.Close() })
	return New(client, nil)
}

func TestReconLockIsExclusive(t *testing.T) {
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	locker := newLocker(t)
	first, err := locker.AcquireRecon(t.Context(), "20261026", "jobsd-a")
	if err != nil || !first {
		t.Fatalf("first acquire = %v err=%v", first, err)
	}
	second, err := locker.AcquireRecon(t.Context(), "20261026", "jobsd-b")
	if err != nil || second {
		t.Fatalf("second acquire = %v err=%v; the daily job must run once", second, err)
	}
	otherDay, err := locker.AcquireRecon(t.Context(), "20261027", "jobsd-b")
	if err != nil || !otherDay {
		t.Fatalf("next-day acquire = %v err=%v", otherDay, err)
	}
}

func TestExpireBatchRenewRequiresOwner(t *testing.T) {
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	locker := newLocker(t)
	acquired, err := locker.AcquireExpireBatch(t.Context(), "owner-a")
	if err != nil || !acquired {
		t.Fatalf("acquire = %v err=%v", acquired, err)
	}
	renewed, err := locker.RenewExpireBatch(t.Context(), "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	if renewed {
		t.Fatal("a different owner renewed K22")
	}
	renewed, err = locker.RenewExpireBatch(t.Context(), "owner-a")
	if err != nil || !renewed {
		t.Fatalf("owner renew = %v err=%v", renewed, err)
	}
	ttl := redistest.Client().TTL(t.Context(), redisx.ExpireBatchLockKey()).Val()
	if ttl < ExpireBatchLockTTL-time.Second || ttl > ExpireBatchLockTTL {
		t.Fatalf("TTL = %v want ~%v", ttl, ExpireBatchLockTTL)
	}
}

func TestHeartbeatLiveness(t *testing.T) {
	if err := redistest.Client().FlushDB(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	locker := newLocker(t)
	alive, err := locker.Alive(t.Context(), "jobsd-1")
	if err != nil || alive {
		t.Fatalf("alive before heartbeat = %v err=%v", alive, err)
	}
	if err := locker.Heartbeat(t.Context(), "jobsd-1"); err != nil {
		t.Fatal(err)
	}
	alive, err = locker.Alive(t.Context(), "jobsd-1")
	if err != nil || !alive {
		t.Fatalf("alive after heartbeat = %v err=%v", alive, err)
	}
	ttl := redistest.Client().TTL(t.Context(), redisx.JobHeartbeatKey("jobsd-1")).Val()
	if ttl <= 0 || ttl > JobHeartbeatTTL {
		t.Fatalf("heartbeat TTL = %v", ttl)
	}
}
