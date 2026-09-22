package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/agenttoken"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

type contextKey string

const (
	keyContextKey              contextKey = "auth_key"
	keyInfoKey                 contextKey = "auth_key_info"
	keyRouteSettingsKey        contextKey = "auth_key_route_settings"
	teamInfoKey                contextKey = "auth_team_info"
	upstreamTokenKey           contextKey = "upstream_token"
	scopedUpstreamTokenKey     contextKey = "scoped_upstream_token"
	scopedUpstreamAccountIDKey contextKey = "scoped_upstream_account_id"
)

const defaultUpstreamTokenHeader = "X-Ubiquum-Upstream-Authorization"
const defaultUpstreamAccountIDHeader = "X-Ubiquum-Upstream-Account-ID"
const upstreamProviderHeader = "X-Ubiquum-Upstream-Provider"

type scopedUpstreamToken struct {
	Provider string
	Token    string
}

type scopedUpstreamAccountID struct {
	Provider  string
	AccountID string
}

// KeyFromContext extracts the raw API key from the request context.
func KeyFromContext(ctx context.Context) string {
	if k, ok := ctx.Value(keyContextKey).(string); ok {
		return k
	}
	return ""
}

// KeyInfoFromContext extracts the resolved API key info from the request context.
func KeyInfoFromContext(ctx context.Context) *store.APIKey {
	if k, ok := ctx.Value(keyInfoKey).(*store.APIKey); ok {
		return k
	}
	return nil
}

type routeSettingsContextValue struct {
	settings *store.KeyRouteSettings
}

// KeyRouteSettingsFromContext returns the route projection captured during
// authentication. The boolean distinguishes a captured empty projection from
// middleware invoked without authentication.
func KeyRouteSettingsFromContext(ctx context.Context) (*store.KeyRouteSettings, bool) {
	value, ok := ctx.Value(keyRouteSettingsKey).(routeSettingsContextValue)
	return value.settings, ok
}

// agentStatusActive is the only status that may use a credential. Mirrors
// AgentStatus in the portal's app/models/agent.py.
const agentStatusActive = "active"

// agentReader is the slice of the store this package needs to answer whether a
// key's agent may still be used. Kept off the Store interface so a test double
// stays compilable without it — the same reason LogSecurityEvent is.
type agentReader interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
}

type governanceStateReader interface {
	GetKeyGovernanceState(ctx context.Context, keyID string) (*store.APIKey, *store.KeyRouteSettings, error)
}

// TeamFromContext extracts the resolved team info from the request context.
func TeamFromContext(ctx context.Context) *store.Team {
	if t, ok := ctx.Value(teamInfoKey).(*store.Team); ok {
		return t
	}
	return nil
}

// UpstreamTokenFromContext extracts the raw client token for pass-through to upstream providers.
func UpstreamTokenFromContext(ctx context.Context) string {
	if t, ok := ctx.Value(upstreamTokenKey).(string); ok {
		return t
	}
	return ""
}

// ContextWithUpstreamToken injects a global pass-through upstream token into
// the context. It is intended for tests and pass-through style integrations
// that don't scope the token to a single provider (see
// ContextWithScopedUpstreamToken for the provider-scoped variant).
func ContextWithUpstreamToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, upstreamTokenKey, token)
}

// UpstreamTokenForProvider extracts a client-supplied upstream token scoped to a
// specific provider. Global pass-through mode remains a fallback for existing
// OAuth-based integrations.
func UpstreamTokenForProvider(ctx context.Context, providerName string) string {
	if token, ok := ctx.Value(scopedUpstreamTokenKey).(scopedUpstreamToken); ok {
		if strings.EqualFold(token.Provider, providerName) {
			return token.Token
		}
	}
	return UpstreamTokenFromContext(ctx)
}

// ScopedUpstreamProvider names the provider a client-supplied token was scoped
// to, or "" when the request carries none. It answers "is this call paid for by
// the caller's own subscription, and to whom".
func ScopedUpstreamProvider(ctx context.Context) string {
	if token, ok := ctx.Value(scopedUpstreamTokenKey).(scopedUpstreamToken); ok {
		return token.Provider
	}
	return ""
}

// ContextWithScopedUpstreamToken injects a scoped upstream token into the
// context. It is intended for tests and provider-level integrations.
func ContextWithScopedUpstreamToken(ctx context.Context, providerName, token string) context.Context {
	return context.WithValue(ctx, scopedUpstreamTokenKey, scopedUpstreamToken{
		Provider: strings.ToLower(strings.TrimSpace(providerName)),
		Token:    strings.TrimSpace(token),
	})
}

