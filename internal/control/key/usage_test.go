package key

import (
	"net/http"
	"strings"
	"testing"
)

type usagePoint struct {
	Hour          string `json:"hour"`
	Requests      int64  `json:"requests"`
	ConsumedMicro int64  `json:"consumed_micro"`
}

type usageBody struct {
	KeyID              int64        `json:"key_id"`
	Hours              int          `json:"hours"`
	Points             []usagePoint `json:"points"`
	TotalRequests      int64        `json:"total_requests"`
	TotalConsumedMicro int64        `json:"total_consumed_micro"`
}

// 图表需要连续的时间点：没有调用的小时必须补 0，否则曲线会把两个相隔很久的
// 小时连成一条直线，看起来像一直有流量。
func TestKeyUsageFillsEmptyHours(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "usage-key", []int64{modelID})

	rec := call(t, svc.Usage, http.MethodGet,
		"/api/console/keys/"+idStr(item.ID)+"/usage?hours=24", nil, uid, idStr(item.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body usageBody
	decodeBody(t, rec, &body)
	if body.Hours != 24 {
		t.Fatalf("hours=%d, want 24", body.Hours)
	}
	if len(body.Points) != 24 {
		t.Fatalf("points=%d, want one per hour (24)", len(body.Points))
	}
	if body.TotalRequests != 0 || body.TotalConsumedMicro != 0 {
		t.Fatalf("totals=%d/%d, want 0 with no usage rows", body.TotalRequests, body.TotalConsumedMicro)
	}
	for i, point := range body.Points {
		if point.Requests != 0 || point.ConsumedMicro != 0 {
			t.Fatalf("point %d not zeroed: %+v", i, point)
		}
		if i > 0 && point.Hour <= body.Points[i-1].Hour {
			t.Fatalf("hours not strictly increasing at %d: %q then %q", i, body.Points[i-1].Hour, point.Hour)
		}
	}
}

func TestKeyUsageRejectsForeignKey(t *testing.T) {
	svc, _ := newTestService(t)
	owner := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, owner, "owned-key", []int64{modelID})

	other := newTestUser(t, nil)
	rec := call(t, svc.Usage, http.MethodGet,
		"/api/console/keys/"+idStr(item.ID)+"/usage", nil, other, idStr(item.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for someone else's key", rec.Code, rec.Body.String())
	}
}

func TestKeyUsageValidatesHours(t *testing.T) {
	svc, _ := newTestService(t)
	uid := newTestUser(t, nil)
	modelID := newTestModel(t)
	item := mustCreate(t, svc, uid, "hours-key", []int64{modelID})

	for _, raw := range []string{"0", "169", "abc"} {
		rec := call(t, svc.Usage, http.MethodGet,
			"/api/console/keys/"+idStr(item.ID)+"/usage?hours="+raw, nil, uid, idStr(item.ID))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("hours=%s status=%d body=%s, want 400", raw, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "hours") {
			t.Fatalf("hours=%s body=%s, want the field named", raw, rec.Body.String())
		}
	}
}
