package gw

import (
	"context"
	"errors"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/billing"
	"yunshu.dev/core/internal/config"
	obslog "yunshu.dev/core/internal/obs/log"
)

// EstimateStage performs P09. It is read-only: the price comes from K19, the
// balance from K26, and nothing is written. The prehold remains in P10, after
// the rate-limit gate (TDS §3.1).
type EstimateStage struct{ d *Deps }

// NewEstimateStage builds P09.
func NewEstimateStage(d *Deps) *EstimateStage { return &EstimateStage{d: d} }

// Name implements Stage.
func (s *EstimateStage) Name() string { return StageP09 }

// Run implements Stage.
//
// G-02: admission compares raw_est (tokenizer input + requested max_tokens,
// no margin, no prehold ratio) with the available balance. est, which carries
// the 10% token safety margin and the 1.2 prehold ratio, is used only for the
// hold amount. The two numbers are never derived from one another.
func (s *EstimateStage) Run(ctx context.Context, c *Ctx) error {
	if c.Model == nil || c.Auth == nil || c.IR == nil {
		return StageErr(StageP09, apierr.New(apierr.CodeInternalError))
	}
	rawPrice, err := s.d.Prices.GetStandard(ctx, c.Model.ID)
	if err != nil {
		if errors.Is(err, config.ErrKeyNotFound) {
			return StageErr(StageP09, apierr.New(apierr.CodePriceNotConfigured))
		}
		return StageErr(StageP09, apierr.New(apierr.CodeConfigUnavailable).WithCause(err))
	}
	price, err := ParsePrice(rawPrice)
	if err != nil {
		if s.d.Logger != nil {
			s.d.Logger.Error(ctx, "price_rule_invalid: "+err.Error(), obslog.Fields{Stage: "P09"})
		}
		return StageErr(StageP09, apierr.New(apierr.CodePriceNotConfigured).WithCause(err))
	}
	if price.ModelType == "" {
		price.ModelType = c.Model.ModelType
	}
	if apiErr := checkEndpointModelType(c.Endpoint, price.ModelType); apiErr != nil {
		return StageErr(StageP09, apiErr)
	}

	safetyBP := s.intKey(config.KeyBillingPrestimateSafetyBP, 11000)
	preholdBP := s.decimalBP(config.KeyBillingPreholdRatio, 12000)
	guardBP := s.intKey(config.KeyBillingLongCtxGuardBP, 9000)

	est, apiErr := s.estimate(c, price, safetyBP, preholdBP, guardBP)
	if apiErr != nil {
		return StageErr(StageP09, apiErr)
	}

	available, err := s.d.Balance.Get(ctx, c.Auth.UserID)
	if err != nil {
		return StageErr(StageP09, apierr.New(apierr.CodeSettlementUnavailable).WithCause(err))
	}
	if est.RawMicro > available {
		overdraft := float64(s.intKey(config.KeyBillingOverdraftLimitDefault, 50))
		return StageErr(StageP09,
			apierr.NewInsufficientCredits(est.RawMicro, float64(available), overdraft))
	}

	c.Est = &est
	c.Price = price
	c.AvailableMicro = available
	c.RawEstMicro = est.RawMicro
	return nil
}

