package acceptance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/jsonx"
)

// TestG11RoutingPublishUsesRealChain is T30/G11 (T6). The quota-apply unit test
// proved only that its own stub incremented cfg:ver; this case exercises the
// production chain boot.RouteSnapshot.Publish -> config.Publisher.PublishRaw
// against the real MySQL schema and the real Redis instance.
//
// The test itself never writes cfg:ver: it reads it, and the only INCR is the
// one inside writeSnapshotsAndVersion.
func TestG11RoutingPublishUsesRealChain(t *testing.T) {
	server(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm-up publish. nextVersion repairs a missing or stale cfg:ver with SET
	// before the INCR, so the very first publish after Redis was flushed is not
	// guaranteed to be N+1. Publishing once through the real path pins the
	// baseline; the measured publish below is still required to move it by one.
	if _, _, err := testRouteBuilder.Publish(ctx); err != nil {
		t.Fatalf("warm-up publish: %v", err)
	}
	beforeVersion := g11RedisInt(t, ctx, config.RedisVersionKey)
	beforeSnap := g11RedisValue(t, ctx, config.SnapshotRedisKey(config.NsRouting))

	// Subscribe after the warm-up so the only message observed is the one the
	// measured publish emits.
	cfgRedis := config.NewGoRedis(testRedis.Raw())
	sub, err := cfgRedis.Subscribe(ctx, config.RedisInvalidateChannel)
	if err != nil {
		t.Fatalf("subscribe %s: %v", config.RedisInvalidateChannel, err)
	}
	defer sub.Close()

	// Change the routing input: a fresh channel row whose name is unique per
	// run. It must show up in cfg:snap:ROUTING and must be removed on cleanup so
	// -count=2 and later package tests see the same preconditions.
	marker := fmt.Sprintf("g11-marker-%d-%d", time.Now().UnixNano(), nextInt())
	if strings.Contains(beforeSnap, marker) {
		t.Fatalf("marker %q already present before the change", marker)
	}
	res, err := testDB.ExecContext(ctx, `
		INSERT INTO t_channel
			(name, protocol_id, region, base_url, upstream_key_enc, upstream_key_mask,
			 created_by, created_at, updated_at)
		VALUES (?,?,'DOMESTIC','https://g11.invalid/v1',?,'sk-****g11',0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		marker, protocolID(t, "OPENAI_COMPAT"), []byte("v1:g11-placeholder"))
	if err != nil {
		t.Fatalf("insert marker channel: %v", err)
	}
	channelID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("marker channel id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.ExecContext(context.Background(),
			`DELETE FROM t_channel WHERE id = ?`, channelID)
	})

	// The measured publish. No stub is involved.
	_, result, err := testRouteBuilder.Publish(ctx)
	if err != nil {
		t.Fatalf("publish routing snapshot: %v", err)
	}

	// 1) cfg:ver moved from N to N+1, and it is the version PublishRaw reported.
	afterVersion := g11RedisInt(t, ctx, config.RedisVersionKey)
	if afterVersion != beforeVersion+1 {
		t.Fatalf("cfg:ver = %d after publish, want %d (before + 1)", afterVersion, beforeVersion+1)
	}
	if result.Version != afterVersion {
		t.Fatalf("PublishResult.Version = %d, cfg:ver = %d", result.Version, afterVersion)
	}

	// 2) The snapshot bytes changed and carry the new channel name, proving the
	// payload follows the database input rather than only the version field.
	afterSnap := g11RedisValue(t, ctx, config.SnapshotRedisKey(config.NsRouting))
	if afterSnap == beforeSnap {
		t.Fatalf("cfg:snap:ROUTING unchanged across a publish that changed t_channel")
	}
	if !strings.Contains(afterSnap, marker) {
		t.Fatalf("cfg:snap:ROUTING does not contain the new channel %q", marker)
	}

	// 3) cfg:invalidate carried one message for this publish/version.
	//
	// The channel is global and `go test -p 2` (scripts/coverage-gate.sh) runs
	// several packages against the same Redis, so another package's publish can
	// arrive first. Reading only the first message made this case fail with
	// "invalidation version = 13, want 94" while passing on its own; drain until
	// our version shows up, bounded by one deadline so a missing message still
	// fails instead of hanging.
	deadline := time.After(10 * time.Second)
	for {
		var msg string
		select {
		case msg = <-sub.Messages():
		case <-deadline:
			t.Fatalf("no message with version %d on %s within 10s after publish",
				afterVersion, config.RedisInvalidateChannel)
		}
		var invalidation struct {
			Version    int64    `json:"version"`
			Namespaces []string `json:"namespaces"`
		}
		if err := jsonx.Unmarshal([]byte(msg), &invalidation); err != nil {
			t.Fatalf("decode %s message %q: %v", config.RedisInvalidateChannel, msg, err)
		}
		if invalidation.Version != afterVersion {
			// Another package's publish; not ours to judge.
			continue
		}
		routing := false
		for _, ns := range invalidation.Namespaces {
			if ns == string(config.NsRouting) {
				routing = true
			}
		}
		if !routing {
			t.Fatalf("invalidation namespaces = %v, want %s", invalidation.Namespaces, config.NsRouting)
		}
		break
	}

	t.Logf("cfg:ver %d -> %d; cfg:snap:ROUTING %d -> %d bytes; new channel %q",
		beforeVersion, afterVersion, len(beforeSnap), len(afterSnap), marker)
}

func g11RedisValue(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	value, err := testRedis.Raw().Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("redis GET %s: %v", key, err)
	}
	return value
}

func g11RedisInt(t *testing.T, ctx context.Context, key string) int64 {
	t.Helper()
	raw := g11RedisValue(t, ctx, key)
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		t.Fatalf("redis GET %s = %q is not an integer: %v", key, raw, err)
	}
	return value
}
