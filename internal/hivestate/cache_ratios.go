package hivestate

import (
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// CacheRatios are the cache tariffs the guard weighs a rewrite with, expressed
// as multiples of a full-price input token.
type CacheRatios struct {
	Read  float64
	Write float64
	// FromCatalog is false when the ratios are assumptions rather than the
	// model's real prices.
	FromCatalog bool
}

// cacheRatiosFor prefers the model's catalogue prices over a per-API guess.
//
// The guess was one ratio per API surface, and it was wrong by a factor of
// five: OpenAI's 0.5 is the gpt-4o tariff, while the gpt-5 family reads cache
// at 0.1 — the same discount as Anthropic — and also charges 1.25x to write it,
// which the per-API guess assumed only Anthropic did. Those two numbers decide
// every rewrite, so they should come from the price the request will actually
// be billed at rather than from a constant that ages badly.
//
// The catalogue is keyed by the upstream id, not the name the gateway exposes:
// "codex-gpt-5.6-sol" is served by "gpt-5.6-sol", and only the latter is
// priced. Resolving through the registry is what billing already does.
func cacheRatiosFor(pc *pricing.Calculator, registry *provider.Registry, model string, api APIFlavor, anthropicRatio, openaiRatio float64) CacheRatios {
	if pc != nil {
		for _, name := range pricingNames(registry, model) {
			p, ok := pc.GetPrice(name)
			if !ok || p.InputCostPerToken <= 0 {
				continue
			}
			r := CacheRatios{Read: 1.0, Write: 1.0, FromCatalog: true}
			if p.CacheReadInputTokenCost > 0 {
				r.Read = p.CacheReadInputTokenCost / p.InputCostPerToken
			}
			if p.CacheCreationInputTokenCost > 0 {
				r.Write = p.CacheCreationInputTokenCost / p.InputCostPerToken
			}
			if r.Write < 1.0 {
				r.Write = 1.0
			}
			return r
		}
	}
	return CacheRatios{
		Read:  CacheReadRatioFor(api, anthropicRatio, openaiRatio),
		Write: CacheWriteRatioFor(api),
	}
}

// pricingNames lists the names a model may be priced under, upstream id first.
func pricingNames(registry *provider.Registry, model string) []string {
	if model == "" {
		return nil
	}
	if registry != nil {
		if dep, err := registry.GetDeployment(model); err == nil && dep != nil && dep.ProviderModel != "" {
			return []string{dep.ProviderModel, model}
		}
	}
	return []string{model}
}
