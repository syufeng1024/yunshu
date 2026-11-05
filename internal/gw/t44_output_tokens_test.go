package gw

import (
	"testing"

	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/relaykit/ir"
)

func t44Context(contextLength, maxOutputTokens int64, maxTokens *int64) *Ctx {
	return &Ctx{
		Auth:  &AuthContext{UserID: 7, KeyID: 10, GroupRatioBP: 10000},
		Model: &ModelEntry{ID: 1, ModelType: ModelTypeText, ContextLength: contextLength, MaxOutputTokens: maxOutputTokens},
		IR:    &ir.Request{Model: "m", MaxTokens: maxTokens},
	}
}

func TestOutputTokensFallbackIsCappedByPlatformCeiling(t *testing.T) {
	// local-deepseek-chat: context 65536, max_output_tokens 0 (seed default),
	// no client max_tokens. Estimating 65536 output tokens exceeds the default
	// 60000 TPM budget, so every request was rejected with 429.
	c := t44Context(65536, 0, nil)
	got := NewEstimateStage(nil).outputTokens(c)
	if got > PlatformMaxOutputTokens {
		t.Fatalf("outputTokens() = %d, want <= %d (platform ceiling)", got, PlatformMaxOutputTokens)
	}
	t.Logf("context=65536 max_output_tokens=0 -> outputTokens=%d", got)
}

func TestOutputTokensFallbackKeepsSmallContext(t *testing.T) {
	// A context smaller than the platform ceiling must not be raised to 32768.
	c := t44Context(4096, 0, nil)
	got := NewEstimateStage(nil).outputTokens(c)
	if got != 4096 {
		t.Fatalf("outputTokens() = %d, want 4096", got)
	}
}

func TestOutputTokensExplicitMaxTokensWins(t *testing.T) {
	maxTokens := int64(2000)
	c := t44Context(65536, 0, &maxTokens)
	got := NewEstimateStage(nil).outputTokens(c)
	if got != 2000 {
		t.Fatalf("outputTokens() = %d, want 2000", got)
	}
}

func TestOutputTokensModelMaxOutputWinsOverFallback(t *testing.T) {
	c := t44Context(65536, 8192, nil)
	got := NewEstimateStage(nil).outputTokens(c)
	if got != 8192 {
		t.Fatalf("outputTokens() = %d, want 8192", got)
	}
}

// t44EstimateDeps prices input and output at 1 micro-credit per 1k tokens so
// the estimate is driven only by the token counts.
func t44EstimateDeps() *Deps {
	d := staticDeps(nil)
	d.Prices = fakePrices{raw: jsonx.RawMessage(
		`{"id":11,"model_type":"TEXT","in_price_micro_per_k":1000,"out_price_micro_per_k":1000}`)}
	d.Balance = fakeBalance{micro: 1_000_000}
	return d
}

func TestEstimateOutputTokensCappedAtPlatformCeiling(t *testing.T) {
	// End-to-end through estimate(): context 65536, max_output_tokens 0, no
	// max_tokens. OutputTokens feeds the TPM admission estimate, so it must not
	// come back as 65536.
	c := t44Context(65536, 0, nil)
	c.Endpoint = EndpointChat
	c.IR.Messages = []ir.Message{{Role: "user", Content: ir.Content{Text: "t44"}}}
	stage := NewEstimateStage(t44EstimateDeps())
	if err := stage.Run(t.Context(), c); err != nil {
		t.Fatalf("estimate failed: %v", err)
	}
	if c.Est.Tokens.OutputTokens > PlatformMaxOutputTokens {
		t.Fatalf("est.OutputTokens = %d, want <= %d", c.Est.Tokens.OutputTokens, PlatformMaxOutputTokens)
	}
	t.Logf("estimate context=65536 max_output_tokens=0 -> output_tokens=%d", c.Est.Tokens.OutputTokens)
}
