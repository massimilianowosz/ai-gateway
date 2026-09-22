package proxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func deploymentEligible(ctx context.Context) func(*provider.Deployment) bool {
	return func(dep *provider.Deployment) bool {
		return dep != nil && auth.IsProviderNameAllowed(ctx, dep.ProviderName)
	}
}

func getAuthorizedDeployment(ctx context.Context, registry *provider.Registry, model string) (*provider.Deployment, error) {
	return registry.GetDeploymentMatching(model, deploymentEligible(ctx))
}

// checkModelAccess runs the same model/provider/residency gate completions,
// embeddings, responses and Anthropic messages already apply, for handlers
// that never had it at all: moderations and every media endpoint (image
// generation, TTS, transcription, image edits/variations) went straight to
// CheckBudget with either no model-access check whatsoever or a hand-rolled
// one that only ever consulted the allow-list, never denied_models,
// denied_providers, or residency (GW-02, "allineare il comportamento dei
// percorsi ... media e file"). Writes the same 403 access_denied shape
// those other handlers already use and reports whether the caller may
// proceed, so a handler can `if !checkModelAccess(...) { return }`.
func checkModelAccess(w http.ResponseWriter, r *http.Request, registry *provider.Registry, model string) bool {
	if !auth.IsModelAllowed(r.Context(), model, registry.IsRestricted(model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for model %q", model))
		return false
	}
	if !auth.IsProviderAllowed(r.Context(), registry.ProvidersFor(model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for the provider serving model %q", model))
		return false
	}
	if allEU, hasDeployments := registry.AllDeploymentsEU(model); !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied: model %q is not available in an EU-only deployment", model))
		return false
	}
	return true
}

func getAuthorizedDeploymentByID(ctx context.Context, registry *provider.Registry, model, deploymentID string) (*provider.Deployment, error) {
	dep, err := registry.GetDeploymentByID(model, deploymentID)
	if err != nil {
		return nil, err
	}
	if !auth.IsProviderNameAllowed(ctx, dep.ProviderName) {
		return nil, fmt.Errorf("deployment %q is not authorized for this key", deploymentID)
	}
	return dep, nil
}
