package migrate

import "testing"

// AUTO_MIGRATE 的默认值决定了"谁来跑 DDL"，比它看起来危险得多：
//   - 本地默认开，是为了把 go run ./cmd/controld 变成一条命令；
//   - 生产默认关，是因为那里有独立的 migrate Job，且应用账号不该有 DDL 权限；
//   - 显式值永远覆盖默认；写错的布尔必须报错，而不是猜一个。
func TestAutoMigrateEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  string
		raw  string
		want bool
	}{
		{"dev default on", "dev", "", true},
		{"local default on", "local", "", true},
		{"dev default on (case)", "DEV", "", true},
		{"production default off", "prod", "", false},
		{"staging default off", "staging", "", false},
		{"explicit true in prod", "prod", "true", true},
		{"explicit 1 in prod", "prod", "1", true},
		{"explicit YES in prod", "prod", " YES ", true},
		{"explicit false in dev", "dev", "false", false},
		{"explicit 0 in local", "local", "0", false},
		{"explicit off in prod", "prod", "off", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AutoMigrateEnabled(tc.env, tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("AutoMigrateEnabled(%q,%q)=%v, want %v", tc.env, tc.raw, got, tc.want)
			}
		})
	}

	if _, err := AutoMigrateEnabled("prod", "ture"); err == nil {
		t.Fatalf("a misspelled boolean must be an error, not a silent default")
	}
}

func TestAppliedCountIgnoresSkips(t *testing.T) {
	results := []Result{
		{Version: "0019", Direction: "skip"},
		{Version: "0027", Direction: "up"},
		{Version: "0028", Direction: "up"},
	}
	if n := AppliedCount(results); n != 2 {
		t.Fatalf("AppliedCount=%d, want 2 (skips are not applications)", n)
	}
	if n := AppliedCount(nil); n != 0 {
		t.Fatalf("AppliedCount(nil)=%d, want 0", n)
	}
}
