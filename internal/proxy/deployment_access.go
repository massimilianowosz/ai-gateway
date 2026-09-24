package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
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

// unavailableModelPhrase explains a routing miss.
//
// A model served only by an OAuth pass-through deployment is unreachable
// without a scoped upstream token, and reporting it as simply unavailable is
// misleading: the deployment exists, the credential does not. Saying so turns
// a dead end into an instruction.
func unavailableModelPhrase(registry *provider.Registry, model string) string {
	if registry != nil {
		if providers := registry.UpstreamAliasProviders(model); len(providers) > 0 {
			return fmt.Sprintf(
				"model %q is only served by an OAuth pass-through deployment: forward a scoped %s token, or configure a metered deployment under this name",
				model, strings.Join(providers, "/"))
		}
	}
	return fmt.Sprintf("model %q is not available", model)
}

// deniedModelPhrase names the model in terms the caller recognises.
//
// A pass-through deployment is renamed in our catalogue, so a request for
// claude-sonnet-5 is refused as claude-code-sonnet-5 — a string the caller
// never sent and cannot find in its own configuration. Naming both makes the
// refusal say which allow-list entry is actually missing.
//
// The client's own name is read from the context rather than the body: the
// alias middleware rewrote the body before the handler parsed it, so by here
// the original spelling survives only in the context.
func deniedModelPhrase(ctx context.Context, resolved string) string {
	requested := middleware.RequestedModelFromContext(ctx)
	if requested == "" || requested == resolved {
		return fmt.Sprintf("model %q", resolved)
	}
	return fmt.Sprintf("model %q (requested as %q)", resolved, requested)
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