func (s *EstimateStage) estimate(c *Ctx, price *PriceRule, safetyBP, preholdBP, guardBP int64) (billing.Preestimate, *apierr.APIError) {
	ratio := c.Auth.GroupRatioBP
	if ratio < 0 || ratio > billing.BasisPoints {
		// The ratio comes from K13, not from the request; an out-of-range value
		// is a platform data fault, not a client error.
		return billing.Preestimate{}, apierr.New(apierr.CodeInternalError)
	}

	if price.ModelType == ModelTypeImage || c.IR.ImageParams != nil {
		n := int64(c.IR.N)
		if n <= 0 {
			n = 1
		}
		raw := billing.RoundUpDiv(price.PerCallPriceMicro*n*ratio, billing.BasisPoints)
		est := billing.RoundUpDiv(raw*preholdBP, billing.BasisPoints)
		return billing.Preestimate{
			RawMicro:          raw,
			EstMicro:          est,
			InPriceMicroPerK:  0,
			OutPriceMicroPerK: 0,
		}, nil
	}

	in, err := s.d.Tokens.CountInput(c.IR.InputText())
	if err != nil {
		return billing.Preestimate{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	out := s.outputTokens(c)
	if c.Model.ContextLength > 0 && in < c.Model.ContextLength {
		if room := c.Model.ContextLength - in; out > room {
			out = room
		}
	} else if c.Model.ContextLength > 0 && in >= c.Model.ContextLength {
		out = 0
	}

	tier := billing.SelectTierPrices(
		in,
		price.InPriceMicroPerK, price.OutPriceMicroPerK,
		price.LongCtxThresholdTokens,
		price.LongCtxInPriceMicroPerK, price.LongCtxOutPriceMicroPerK,
	)
	inP, err := billing.ApplyRatioBp(tier.InPriceMicroPerK, ratio)
	if err != nil {
		return billing.Preestimate{}, apierr.New(apierr.CodePriceNotConfigured)
	}
	outP, err := billing.ApplyRatioBp(tier.OutPriceMicroPerK, ratio)
	if err != nil {
		return billing.Preestimate{}, apierr.New(apierr.CodePriceNotConfigured)
	}

	raw := billing.RawEstimateMicro(in, out, inP, outP)

	estInP, estOutP := inP, outP
	if tier.Tier == billing.TierBase && guardBP > 0 && price.LongCtxThresholdTokens != nil &&
		in*billing.BasisPoints >= *price.LongCtxThresholdTokens*guardBP {
		// The estimate is already within 10% of the long-context threshold.
		// Prehold at the long-context price so a real usage that crosses the
		// threshold cannot create an unfunded negative balance (TDS §3.2 P09
		// step 2). Settlement refunds the difference.
		long := billing.SelectTierPrices(
			*price.LongCtxThresholdTokens+1,
			price.InPriceMicroPerK, price.OutPriceMicroPerK,
			price.LongCtxThresholdTokens,
			price.LongCtxInPriceMicroPerK, price.LongCtxOutPriceMicroPerK,
		)
		estInP, _ = billing.ApplyRatioBp(long.InPriceMicroPerK, ratio)
		estOutP, _ = billing.ApplyRatioBp(long.OutPriceMicroPerK, ratio)
	}

	est, err := billing.EstimateMicro(in, out, estInP, estOutP, safetyBP, preholdBP)
	if err != nil {
		return billing.Preestimate{}, apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	return billing.Preestimate{
		Tokens: billing.TokenEstimate{
			InputTokens:      in,
			OutputTokens:     out,
			SafeInputTokens:  billing.RoundUpDiv(in*safetyBP, billing.BasisPoints),
			SafeOutputTokens: billing.RoundUpDiv(out*safetyBP, billing.BasisPoints),
			SafetyBP:         safetyBP,
		},
		RawMicro:          raw,
		EstMicro:          est,
		InPriceMicroPerK:  inP,
		OutPriceMicroPerK: outP,
	}, nil
}

// checkEndpointModelType stops a mispriced request before any estimate. An
// IMAGE call priced from a TEXT rule uses a zero per-call price, which would
// make the request free; an EMBEDDING call against a TEXT rule would bill
// output tokens the model never produces. Both are configuration faults, not
// client errors.
func checkEndpointModelType(endpoint Endpoint, modelType string) *apierr.APIError {
	switch endpoint {
	case EndpointImages:
		if modelType != ModelTypeImage {
			return apierr.New(apierr.CodePriceNotConfigured)
		}
	case EndpointEmbeddings:
		if modelType != ModelTypeEmbedding {
			return apierr.New(apierr.CodePriceNotConfigured)
		}
	case EndpointChat, EndpointMessages:
		if modelType != ModelTypeText {
			return apierr.New(apierr.CodePriceNotConfigured)
		}
	}
	return nil
}

func (s *EstimateStage) outputTokens(c *Ctx) int64 {
	if c.IR.MaxTokens != nil && *c.IR.MaxTokens > 0 {
		return *c.IR.MaxTokens
	}
	if c.Model.MaxOutputTokens > 0 {
		return c.Model.MaxOutputTokens
	}
	if c.Model.ContextLength > 0 {
		// context_length is input + output, so using it as the output ceiling
		// reserves the whole context twice. Cap it at the platform output
		// ceiling; smaller contexts stay as they are (more conservative).
		if c.Model.ContextLength < PlatformMaxOutputTokens {
			return c.Model.ContextLength
		}
		return PlatformMaxOutputTokens
	}
	return PlatformMaxOutputTokens
}

func (s *EstimateStage) intKey(key config.Key, def int64) int64 {
	if s.d.Config == nil {
		return def
	}
	if v, ok := s.d.Config.Int(key); ok {
		return v
	}
	return def
}

func (s *EstimateStage) decimalBP(key config.Key, def int64) int64 {
	if s.d.Config == nil {
		return def
	}
	if v, ok := s.d.Config.Decimal(key); ok {
		return v.BasisPoints()
	}
	return def
}
