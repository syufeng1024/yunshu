package bootstrap

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"yunshu.dev/core/internal/control/ctltest"
)

// testDB is the shared yunshu_test pool. ctltest.SetupDB migrates it to the
// head of the embedded set, so migration 0027 (t_user.username) is present
// before any test in this package runs — which is exactly what the bootstrap
// needs and what a schema-not-migrated deployment does not have.
//
// Redis is deliberately not set up: EnsureSuperAdmin touches only MySQL and the
// audit writer. Adding a Redis fixture would make this package fail for an
// unrelated dependency.
var testDB *sql.DB

func TestMain(m *testing.M) {
	db, err := ctltest.SetupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}
