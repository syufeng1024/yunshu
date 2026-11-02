package main

import (
	"testing"

	"yunshu.dev/core/internal/gw"
)

// TestRejectedUsageRecordPinsZeroMoney pins the structural guarantee behind
// G23: a failure row can carry metadata and status, but every money column must
// stay zero because the request never reached a prehold or settlement.
func TestRejectedUsageRecordPinsZeroMoney(t *testing.T) {
	rec := rejectedUsageRecord(gw.FailureUsage{
		RequestID:  "req-1",
		HTTPStatus: 429,
		BizCode:    "rpm_limit_exceeded",
		FailStage:  "RATE_LIMIT",
		FailOwner:  "CLIENT",
	})

	money := []struct {
		name string
		v    int64
	}{
		{"PreholdMicro", rec.PreholdMicro},
		{"ConsumedMicro", rec.ConsumedMicro},
		{"RefundedMicro", rec.RefundedMicro},
		{"ConsumedRechargeMicro", rec.ConsumedRechargeMicro},
		{"ConsumedBonusMicro", rec.ConsumedBonusMicro},
	}
	for _, m := range money {
		if m.v != 0 {
			t.Errorf("%s = %d, want 0", m.name, m.v)
		}
	}

	if rec.Result != "FAILED" {
		t.Errorf("Result = %q, want FAILED", rec.Result)
	}
	if rec.UsageSource != "NONE" {
		t.Errorf("UsageSource = %q, want NONE", rec.UsageSource)
	}
	if rec.BillingTier != "BASE" {
		t.Errorf("BillingTier = %q, want BASE", rec.BillingTier)
	}
}
