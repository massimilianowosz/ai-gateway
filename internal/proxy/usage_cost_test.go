package proxy

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func priceCalculator(t *testing.T, prices map[string]pricing.ModelPrice) *pricing.Calculator {
	t.Helper()
	c := pricing.NewCalculator(slog.New(slog.NewTextHandler(io.Discard, nil)), "", prices)
	return c
}

// The upstream id is the more precise name and stays first: a model exposed as
// "gpt-4o" is served by a dated deployment the catalog prices separately.
func TestComputeCostUsage_PrefersTheUpstreamModelId(t *testing.T) {
	pc := priceCalculator(t, map[string]pricing.ModelPrice{
		"gpt-4o":            {InputCostPerToken: 1, OutputCostPerToken: 1},
		"gpt-4o-2024-11-20": {InputCostPerToken: 2, OutputCostPerToken: 2},
	})
	dep := &provider.Deployment{ModelName: "gpt-4o", ProviderModel: "gpt-4o-2024-11-20"}

	cost := computeCostUsage(dep, pc, &provider.Usage{PromptTokens: 10, CompletionTokens: 0})
	assert.Equal(t, 20.0, cost, "the dated deployment's own price was ignored")
}

// Several config entries may share one upstream id. When their declared prices
// disagree, none is registered under it — and the model's own price, which is
// not in doubt, must still be what it bills at.
func TestComputeCostUsage_FallsBackToTheModelsOwnPrice(t *testing.T) {
	// "llama3.2:1b" is deliberately absent: that is the state a conflict leaves.
	pc := priceCalculator(t, map[string]pricing.ModelPrice{
		"small": {InputCostPerToken: 0.5, OutputCostPerToken: 0.5},
	})
	dep := &provider.Deployment{ModelName: "small", ProviderModel: "llama3.2:1b"}

	cost := computeCostUsage(dep, pc, &provider.Usage{PromptTokens: 10, CompletionTokens: 10})
	assert.Equal(t, 10.0, cost,
		"a priced model billed nothing, so its budget can never run out")
}

// Flat deployments bill nothing whatever the catalog says.
func TestComputeCostUsage_FlatBillsNothing(t *testing.T) {
	pc := priceCalculator(t, map[string]pricing.ModelPrice{
		"small": {InputCostPerToken: 0.5, OutputCostPerToken: 0.5},
	})
	dep := &provider.Deployment{
		ModelName: "small", ProviderModel: "llama3.2:1b",
		BillingMode: config.BillingModeFlat,
	}
	assert.Zero(t, computeCostUsage(dep, pc, &provider.Usage{PromptTokens: 10}))
}

// An unpriced model is still free, under either name.
func TestComputeCostUsage_UnknownModelIsFree(t *testing.T) {
	pc := priceCalculator(t, nil)
	dep := &provider.Deployment{ModelName: "mystery", ProviderModel: "mystery-v1"}
	assert.Zero(t, computeCostUsage(dep, pc, &provider.Usage{PromptTokens: 10}))
}
