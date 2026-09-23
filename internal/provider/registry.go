package provider

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// Registry maps model names to their available deployments.
type Registry struct {
	mu               sync.RWMutex
	deployments      map[string][]*Deployment // model name → deployments
	allModels        []string                 // ordered list of unique model names
	restrictedModels map[string]bool          // models hidden from listing unless explicitly allowed
	consoleHidden    map[string]bool          // data-plane models omitted from the appliance console
	aliases          map[string]string        // public alias → configured model name
	// upstream provider name → the provider's own model name → our name, for
	// deployments the caller's own subscription answers.
	upstreamAliases map[string]map[string]string
}

// NewRegistry creates a provider registry from the given configuration.
// Providers that fail to initialize are logged and skipped (partial startup).
func NewRegistry(models []config.ModelConfig, factory ProviderFactory) (*Registry, error) {
	return NewRegistryWithAliases(models, nil, factory)
}

// NewRegistryWithAliases creates a provider registry with additive public
// aliases. Aliases resolve to an existing configured model without duplicating
// its deployments, so routing, circuit breaking, billing and provider affinity
// continue to use the established model pool.
func NewRegistryWithAliases(models []config.ModelConfig, aliases map[string]string, factory ProviderFactory) (*Registry, error) {
	r := &Registry{
		deployments:      make(map[string][]*Deployment),
		restrictedModels: make(map[string]bool),
		consoleHidden:    make(map[string]bool),
		aliases:          make(map[string]string, len(aliases)),
		upstreamAliases:  make(map[string]map[string]string),
	}

	configuredModels := make(map[string]struct{}, len(models))
	for _, m := range models {
		configuredModels[m.Name] = struct{}{}
	}
	for alias, target := range aliases {
		if alias == "" || target == "" {
			return nil, fmt.Errorf("model aliases require non-empty alias and target")
		}
		if _, conflict := configuredModels[alias]; conflict {
			return nil, fmt.Errorf("model alias %q conflicts with a configured model", alias)
		}
		if _, chained := aliases[target]; chained {
			return nil, fmt.Errorf("model alias %q must point directly to a configured model, not alias %q", alias, target)
		}
		if _, exists := configuredModels[target]; !exists {
			return nil, fmt.Errorf("model alias %q references unknown model %q", alias, target)
		}
		r.aliases[alias] = target
	}

	seen := make(map[string]bool)
	var skipped int
	for i, m := range models {
		p, err := factory.Create(m)
		if err != nil {
			// Log warning and skip this deployment instead of failing the entire registry
			fmt.Printf("[WARN] skipping model %q: %v\n", m.Name, err)
			skipped++
			continue
		}

		// Use profile name if available, otherwise fall back to provider type
		provName := m.ProviderProfile
		if provName == "" {
			provName = m.Provider
		}

		d := &Deployment{
			ID:            fmt.Sprintf("%s-%d", m.Name, i),
			ModelName:     m.Name,
			ProviderName:  provName,
			ProviderModel: m.ProviderModel,
			Provider:      p,
			DropParams:    m.DropParams,
			IsEU:          m.IsEU,
			AuthMode:      m.AuthMode,
			BillingMode:   m.BillingMode,
		}

		r.deployments[m.Name] = append(r.deployments[m.Name], d)
		if m.AuthMode == config.AuthModeOAuthPassthrough && m.ProviderModel != "" {
			upstream := p.Name()
			if r.upstreamAliases[upstream] == nil {
				r.upstreamAliases[upstream] = make(map[string]string)
			}
			r.upstreamAliases[upstream][m.ProviderModel] = m.Name
		}
		if !seen[m.Name] {
			seen[m.Name] = true
			r.allModels = append(r.allModels, m.Name)
			if m.Restricted {
				r.restrictedModels[m.Name] = true
			}
			if m.ConsoleHidden {
				r.consoleHidden[m.Name] = true
			}
		}
	}

	if len(r.allModels) == 0 && len(models) > 0 {
		return nil, fmt.Errorf("all %d models failed to register", len(models))
	}

	return r, nil
}

// CanonicalModel returns the configured model name for a public alias. Unknown
// names are returned unchanged so handlers retain their existing model-not-found
// behavior.
func (r *Registry) CanonicalModel(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.canonicalModelLocked(model)
}

func (r *Registry) canonicalModelLocked(model string) string {
	if target, ok := r.aliases[model]; ok {
		return target
	}
	return model
}

// GetDeployment returns a random deployment for the given model name.
// Returns an error if the model is not registered.
func (r *Registry) GetDeployment(model string) (*Deployment, error) {
	return r.GetDeploymentMatching(model, nil)
}

