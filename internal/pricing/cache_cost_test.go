package pricing

import (
	"io"
	"log/slog"
	"math"
	"testing"
)

func newCalc(t *testing.T, prices map[string]ModelPrice) *Calculator {
	t.Helper()
	c := NewCalculator(slog.New(slog.NewTextHandler(io.Discard, nil)), "", prices)
	return c
}

// Real Anthropic Sonnet tariffs: cache reads at 0.1x, cache writes at 1.25x.
var sonnet = ModelPrice{
	InputCostPerToken:           3e-06,
	OutputCostPerToken:          1.5e-05,
	CacheReadInputTokenCost:     3e-07,
	CacheCreationInputTokenCost: 3.75e-06,
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > want*1e-9+1e-15 {
		t.Fatalf("got %.12g, want %.12g", got, want)
	}
}

func TestCostUsage_CacheReadIsBilledAtItsOwnTariff(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{"claude-sonnet": sonnet})

	// 100k prompt tokens of which 90k came from cache, 10k fresh.
	got := c.CostUsage("claude-sonnet", UsageCost{
		PromptTokens:       100000,
		CachedPromptTokens: 90000,
		CompletionTokens:   1000,
	})
	want := 10000*3e-06 + 90000*3e-07 + 1000*1.5e-05
	approx(t, got, want)

	// Billing everything at full input price is what the gateway did before;
	// it must now be strictly more expensive than the cache-aware figure.
	naive := c.Cost("claude-sonnet", 100000, 1000)
	if got >= naive {
		t.Fatalf("cache-aware cost %.10f should be below naive %.10f", got, naive)
	}
}

func TestCostUsage_CacheCreationIsBilledAtAPremium(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{"claude-sonnet": sonnet})

	got := c.CostUsage("claude-sonnet", UsageCost{
		PromptTokens:        100000,
		CacheCreationTokens: 100000,
		CompletionTokens:    0,
	})
	want := 100000 * 3.75e-06
	approx(t, got, want)

	// Writing the cache costs more than a plain uncached request.
	if plain := c.Cost("claude-sonnet", 100000, 0); got <= plain {
		t.Fatalf("cache creation %.10f should exceed plain input %.10f", got, plain)
	}
}

// The categories partition PromptTokens, so no token may be charged twice.
func TestCostUsage_CategoriesDoNotDoubleCount(t *testing.T) {
	flat := ModelPrice{InputCostPerToken: 1e-06, OutputCostPerToken: 1e-06,
		CacheReadInputTokenCost: 1e-06, CacheCreationInputTokenCost: 1e-06}
	c := newCalc(t, map[string]ModelPrice{"flat": flat})

	// With every tariff equal, any split must price identically to the total.
	total := c.CostUsage("flat", UsageCost{PromptTokens: 1000, CompletionTokens: 0})
	split := c.CostUsage("flat", UsageCost{
		PromptTokens: 1000, CachedPromptTokens: 600, CacheCreationTokens: 300,
	})
	approx(t, split, total)
}

// A provider that reports more cache tokens than prompt tokens must not
// produce a negative uncached charge.
func TestCostUsage_OverReportedCacheNeverGoesNegative(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{"claude-sonnet": sonnet})
	got := c.CostUsage("claude-sonnet", UsageCost{
		PromptTokens:       1000,
		CachedPromptTokens: 5000,
	})
	if got < 0 {
		t.Fatalf("cost must never be negative, got %.10f", got)
	}
	approx(t, got, 5000*3e-07)
}

// A catalog entry without cache tariffs must fall back to the input price,
// never to zero — a missing tariff cannot make tokens free.
func TestCostUsage_MissingTariffsFallBackToInputPrice(t *testing.T) {
	bare := ModelPrice{InputCostPerToken: 2e-06, OutputCostPerToken: 8e-06}
	c := newCalc(t, map[string]ModelPrice{"bare": bare})

	got := c.CostUsage("bare", UsageCost{
		PromptTokens:        1000,
		CachedPromptTokens:  400,
		CacheCreationTokens: 200,
	})
	approx(t, got, 1000*2e-06)
}

func TestCostUsage_UnknownModelCostsZero(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{})
	if got := c.CostUsage("nope", UsageCost{PromptTokens: 1000}); got != 0 {
		t.Fatalf("unknown model must cost 0, got %v", got)
	}
}

// Cost() is the legacy entry point and must keep charging every prompt token
// at full input price.
func TestCost_LegacyPathUnchanged(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{"claude-sonnet": sonnet})
	approx(t, c.Cost("claude-sonnet", 1000, 500), 1000*3e-06+500*1.5e-05)
}

// Config overrides must be able to set every tariff, including the cache ones.
func TestCostUsage_ConfigOverridesEveryTariff(t *testing.T) {
	c := newCalc(t, map[string]ModelPrice{
		"custom": {InputCostPerToken: 1e-05, OutputCostPerToken: 2e-05,
			CacheReadInputTokenCost: 1e-07, CacheCreationInputTokenCost: 5e-05},
	})
	got := c.CostUsage("custom", UsageCost{
		PromptTokens: 300, CachedPromptTokens: 100, CacheCreationTokens: 100, CompletionTokens: 10,
	})
	approx(t, got, 100*1e-05+100*1e-07+100*5e-05+10*2e-05)
}

// The shipped catalog must actually carry the cache tariffs — the generator
// used to strip them, which is what made cached reads unbillable.
func TestEmbeddedCatalogCarriesCacheTariffs(t *testing.T) {
	p, ok := LookupEmbedded("claude-sonnet-4-5")
	if !ok {
		t.Skip("claude-sonnet-4-5 not in catalog")
	}
	if p.CacheReadInputTokenCost <= 0 {
		t.Fatal("embedded catalog is missing cache_read_input_token_cost")
	}
	if p.CacheCreationInputTokenCost <= 0 {
		t.Fatal("embedded catalog is missing cache_creation_input_token_cost")
	}
	if p.CacheReadInputTokenCost >= p.InputCostPerToken {
		t.Fatal("cache reads should be cheaper than fresh input")
	}
	if p.CacheCreationInputTokenCost <= p.InputCostPerToken {
		t.Fatal("cache writes should cost more than fresh input")
	}
}
