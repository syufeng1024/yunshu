package boot

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"yunshu.dev/core/internal/jsonx"
)

// splitModelSelectColumns turns the projection constant into the ordered column
// names it selects, so the assertion below compares against the SQL text itself
// rather than a hand-copied second list.
func splitModelSelectColumns(t *testing.T) []string {
	t.Helper()
	parts := strings.Split(modelEntryColumns, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		columns = append(columns, strings.TrimSpace(part))
	}
	return columns
}

// TestModelSelectColumnsMatchScanOrder proves that modelEntryColumns and the
// destination order inside scanModelEntry describe the same column sequence.
//
// Why this needs a test instead of a comment: context_length and
// max_output_tokens are both 64-bit integers and are adjacent in both the query
// and the struct, so a swapped scan does not make rows.Scan fail — it silently
// publishes the context length as the model's output ceiling. The gateway then
// estimates with a nonsense ceiling, and nothing in the pipeline complains.
func TestModelSelectColumnsMatchScanOrder(t *testing.T) {
	columns := splitModelSelectColumns(t)

	var probe modelEntry
	probeValue := reflect.ValueOf(&probe).Elem()
	// rows.Scan receives addresses of modelEntry fields; map each address back
	// to the column name carried by that field's JSON tag (the tags are the
	// snapshot contract and match t_model column names one for one).
	fieldByOffset := make(map[uintptr]string, probeValue.NumField())
	for i := 0; i < probeValue.NumField(); i++ {
		field := probeValue.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			name = field.Name
		}
		fieldByOffset[probeValue.Field(i).Addr().Pointer()] = name
	}

	scanned := make([]string, 0, len(columns))
	record := func(dest ...any) error {
		for _, d := range dest {
			value := reflect.ValueOf(d)
			if value.Kind() != reflect.Ptr || value.IsNil() {
				t.Fatalf("scan destination %T 不是非空指针", d)
			}
			name, ok := fieldByOffset[value.Pointer()]
			if !ok {
				// The only destination that is not a modelEntry field is the
				// nullable default_price_rule_id pointer.
				if _, isNullInt := d.(*sql.NullInt64); !isNullInt {
					t.Fatalf("scan destination 类型 %T 既不是 modelEntry 字段也不是 *sql.NullInt64", d)
				}
				name = "default_price_rule_id"
			}
			scanned = append(scanned, name)
		}
		return nil
	}

	if err := scanModelEntry(&probe, record); err != nil {
		t.Fatalf("scanModelEntry 在记录型 scanner 上失败: %v", err)
	}
	if len(scanned) != len(columns) {
		t.Fatalf("scan 目标数 = %d，SELECT 列数 = %d；两者必须一一对应\nscan=%v\nSELECT=%v",
			len(scanned), len(columns), scanned, columns)
	}
	for i := range columns {
		if scanned[i] != columns[i] {
			t.Fatalf("第 %d 列错位：SELECT 选 %q，scan 读 %q\nscan=%v\nSELECT=%v",
				i+1, columns[i], scanned[i], scanned, columns)
		}
	}

	// The two adjacent int64 columns are the reason this test exists; name them
	// explicitly so a future reordering fails with an obvious message.
	contextIdx, outputIdx := -1, -1
	for i, name := range columns {
		switch name {
		case "context_length":
			contextIdx = i
		case "max_output_tokens":
			outputIdx = i
		}
	}
	if contextIdx < 0 || outputIdx != contextIdx+1 {
		t.Fatalf("max_output_tokens 必须紧跟 context_length，实际下标 context=%d output=%d（列序 %v）",
			contextIdx, outputIdx, columns)
	}
}

// TestModelEntrySerialisesMaxOutputTokens pins the snapshot wire contract: the
// gateway decodes gw.ModelEntry from this JSON, so the key must be present with
// the value the database holds (0 means "unconfigured").
func TestModelEntrySerialisesMaxOutputTokens(t *testing.T) {
	raw, err := jsonx.Marshal(modelEntry{ID: 1, ContextLength: 65536, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"context_length":65536`, `"max_output_tokens":4096`} {
		if !strings.Contains(body, want) {
			t.Fatalf("快照缺少 %s: %s", want, body)
		}
	}
}