// GetDeploymentMatching returns a random deployment among those accepted by
// eligible. It is used by request authorization to remove forbidden providers
// before random selection, rather than checking only that some allowed
// provider exists and then accidentally selecting a denied one.
func (r *Registry) GetDeploymentMatching(model string, eligible func(*Deployment) bool) (*Deployment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model = r.canonicalModelLocked(model)
	deps, ok := r.deployments[model]
	if !ok || len(deps) == 0 {
		return nil, fmt.Errorf("model %q not found", model)
	}

	candidates := deps
	if eligible != nil {
		candidates = make([]*Deployment, 0, len(deps))
		for _, dep := range deps {
			if eligible(dep) {
				candidates = append(candidates, dep)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no authorized deployments available for model %q", model)
	}

	// Simple shuffle: pick a random deployment.
	return candidates[rand.Intn(len(candidates))], nil
}

// GetDeployments returns all deployments for the given model name.
// Returns an error if the model is not registered.
func (r *Registry) GetDeployments(model string) ([]*Deployment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model = r.canonicalModelLocked(model)
	deps, ok := r.deployments[model]
	if !ok || len(deps) == 0 {
		return nil, fmt.Errorf("model %q not found", model)
	}

	result := make([]*Deployment, len(deps))
	copy(result, deps)
	return result, nil
}

// ProvidersFor returns the distinct provider names among model's deployments
// (post-alias resolution), for a provider_allowlist check: a model refusable
// only because none of its deployments run on an allowed provider is
// refused the same way IsModelAllowed refuses one absent from a model
// whitelist. Unknown or unregistered models return nil, which callers should
// treat as "nothing to check" rather than "no providers allowed" — a
// not-found model fails for its own reason downstream, not this one.
func (r *Registry) ProvidersFor(model string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model = r.canonicalModelLocked(model)
	deps := r.deployments[model]
	if len(deps) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(deps))
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if !seen[d.ProviderName] {
			seen[d.ProviderName] = true
			out = append(out, d.ProviderName)
		}
	}
	return out
}

// AllDeploymentsEU reports whether every deployment registered for model runs
// in the EU region (Deployment.IsEU), and whether the model has any
// deployments at all. Residency is a compliance claim, not a best-effort
// preference like provider_allowlist's "at least one" — a model that can fail
// over to a non-EU deployment must not be reported as EU-resident, so this
// requires ALL of them, not just one. hasDeployments false (an unregistered
// model) means there is nothing to check here — the model lookup itself
// refuses the request for its own reason.
func (r *Registry) AllDeploymentsEU(model string) (allEU bool, hasDeployments bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model = r.canonicalModelLocked(model)
	deps := r.deployments[model]
	if len(deps) == 0 {
		return false, false
	}
	for _, d := range deps {
		if !d.IsEU {
			return false, true
		}
	}
	return true, true
}

// GetDeploymentByID returns the exact deployment registered for a model.
// File references use this to remain pinned to the provider account that owns
// the upstream file.
func (r *Registry) GetDeploymentByID(model, deploymentID string) (*Deployment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model = r.canonicalModelLocked(model)
	deps, ok := r.deployments[model]
	if !ok {
		return nil, fmt.Errorf("model %q not found", model)
	}
	for _, dep := range deps {
		if dep.ID == deploymentID {
			return dep, nil
		}
	}
	return nil, fmt.Errorf("deployment %q not found for model %q", deploymentID, model)
}

// ListModels returns all registered model names.
func (r *Registry) ListModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listedModelsLocked()
}

// ListConsoleModels returns the models operators should see in the appliance
// UI. Hidden models remain registered, routable and visible on the data-plane
// catalog; this is presentation metadata, not an authorization mechanism.
func (r *Registry) ListConsoleModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	listed := r.listedModelsLocked()
	result := make([]string, 0, len(listed))
	for _, model := range listed {
		if !r.consoleHidden[r.canonicalModelLocked(model)] {
			result = append(result, model)
		}
	}
	return result
}

// Aliases returns a copy of the public alias → configured model mapping.
// Callers outside the gateway need it to compare a stored model list against
// the ids /v1/models publishes: a whitelist written before an alias existed
// holds the configured name, and only this mapping relates the two.
func (r *Registry) Aliases() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.aliases))
	for alias, target := range r.aliases {
		out[alias] = target
	}
	return out
}

// UpstreamAlias returns our name for a model the given upstream provider calls
// something else.
//
// The catalogue name keeps a pass-through model apart from a metered one, but
// the client's own model picker only knows the provider's name. Accepting both
// keeps that distinction ours instead of making it the caller's problem.
func (r *Registry) UpstreamAlias(upstreamProvider, providerModel string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name, ok := r.upstreamAliases[upstreamProvider][providerModel]
	return name, ok
}