// UpstreamAccountIDForProvider extracts a client-supplied upstream account
// identifier (e.g. a ChatGPT workspace/account id) scoped to a specific
// provider. Used alongside UpstreamTokenForProvider by providers whose
// upstream API requires an account id in addition to a bearer token (e.g.
// OpenAI's ChatGPT-backed Codex API, which requires ChatGPT-Account-Id).
func UpstreamAccountIDForProvider(ctx context.Context, providerName string) string {
	if id, ok := ctx.Value(scopedUpstreamAccountIDKey).(scopedUpstreamAccountID); ok {
		if strings.EqualFold(id.Provider, providerName) {
			return id.AccountID
		}
	}
	return ""
}

// ContextWithScopedUpstreamAccountID injects a scoped upstream account id into
// the context. It is intended for tests and provider-level integrations.
func ContextWithScopedUpstreamAccountID(ctx context.Context, providerName, accountID string) context.Context {
	return context.WithValue(ctx, scopedUpstreamAccountIDKey, scopedUpstreamAccountID{
		Provider:  strings.ToLower(strings.TrimSpace(providerName)),
		AccountID: strings.TrimSpace(accountID),
	})
}

// IsModelAllowed checks whether the given model is allowed by both key-level
// and team-level whitelists. An absent whitelist (NULL) means all models are
// allowed; an empty one ([]) denies every model.
// Models in granted_models bypass allowed_models, but never an explicit
// denied_models entry on the key.
// IsModelAllowed reports whether the caller may use model.
//
// restricted says whether the deployment is marked restricted in the registry.
// That flag used to affect only what GET /v1/models listed, which is
// obscurity, not access control: a team with an empty allowed_models fell
// through to "allowed" and could simply name the model in a request. A
// restricted model is now reachable only by a team that holds it in
// granted_models — including a key with no team, which holds nothing.
func IsModelAllowed(ctx context.Context, model string, restricted bool) bool {
	if keyInfo := KeyInfoFromContext(ctx); keyInfo != nil && containsModelFold(keyInfo.DeniedModels, model) {
		return false
	}
	if restricted && !isUnrestrictedPrincipal(ctx) && !isModelGranted(ctx, model) {
		return false
	}
	return isModelAllowed(ctx, model)
}

// IsProviderAllowed reports whether the caller's key may reach at least one
// of the given providers — the distinct providers a requested model's
// deployments run on (Registry.ProvidersFor), not a provider named by the
// caller. A key with no allow-list (nil) may reach any provider; the whole
// point of provider_allowlist is that a policy can only narrow this per key,
// never widen a request beyond what its model whitelist already permits.
// Denied providers are removed first and always override the allow-list.
// An empty providers slice (an unregistered model) has nothing to check here
// — the model lookup itself refuses the request for its own reason.
func IsProviderAllowed(ctx context.Context, providers []string) bool {
	if len(providers) == 0 {
		return true
	}
	keyInfo := KeyInfoFromContext(ctx)
	if keyInfo == nil {
		return true
	}
	for _, p := range providers {
		if IsProviderNameAllowed(ctx, p) {
			return true
		}
	}
	return false
}

// IsProviderNameAllowed evaluates the provider selected for an actual
// deployment. A deny always wins; otherwise nil allow-list means unrestricted
// and a configured allow-list must contain the provider.
func IsProviderNameAllowed(ctx context.Context, providerName string) bool {
	keyInfo := KeyInfoFromContext(ctx)
	if keyInfo == nil {
		return true
	}
	if containsModelFold(keyInfo.DeniedProviders, providerName) {
		return false
	}
	if keyInfo.AllowedProviders == nil {
		return true
	}
	return containsModelFold(keyInfo.AllowedProviders, providerName)
}

// IsResidencyAllowed reports whether the caller's key may use a model given
// that model's own EU-residency status (Registry.AllDeploymentsEU). A key
// with RequireEUResidency=false imposes no constraint. hasDeployments false
// (an unregistered model) has nothing to check here — the model lookup
// itself refuses the request for its own reason.
func IsResidencyAllowed(ctx context.Context, allEU bool, hasDeployments bool) bool {
	if !hasDeployments {
		return true
	}
	keyInfo := KeyInfoFromContext(ctx)
	if keyInfo == nil || !keyInfo.RequireEUResidency {
		return true
	}
	return allEU
}

