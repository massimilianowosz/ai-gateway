package proxy

import (
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// usageCostOf projects a provider usage report onto the pricing package's
// input type, resolving whichever way the provider reported cache usage.
// A nil usage yields a zero breakdown, which prices to zero.
func usageCostOf(u *provider.Usage) pricing.UsageCost {
	if u == nil {
		return pricing.UsageCost{}
	}
	return pricing.UsageCost{
		PromptTokens:        u.PromptTokens,
		CachedPromptTokens:  u.CachedTokens(),
		CacheCreationTokens: u.CacheCreation(),
		CompletionTokens:    u.CompletionTokens,
	}
}

// computeCostUsage is computeCost with the full token breakdown, so cached
// reads and cache writes are billed at their own tariffs instead of full
// input price.
//
// It goes through the same billing-mode gate as computeCost: a "flat"
// deployment (OAuth subscription pass-through) bills nothing regardless of
// what the provider reported.
func computeCostUsage(dep *provider.Deployment, pc *pricing.Calculator, u *provider.Usage) float64 {
	if dep == nil || dep.BillingMode == config.BillingModeFlat {
		return 0
	}
	if pc == nil {
		return 0
	}
	return pc.CostUsage(billingNameFor(pc, dep), usageCostOf(u))
}

// billingNameFor picks the name the price is recorded under.
//
// The upstream id comes first, because it is the more precise of the two: a
// model exposed as "gpt-4o" is served by "gpt-4o-2024-11-20", and the catalog
// prices the dated one. But several config entries can point at one upstream
// id, and when their declared prices disagree none of them is registered under
// it — so a model carrying an explicit, unambiguous price of its own billed
// zero because some other entry happened to share its deployment. Zero is the
// worst answer available here: a budget that accrues nothing never runs out,
// so the funding gate never fires and the key bills forever.
func billingNameFor(pc *pricing.Calculator, dep *provider.Deployment) string {
	if _, known := pc.GetPrice(dep.ProviderModel); known {
		return dep.ProviderModel
	}
	return dep.ModelName
}

// applyUsageTokens copies a provider usage report into a spend record,
// including the prompt-cache breakdown.
//
// CostEstimated marks a row whose cache split the provider never reported: it
// is billed as if nothing were cached, which is the pre-existing behaviour and
// an upper bound on the true cost. A flat-billed row is never an estimate —
// its cost is exactly zero by policy — so the flag stays false there.
func applyUsageTokens(rec *store.SpendRecord, dep *provider.Deployment, u *provider.Usage) {
	if rec == nil || u == nil {
		return
	}
	rec.PromptTokens = u.PromptTokens
	rec.CompletionTokens = u.CompletionTokens
	rec.TotalTokens = u.TotalTokens
	rec.CachedPromptTokens = u.CachedTokens()
	rec.CacheCreationTokens = u.CacheCreation()
	if d := u.CompletionTokensDetails; d != nil {
		rec.ReasoningTokens = d.ReasoningTokens
	}
	rec.CostEstimated = !u.CacheReported() &&
		(dep == nil || dep.BillingMode != config.BillingModeFlat)
}
