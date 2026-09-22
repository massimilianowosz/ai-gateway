package hivestate

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
)

func testCalculator() *pricing.Calculator {
	return pricing.NewCalculator(slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil)
}

// The per-API guess was wrong by a factor of five: 0.5 is the gpt-4o tariff,
// while gpt-5.6-sol reads cache at 0.1 and charges 1.25x to write it. Those two
// numbers decide every rewrite, so they must come from the model's real price.
func TestCacheRatiosFor_PrefersTheCataloguePrice(t *testing.T) {
	pc := testCalculator()
	pc.SetPrice("codex-gpt-5.6-sol", pricing.ModelPrice{
		InputCostPerToken:           4e-06,
		OutputCostPerToken:          2e-05,
		CacheReadInputTokenCost:     4e-07,
		CacheCreationInputTokenCost: 5e-06,
	})

	got := cacheRatiosFor(pc, nil, "codex-gpt-5.6-sol", APIResponses, 0, 0)
	if !got.FromCatalog {
		t.Fatal("the catalogue price was ignored")
	}
	if got.Read < 0.09 || got.Read > 0.11 {
		t.Errorf("read ratio = %.3f, want ~0.10 from $0.40/$4.00", got.Read)
	}
	if got.Write < 1.24 || got.Write > 1.26 {
		t.Errorf("write ratio = %.3f, want ~1.25 from $5.00/$4.00", got.Write)
	}
}

// An unpriced model still has to be judged, and the assumption must be the
// cautious one: overstating the discount makes passthrough look dearer than it
// is and waves through rewrites that do not pay.
func TestCacheRatiosFor_FallsBackWithoutACataloguePrice(t *testing.T) {
	got := cacheRatiosFor(nil, nil, "unknown-model", APIResponses, 0, 0)
	if got.FromCatalog {
		t.Fatal("reported a catalogue price that does not exist")
	}
	if got.Read != DefaultOpenAICacheReadRatio {
		t.Errorf("read ratio = %v, want the default %v", got.Read, DefaultOpenAICacheReadRatio)
	}
	if got.Write < 1.0 {
		t.Errorf("write ratio = %v, must never be below full price", got.Write)
	}
}

// A model priced with no cache tariffs gets no discount, not a free one.
func TestCacheRatiosFor_NoCacheTariffMeansNoDiscount(t *testing.T) {
	pc := testCalculator()
	pc.SetPrice("plain", pricing.ModelPrice{InputCostPerToken: 1e-06, OutputCostPerToken: 2e-06})

	got := cacheRatiosFor(pc, nil, "plain", APIOpenAI, 0, 0)
	if got.Read != 1.0 {
		t.Errorf("read ratio = %v, want 1.0 when the model has no cache tariff", got.Read)
	}
}