// BudgetStatus says whether a caller may spend on a model, and if not, why.
type BudgetStatus int

const (
	// BudgetOK: the call may proceed.
	BudgetOK BudgetStatus = iota
	// BudgetUnfunded: the key was never given a budget.
	BudgetUnfunded
	// BudgetExhausted: the key had a budget and has spent it.
	BudgetExhausted
)

// CheckBudget reports whether this caller may spend on this model.
//
// Both halves of the funding gate live here, and both are per model: a key
// with no budget, and a key that has spent the one it had, are equally unable
// to pay — but neither is being asked to. A model the gateway does not pay for
// (an OAuth pass-through deployment, or one billed flat) costs nothing to
// serve, so no budget can be consumed by serving it and none is required.
//
// The exhaustion half used to run in authentication, which sees every request
// but not the model. That blocked flat models on an exhausted metered budget,
// and blocked read-only routes like /v1/models and /v1/key/info as well, so a
// key that had run out could not read its own spend to find out why.
func CheckBudget(ctx context.Context, gatewayBilled bool) BudgetStatus {
	if !gatewayBilled {
		return BudgetOK
	}
	if isUnrestrictedPrincipal(ctx) {
		// The master key is the operator and a pass-through caller pays its own
		// upstream. Neither carries a budget, and neither should be asked for
		// one — the same principals the restriction gate exempts.
		return BudgetOK
	}
	ki := KeyInfoFromContext(ctx)
	if ki == nil {
		return BudgetOK
	}
	if ki.Budget <= 0 {
		return BudgetUnfunded
	}
	if ki.Spend >= ki.Budget {
		return BudgetExhausted
	}
	return BudgetOK
}

// isUnrestrictedPrincipal reports whether the caller is outside the tenancy
// model that per-model restriction expresses.
//
// The master key is the operator, and a pass-through caller carries its own
// upstream credentials — the provider grants that access, not the gateway.
// Neither has a team, so requiring a team grant locked both out of restricted
// models, with the master key able to see one in the listing and not call it.
func isUnrestrictedPrincipal(ctx context.Context) bool {
	ki := KeyInfoFromContext(ctx)
	if ki == nil {
		return false
	}
	return ki.KeyHash == "master" || ki.KeyHash == "passthrough"
}

// isModelGranted reports whether the caller's team was explicitly granted model.
func isModelGranted(ctx context.Context, model string) bool {
	team := TeamFromContext(ctx)
	if team == nil {
		return false
	}
	for _, m := range team.GrantedModels {
		if m == model {
			return true
		}
	}
	return false
}

func isModelAllowed(ctx context.Context, model string) bool {
	// Check key-level whitelist
	if keyInfo := KeyInfoFromContext(ctx); keyInfo != nil && keyInfo.Models != nil {
		if !containsModel(keyInfo.Models, model) {
			return false
		}
	}
	// Check team-level: granted_models always pass
	if team := TeamFromContext(ctx); team != nil {
		if containsModel(team.GrantedModels, model) {
			return true
		}
		// Check allowed_models whitelist
		if team.AllowedModels != nil {
			return containsModel(team.AllowedModels, model)
		}
	}
	return true
}

