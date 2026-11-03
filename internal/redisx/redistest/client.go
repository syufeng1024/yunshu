package redistest

import (
	"time"

	"yunshu.dev/core/internal/redisx"
)

// Fixture command budget.
//
// redisx.DefaultCommandTimeout is 50ms and it is a *production* latency policy:
// a request that overruns it must fail fast. It is the wrong budget for a test
// fixture, because `go test ./...` runs a dozen package binaries against one
// Redis server, and several of them flush a whole logical database per test. A
// momentary stall then turns into a wrong answer instead of a slow one:
//
//   - control/key  TestBodyErrorsStayInvalidRequestBody -> 503 auth_unavailable
//   - jobs         TestHeartbeatWritesK25WithTTL        -> context deadline exceeded
//   - lock         TestHeartbeatLiveness                -> context deadline exceeded
//   - control      TestSessionMutationRequiresCSRF      -> 503 / dropped revoke DEL
//   - acceptance   G-09 trial claim                     -> 503 instead of 409
//
// None of those assertions are about latency, so the fixture asks for 2s. The
// production constant is untouched: a test that *does* assert the 50ms budget
// (or a degraded path) must build its own client with the budget it means to
// test — see internal/balance, internal/idemstore, internal/ratelimit and
// internal/redisx, which keep a hand-built "bad client" next to the fixture one.
const (
	FixtureCommandTimeout = 2 * time.Second
	FixtureLuaTimeout     = 2 * time.Second
)

// NewClient returns a fixture client for the logical database Setup selected.
// Pool size follows redisx.New: a non-positive value means the default (20).
//
// Use this instead of redisx.New(redisx.Options{Addr: redistest.Addr(), DB:
// redistest.DB(), ...}) so that every fixture shares one budget definition.
func NewClient(poolSize int) *redisx.Client {
	return NewClientAt(Addr(), DB(), poolSize)
}

// NewClientAt is NewClient for an explicit address and database, for the
// packages that call Open instead of Setup and therefore hold an Env rather
// than the package-level current one.
func NewClientAt(addr string, db, poolSize int) *redisx.Client {
	return redisx.New(redisx.Options{
		Addr:           addr,
		DB:             db,
		PoolSize:       poolSize,
		CommandTimeout: FixtureCommandTimeout,
		LuaTimeout:     FixtureLuaTimeout,
	})
}
