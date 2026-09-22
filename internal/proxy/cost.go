package proxy

import (
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// computeCost derives the billable cost for a completion based on the
// deployment's billing mode, independent of how it authenticates upstream
// (auth mode). "flat" (e.g. OAuth subscription pass-through) bills nothing;
// "metered" (default) looks up per-token pricing as usual.
func computeCost(dep *provider.Deployment, pc *pricing.Calculator, promptTokens, completionTokens int) float64 {
	if dep.BillingMode == config.BillingModeFlat {
		return 0
	}
	if pc == nil {
		return 0
	}
	return pc.Cost(dep.ProviderModel, promptTokens, completionTokens)
}
