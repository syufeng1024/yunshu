package main

import (
	"yunshu.dev/core/internal/gw"
	"yunshu.dev/core/internal/settle"
)

// rejectedUsageRecord adapts a pre-terminal failure (G23) into the full P28 row
// shape. The money columns are kept at their zero values on purpose: a request
// that never reached the terminal has no prehold, no consumption and no refund,
// so the row must never show a non-zero amount.
func rejectedUsageRecord(f gw.FailureUsage) settle.UsageRecord {
	return settle.UsageRecord{
		RequestID:     f.RequestID,
		ConfigVersion: f.ConfigVersion,
		UserID:        f.UserID,
		APIKeyID:      f.APIKeyID,
		APIKeyName:    f.APIKeyName,
		ModelID:       f.ModelID,
		ModelName:     f.ModelName,
		RequestModel:  f.RequestModel,
		InboundProto:  f.InboundProto,
		PriceRuleID:   f.PriceRuleID,
		GroupCode:     f.GroupCode,
		PriceRatioBP:  f.PriceRatioBP,
		BillingTier:   "BASE",
		IsStream:      f.IsStream,
		HTTPStatus:    f.HTTPStatus,
		BizCode:       f.BizCode,
		Result:        "FAILED",
		FailStage:     f.FailStage,
		FailOwner:     f.FailOwner,
		UsageSource:   "NONE",

		// A failed request never owed money: keep every money column zero.
		PreholdMicro:          0,
		ConsumedMicro:         0,
		ConsumedRechargeMicro: 0,
		ConsumedBonusMicro:    0,
		RefundedMicro:         0,
		// No terminal means no tokens or upstream attempts either.
		TokensIn:         0,
		TokensOut:        0,
		TokensInEst:      0,
		TokensOutEst:     0,
		UpstreamAttempts: 0,

		TotalMS:   f.TotalMS,
		ClientIP:  f.ClientIP,
		UserAgent: f.UserAgent,
		CreatedAt: f.CreatedAt,
		RawBody:   f.RawBody,
	}
}