// ListVisibleModels returns models visible to a tenant.
// - Public models are always visible (filtered by allowedModels if set).
// - Restricted models are visible ONLY if explicitly listed in grantedModels.
//
// A nil allowedModels is "no whitelist", which shows the whole public catalog.
// An empty non-nil one is a whitelist that grants nothing, and shows none of
// it — the two must not be conflated, or revoking every model would read as
// never having restricted any.
func (r *Registry) ListVisibleModels(allowedModels, grantedModels []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	granted := make(map[string]bool, len(grantedModels))
	for _, m := range grantedModels {
		granted[r.canonicalModelLocked(m)] = true
	}

	allowed := make(map[string]bool, len(allowedModels))
	for _, m := range allowedModels {
		allowed[r.canonicalModelLocked(m)] = true
	}

	var result []string
	for _, m := range r.listedModelsLocked() {
		canonical := r.canonicalModelLocked(m)
		if r.restrictedModels[canonical] {
			// Restricted: only visible if explicitly granted
			if granted[canonical] {
				result = append(result, m)
			}
		} else {
			// Public: visible unless allowed_models is set and excludes it
			if allowedModels == nil || allowed[canonical] {
				result = append(result, m)
			}
		}
	}
	return result
}

// listedModelsLocked returns the public catalog. When a configured model has
// an alias, the alias replaces the provider-prefixed target in listings while
// the target remains fully callable for backward compatibility.
func (r *Registry) listedModelsLocked() []string {
	aliasedTargets := make(map[string]struct{}, len(r.aliases))
	aliases := make([]string, 0, len(r.aliases))
	for alias, target := range r.aliases {
		if len(r.deployments[target]) == 0 {
			continue
		}
		aliasedTargets[target] = struct{}{}
		aliases = append(aliases, alias)
	}

	result := make([]string, 0, len(r.allModels)+len(aliases))
	for _, model := range r.allModels {
		if _, replaced := aliasedTargets[model]; !replaced {
			result = append(result, model)
		}
	}
	// Map iteration is deliberately sorted so /v1/models remains stable across
	// replicas and restarts.
	sort.Strings(aliases)
	return append(result, aliases...)
}

// IsRestricted returns whether a model is marked as restricted.
func (r *Registry) IsRestricted(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	model = r.canonicalModelLocked(model)
	return r.restrictedModels[model]
}

// IsGatewayBilled reports whether serving this model costs the gateway money.
//
// Only a model every one of whose deployments is billed flat is free to the
// gateway: those are the OAuth subscription pass-throughs, paid for by the
// caller's own upstream credentials. An unknown model counts as billed, which
// is the safe direction for a caller exempt from the budget gate.
func (r *Registry) IsGatewayBilled(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	deps := r.deployments[model]
	if len(deps) == 0 {
		return true
	}
	for _, dep := range deps {
		if dep.BillingMode != config.BillingModeFlat {
			return true
		}
	}
	return false
}

// AddDeployment registers a model deployment at runtime (e.g., live edge models).
// Thread-safe; idempotent by deployment ID.
//
// The idempotency is not a nicety. Edge runtimes re-register on every
// heartbeat, so an unconditional append grew the list for one model without
// bound, and every extra entry was another chance for the router to pick a
// duplicate of the same endpoint.
func (r *Registry) AddDeployment(name string, dep *Deployment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// An empty ID is not an identity: config-driven deployments leave it unset
	// and several of them under one model name is exactly how load balancing
	// works. Only an identified deployment replaces its namesake.
	if dep.ID != "" {
		for i, existing := range r.deployments[name] {
			if existing.ID == dep.ID {
				r.deployments[name][i] = dep
				return
			}
		}
	}
	r.deployments[name] = append(r.deployments[name], dep)
	for _, m := range r.allModels {
		if m == name {
			return
		}
	}
	r.allModels = append(r.allModels, name)
}

// RemoveDeployments removes all deployments for a model name at runtime.
func (r *Registry) RemoveDeployments(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deployments, name)
	filtered := r.allModels[:0]
	for _, m := range r.allModels {
		if m != name {
			filtered = append(filtered, m)
		}
	}
	r.allModels = filtered
}

// RemoveDeployment removes one runtime deployment without disturbing another
// provider that exposes the same public model name.
func (r *Registry) RemoveDeployment(name, deploymentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deps := r.deployments[name]
	filtered := deps[:0]
	for _, dep := range deps {
		if dep.ID != deploymentID {
			filtered = append(filtered, dep)
		}
	}
	if len(filtered) > 0 {
		r.deployments[name] = filtered
		return
	}
	delete(r.deployments, name)
	models := r.allModels[:0]
	for _, model := range r.allModels {
		if model != name {
			models = append(models, model)
		}
	}
	r.allModels = models
}

// ProviderFactory creates Provider instances from model configuration.
type ProviderFactory interface {
	Create(cfg config.ModelConfig) (Provider, error)
}