func containsModel(models store.StringList, model string) bool {
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

func containsModelFold(values store.StringList, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

// ContextWithKeyInfo injects API key info into the context.
// This is intended for use in tests that need to simulate an authenticated request
// without going through the full auth middleware.
func ContextWithKeyInfo(ctx context.Context, key *store.APIKey) context.Context {
	return context.WithValue(ctx, keyInfoKey, key)
}

// ContextWithTeam injects resolved team info into the context.
// Like ContextWithKeyInfo, it exists so a test can simulate an authenticated
// request without running the whole middleware.
func ContextWithTeam(ctx context.Context, team *store.Team) context.Context {
	return context.WithValue(ctx, teamInfoKey, team)
}

// ContextWithCanonicalModels returns a request context whose key and team
// model lists use the same configured names as the registry. The stored rows
// are copied, never mutated, so legacy aliases can remain in the database while
// authorization compares them safely with canonicalized requests.
func ContextWithCanonicalModels(ctx context.Context, canonicalize func(string) string) context.Context {
	if canonicalize == nil {
		return ctx
	}
	if key := KeyInfoFromContext(ctx); key != nil && (len(key.Models) > 0 || len(key.DeniedModels) > 0) {
		keyCopy := *key
		keyCopy.Models = canonicalModelList(key.Models, canonicalize)
		keyCopy.DeniedModels = canonicalModelList(key.DeniedModels, canonicalize)
		ctx = context.WithValue(ctx, keyInfoKey, &keyCopy)
	}
	if team := TeamFromContext(ctx); team != nil {
		teamCopy := *team
		teamCopy.AllowedModels = canonicalModelList(team.AllowedModels, canonicalize)
		teamCopy.GrantedModels = canonicalModelList(team.GrantedModels, canonicalize)
		ctx = context.WithValue(ctx, teamInfoKey, &teamCopy)
	}
	return ctx
}

func canonicalModelList(models store.StringList, canonicalize func(string) string) store.StringList {
	// nil is "no whitelist", which is not the same as an empty one. Rebuilding
	// it as an empty slice would silently revoke every model.
	if models == nil {
		return nil
	}
	result := make(store.StringList, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = canonicalize(model)
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	return result
}

// Middleware handles request authentication.
type Middleware struct {
	// agentTokens is nil when no JWKS endpoint is configured, which leaves the
	// gateway behaving exactly as it did before agent tokens existed.
	agentTokens *agenttoken.Verifier
	masterKey   string
	store       store.Store
	passThrough bool
	// gatewayBilled reports whether serving a model costs the gateway money.
	// Optional: without it every model counts as billed, which is the safe
	// direction for the funding gate.
	gatewayBilled func(model string) bool
}

// NewMiddleware creates an auth middleware with the given master key and store.
func NewMiddleware(masterKey string, db store.Store) *Middleware {
	return &Middleware{masterKey: masterKey, store: db}
}

// SetAgentTokenVerifier makes the gateway accept short-lived agent tokens
// alongside raw virtual keys. Without it, only keys authenticate.
func (m *Middleware) SetAgentTokenVerifier(v *agenttoken.Verifier) {
	m.agentTokens = v
}

// SetGatewayBilledFunc supplies the predicate that decides whether a model is
// paid for by the gateway. It is what lets the funding gate recognise a key
// scoped entirely to OAuth pass-through models.
func (m *Middleware) SetGatewayBilledFunc(f func(model string) bool) {
	m.gatewayBilled = f
}

// NewPassThroughMiddleware creates an auth middleware that forwards client tokens
// to upstream providers without validation. Used for OAuth-based tools like
// Claude Code Max and GitHub Copilot.
func NewPassThroughMiddleware() *Middleware {
	return &Middleware{passThrough: true}
}

// UpstreamTokenForwardingOptions configures scoped forwarding of a client
// supplied bearer token to one approved upstream provider.
type UpstreamTokenForwardingOptions struct {
	Enabled          bool
	Header           string
	AccountIDHeader  string
	AllowedProviders []string
}

// NewUpstreamTokenForwardingMiddleware extracts a separate upstream bearer token
// from a configured header and stores it in context for the named provider. The
// header is always removed before downstream middleware so secrets are not sent
// to hooks or request logs.
func NewUpstreamTokenForwardingMiddleware(opts UpstreamTokenForwardingOptions) func(http.Handler) http.Handler {
	headerName := strings.TrimSpace(opts.Header)
	if headerName == "" {
		headerName = defaultUpstreamTokenHeader
	}
	accountIDHeaderName := strings.TrimSpace(opts.AccountIDHeader)
	if accountIDHeaderName == "" {
		accountIDHeaderName = defaultUpstreamAccountIDHeader
	}

	allowed := make(map[string]struct{}, len(opts.AllowedProviders))
	for _, providerName := range opts.AllowedProviders {
		providerName = strings.ToLower(strings.TrimSpace(providerName))
		if providerName != "" {
			allowed[providerName] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawTokenHeader := strings.TrimSpace(r.Header.Get(headerName))
			rawAccountID := strings.TrimSpace(r.Header.Get(accountIDHeaderName))
			// Always strip these before they can reach hooks, request logs, or
			// provider-agnostic middleware, even when forwarding is disabled,
			// malformed, or only one of the two headers is present.
			r.Header.Del(accountIDHeaderName)
			if rawTokenHeader == "" {
				r.Header.Del(headerName)
				next.ServeHTTP(w, r)
				return
			}
			r.Header.Del(headerName)

			// Rejecting rather than ignoring is deliberate: a caller that sends
			// an upstream token expects it to be used, and silently falling back
			// to the gateway's own credentials would bill and identify the
			// request as someone else. The cost is that a client sending the
			// header unconditionally gets 403 on routes where the token is
			// irrelevant, /v1/models included.
			if !opts.Enabled {
				writeAuthStatusError(w, http.StatusForbidden, "upstream token forwarding is disabled")
				return
			}

			providerName := strings.ToLower(strings.TrimSpace(r.Header.Get(upstreamProviderHeader)))
			if providerName == "" {
				writeAuthStatusError(w, http.StatusBadRequest, "upstream provider header is required")
				return
			}

			if len(allowed) > 0 {
				if _, ok := allowed[providerName]; !ok {
					writeAuthStatusError(w, http.StatusForbidden, "upstream provider is not allowed")
					return
				}
			}

			token := extractBearerTokenValue(rawTokenHeader)
			if token == "" {
				writeAuthStatusError(w, http.StatusBadRequest, "upstream authorization must use Bearer token")
				return
			}

			ctx := ContextWithScopedUpstreamToken(r.Context(), providerName, token)
			if rawAccountID != "" {
				ctx = ContextWithScopedUpstreamAccountID(ctx, providerName, rawAccountID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Authenticate validates the Authorization header.
// Supports master key (full access), virtual keys (per-key permissions),
// and pass-through mode (forwards token to upstream without validation).
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := extractBearerToken(r)
		if key == "" {
			writeAuthError(w, "missing Authorization header")
			return
		}

		// Pass-through mode: forward token to upstream without validation
		if m.passThrough {
			ctx := context.WithValue(r.Context(), keyContextKey, key)
			ctx = context.WithValue(ctx, upstreamTokenKey, key)
			ctx = context.WithValue(ctx, keyInfoKey, &store.APIKey{
				KeyPrefix: "passthrough",
				KeyHash:   "passthrough",
				Name:      "passthrough",
				Active:    true,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Master key grants full access
		if subtle.ConstantTimeCompare([]byte(key), []byte(m.masterKey)) == 1 {
			prefix := keyPrefixOf(key)
			ctx := context.WithValue(r.Context(), keyContextKey, key)
			ctx = context.WithValue(ctx, keyInfoKey, &store.APIKey{
				KeyPrefix: prefix,
				KeyHash:   "master",
				Name:      "master",
				Active:    true,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Virtual key validation
		if m.store == nil {
			writeAuthError(w, "invalid API key")
			return
		}

		// An agent token names the key it speaks for, so from here the request
		// is indistinguishable from one made with that key: same budget, same
		// model whitelist, same spend attribution. Authentication changed;
		// authorization did not.
		hash := HashKey(key)
		if m.agentTokens != nil && agenttoken.LooksLikeToken(key) {
			claims, err := m.agentTokens.Verify(r.Context(), key)
			if err != nil {
				writeAuthError(w, "invalid or expired agent token")
				return
			}
			hash = claims.KeyHash
		}
		var apiKey *store.APIKey
		var keyRouteSettings *store.KeyRouteSettings
		var governanceStateCaptured bool
		var err error
		if reader, ok := m.store.(governanceStateReader); ok {
			governanceStateCaptured = true
			apiKey, keyRouteSettings, err = reader.GetKeyGovernanceState(r.Context(), hash)
		} else {
			apiKey, err = m.store.GetKeyByHash(r.Context(), hash)
		}
		if err != nil {
			writeAuthError(w, "internal error validating key")
			return
		}
		if apiKey == nil {
			writeAuthError(w, "invalid API key")
			return
		}
		if apiKey.GovernanceRequired && apiKey.GovernanceRevision == 0 {
			writeAuthTypedStatusError(w, http.StatusForbidden, "governed scope has no applied snapshot", "governance_not_ready", "governance_not_ready")
			return
		}
		if governanceStateCaptured && apiKey.GovernanceRevision > 0 && (keyRouteSettings == nil || keyRouteSettings.GovernanceRevision != apiKey.GovernanceRevision) {
			writeAuthStatusError(w, http.StatusServiceUnavailable, "governance state changed during request admission; retry")
			return
		}

		// Check if key is active
		if !apiKey.Active {
			writeAuthError(w, "API key is disabled")
			return
		}

		// A scope whose policy composition is invalid stops here, before
		// anything downstream — including CheckBudget's flat/BYOK economic
		// exemption, which only decides whether spend is tracked, not
		// whether the caller is authorized. Distinct from Active (an
		// unrelated manual admin kill switch): this is Ubiquum's policy
		// engine saying the desired state for this scope cannot be trusted,
		// for any of the 12 policy kinds, not just the ones with their own
		// per-field deny projection. See store.APIKey.GovernanceBlocked.
		if apiKey.GovernanceBlocked {
			reason := apiKey.GovernanceBlockReason
			if reason == "" {
				reason = "policy composition invalid for this scope"
			}
			writeAuthTypedStatusError(w, http.StatusForbidden, reason, "governance_blocked", "governance_blocked")
			return
		}

		// Check expiration
		if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
			writeAuthError(w, "API key has expired")
			return
		}

		// A suspended agent stops here.
		//
		// This one is deliberately not a shadow-mode policy. Suspension is
		// revocation, not a rule to try out: an administrator reaches for it
		// when an agent is misbehaving now, and a kill switch that only
		// records what it would have done is not a kill switch. Keys with no
		// agent are untouched.
		if apiKey.AgentID != "" {
			if reader, ok := m.store.(agentReader); ok {
				agent, err := reader.GetAgent(r.Context(), apiKey.AgentID)
				if err != nil {
					writeAuthStatusError(w, http.StatusInternalServerError, "internal error validating agent")
					return
				}
				// A missing agent is a dangling reference, not permission. The
				// row is only ever removed by hand, and a credential pointing
				// at nothing has no owner to answer for it.
				if agent == nil {
					writeAuthTypedStatusError(w, http.StatusForbidden,
						"the agent this key belongs to no longer exists", "agent_unavailable", "agent_unavailable")
					return
				}
				if agent.Status != agentStatusActive {
					writeAuthTypedStatusError(w, http.StatusForbidden,
						fmt.Sprintf("agent %q is %s", agent.Name, agent.Status), "agent_unavailable", "agent_unavailable")
					return
				}
			}
		}

		// The funding gate is deliberately not here. Budget is not optional and
		// zero is not unlimited, but whether a call needs a budget at all
		// depends on the model, which is in the body and not yet parsed —
		// CheckBudget decides at the handler, where it is known. Blocking here
		// would take flat models and read-only routes down with it.

		// A stored row without a prefix is not a cosmetic gap: every figure the
		// dashboard reports for a tenant — spend, savings, compression — is
		// aggregated by joining on key_prefix, so a key missing one spends
		// money that no tenant is ever credited with. Rows created outside the
		// gateway have arrived without it.
		//
		// The prefix is the head of the key the caller just presented, so it is
		// derivable here and nowhere later. Filling it in memory keeps every
		// downstream record attributable without touching the row on the hot
		// path.
		if apiKey.KeyPrefix == "" {
			apiKey.KeyPrefix = keyPrefixOf(key)
		}

		// Store key info in context for downstream use (spend tracking, model filtering)
		ctx := context.WithValue(r.Context(), keyContextKey, key)
		ctx = context.WithValue(ctx, keyInfoKey, apiKey)
		if governanceStateCaptured {
			ctx = context.WithValue(ctx, keyRouteSettingsKey, routeSettingsContextValue{settings: keyRouteSettings})
		}

		if apiKey.TeamID != "" {
			team, err := m.store.GetTeam(r.Context(), apiKey.TeamID)
			if err != nil {
				writeAuthStatusError(w, http.StatusInternalServerError, "internal error validating team")
				return
			}
			if team == nil {
				writeAuthTypedStatusError(w, http.StatusForbidden, "team is disabled or not found", "team_disabled", "team_disabled")
				return
			}
			if team.Budget > 0 && team.Spend >= team.Budget {
				writeBudgetExceeded(w, "budget exceeded for this team")
				return
			}
			ctx = context.WithValue(ctx, teamInfoKey, team)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RecheckGovernance closes the queue/preprocessing window between initial
// authentication and an external effect. A revision change makes the caller
// retry under the new complete projection; a revocation fails immediately.
func (m *Middleware) RecheckGovernance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted := KeyInfoFromContext(r.Context())
		if admitted == nil || admitted.KeyHash == "" || admitted.KeyHash == "master" || m.store == nil || m.passThrough {
			next.ServeHTTP(w, r)
			return
		}
		var current *store.APIKey
		var route *store.KeyRouteSettings
		var err error
		if reader, ok := m.store.(governanceStateReader); ok {
			current, route, err = reader.GetKeyGovernanceState(r.Context(), admitted.KeyHash)
		} else {
			current, err = m.store.GetKeyByHash(r.Context(), admitted.KeyHash)
		}
		if err != nil {
			writeAuthStatusError(w, http.StatusServiceUnavailable, "unable to recheck governance state")
			return
		}
		if current == nil || !current.Active {
			writeAuthTypedStatusError(w, http.StatusForbidden, "API key was revoked during request processing", "governance_revoked", "governance_revoked")
			return
		}
		if current.GovernanceRequired && current.GovernanceRevision == 0 {
			writeAuthTypedStatusError(w, http.StatusForbidden, "governed scope has no applied snapshot", "governance_not_ready", "governance_not_ready")
			return
		}
		if current.ExpiresAt != nil && time.Now().After(*current.ExpiresAt) {
			writeAuthTypedStatusError(w, http.StatusForbidden, "API key expired during request processing", "governance_revoked", "governance_revoked")
			return
		}
		if current.GovernanceBlocked {
			writeAuthTypedStatusError(w, http.StatusForbidden, "governance blocked the scope during request processing", "governance_blocked", "governance_blocked")
			return
		}
		if current.AgentID != "" {
			if reader, ok := m.store.(agentReader); ok {
				agent, readErr := reader.GetAgent(r.Context(), current.AgentID)
				if readErr != nil {
					writeAuthStatusError(w, http.StatusServiceUnavailable, "unable to recheck agent state")
					return
				}
				if agent == nil || agent.Status != agentStatusActive {
					writeAuthTypedStatusError(w, http.StatusForbidden, "agent was revoked during request processing", "agent_unavailable", "agent_unavailable")
					return
				}
			}
		}
		if current.GovernanceRevision != admitted.GovernanceRevision || current.GovernanceDigest != admitted.GovernanceDigest ||
			(current.GovernanceRevision > 0 && (route == nil || route.GovernanceRevision != current.GovernanceRevision)) {
			w.Header().Set("Retry-After", "0")
			writeAuthStatusError(w, http.StatusServiceUnavailable, "governance revision changed during request processing; retry")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HashKey computes the SHA-256 hash of an API key.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if token := extractBearerTokenValue(auth); token != "" {
		return token
	}
	// Fallback: Anthropic SDK sends key via x-api-key header (Claude Code, etc.)
	if xKey := strings.TrimSpace(r.Header.Get("X-Api-Key")); xKey != "" {
		return xKey
	}
	return ""
}

func extractBearerTokenValue(auth string) string {
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func writeAuthError(w http.ResponseWriter, message string) {
	writeAuthStatusError(w, http.StatusUnauthorized, message)
}

func writeAuthStatusError(w http.ResponseWriter, status int, message string) {
	writeAuthTypedStatusError(w, status, message, "authentication_error", "invalid_api_key")
}

func writeAuthTypedStatusError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
}

func writeBudgetExceeded(w http.ResponseWriter, message string) {
	// 402, not 429: a rate limit and an exhausted budget need different
	// fixes, and a client checking only the status code should be able to
	// tell them apart.
	writeAuthTypedStatusError(w, http.StatusPaymentRequired, message, "budget_exceeded", "budget_exceeded")
}

// OwnerID derives the tenant key that scopes stored resources — files,
// responses, conversations — from the authenticated caller.
//
// A team owns its resources collectively; a key with no team owns its own. A
// passthrough key has no stored hash, so the presented key is hashed on the
// spot. An empty result means the caller cannot own anything, and callers treat
// that as "storage is off for this request".
func OwnerID(ctx context.Context) string {
	keyInfo := KeyInfoFromContext(ctx)
	if keyInfo == nil {
		return ""
	}
	if keyInfo.TeamID != "" {
		return "team:" + keyInfo.TeamID
	}
	keyHash := keyInfo.KeyHash
	if keyHash == "passthrough" {
		keyHash = HashKey(KeyFromContext(ctx))
	}
	if keyHash == "" {
		return ""
	}
	return "key:" + keyHash
}

// keyPrefixOf returns the identifier a key is reported under: the first ten
// characters, which is what key creation stores and what every spend row and
// metric is grouped by.
func keyPrefixOf(key string) string {
	if len(key) > 10 {
		return key[:10]
	}
	return key
}
