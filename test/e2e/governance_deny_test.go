package e2e

// This file exercises, through the real HTTP route chain and a counted
// scripted upstream, the governance surfaces landed this session:
// api_keys_local.governance_blocked (UBIQUUM_MATURITY_IMPLEMENTATION_PLAN.md
// POL-01's scope-authorization state) and denied_models/denied_providers
// (the same plan's deny-propagation work). Every other test in this package
// checks the request layer or the auth package in isolation; these prove the
// two never let a call reach the upstream when they should refuse it, using
// scriptedFactory's real call counter rather than trusting the status code
// alone (VAL-02: "non usare mock che saltano il gate da verificare").
//
// Not attempted here: driving this through MH's own HTTP API and a real
// policy publish/bind — these seed the exact same api_keys_local columns
// policy_gateway_sync's projection writes, without needing MH's Python
// process or a second database. The MH-side write path already has its own
// direct coverage (tests/backend/test_policy_gateway_sync.py); what was
// missing, and what this closes, is proof that the GW request path actually
// refuses on those columns end-to-end rather than at the unit level only.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/server"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// TestGovernanceBlocked_RefusesRequestAndNeverCallsUpstream pins the
// scope-authorization gate (store.APIKey.GovernanceBlocked, checked in
// Authenticate before CheckBudget): a fully funded, otherwise-unrestricted
// key must still be refused outright, and the scripted upstream must never
// see the request.
func TestGovernanceBlocked_RefusesRequestAndNeverCallsUpstream(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	key := g.mintKey(100, "paid")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		GovernanceBlocked:     boolPtr(true),
		GovernanceBlockReason: strPtr("invalid_enforcing_composition:model_routing"),
	}))

	resp := g.post("/v1/chat/completions", key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)

	assert.Equal(t, http.StatusForbidden, resp.code, resp.body)
	assert.Contains(t, resp.body, "governance_blocked")
	assert.Contains(t, resp.body, "model_routing")
	assert.Equal(t, 0, g.scripts.callCount(), "a governance-blocked key must never reach the upstream")
}

// TestGovernanceBlocked_UnblockingRestoresAccess proves the gate is not
// sticky by accident: clearing the flag (exactly what a valid revision
// landing would do) must let the very next request through.
func TestGovernanceBlocked_UnblockingRestoresAccess(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	key := g.mintKey(100, "paid")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		GovernanceBlocked: boolPtr(true),
	}))
	blocked := g.post("/v1/chat/completions", key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusForbidden, blocked.code)

	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		GovernanceBlocked:     boolPtr(false),
		GovernanceBlockReason: strPtr(""),
	}))
	allowed := g.post("/v1/chat/completions", key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)

	assert.Equal(t, http.StatusOK, allowed.code, allowed.body)
	assert.Equal(t, 1, g.scripts.callCount())
}

// TestGovernanceBlocked_AppliesToAFlatModelRegardlessOfZeroBudget pins GW-02:
// "eseguire identità, stato dello scope e autorizzazione prima e
// indipendentemente da CheckBudget — un deny applicabile deve bloccare anche
// flat/BYOK/pass-through governato". A flat (OAuth pass-through) model costs
// the gateway nothing and is exempt from the funding gate entirely — see
// TestFundingGate_UnfundedKeyReachesAFlatModel — so this key is deliberately
// both unfunded AND governance-blocked: if the economic exemption ran before,
// or instead of, the governance gate, this would incorrectly succeed. It
// cannot, structurally, because GovernanceBlocked is checked in Authenticate,
// which the funding/billing-mode logic (in the handler) never even runs
// ahead of — this test is what actually exercises that ordering for a flat
// model specifically, which no other test in this package does.
func TestGovernanceBlocked_AppliesToAFlatModelRegardlessOfZeroBudget(t *testing.T) {
	g := newGateway(t, []string{"oauth"}, "oauth")
	key := g.seedUnfundedKey("oauth")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		GovernanceBlocked:     boolPtr(true),
		GovernanceBlockReason: strPtr("invalid_enforcing_composition:spend_budget"),
	}))

	resp := g.post("/v1/chat/completions", key,
		`{"model":"oauth","messages":[{"role":"user","content":"hi"}]}`)

	assert.Equal(t, http.StatusForbidden, resp.code, resp.body)
	assert.Equal(t, 0, g.scripts.callCount(), "the economic exemption for a flat model must never let a governance-blocked key reach the upstream")
}

// TestGovernanceBlocked_AppliesEvenWhenForwardingAScopedBYOKToken pins the
// other half of GW-02's "flat/BYOK/pass-through governato" guarantee: BYOK
// here is UpstreamTokenForwarding — a real, governed virtual key that also
// forwards its own separate provider credential (the shape config/types.go's
// own doc comment describes: "a virtual key while forwarding a separate
// provider token"), never the whole-gateway m.passThrough mode, which never
// validates a key at all and is deliberately out of this perimeter. The
// upstream-token-forwarding middleware runs strictly after Authenticate in
// the real route chain (internal/server/routes.go); this is what actually
// exercises that a governance-blocked key is refused before its forwarded
// token is ever consulted, not just before CheckBudget.
func TestGovernanceBlocked_AppliesEvenWhenForwardingAScopedBYOKToken(t *testing.T) {
	g := newGatewayOpts(t, []string{"paid"}, options{byokProviders: []string{"scripted"}})
	key := g.mintKey(100, "paid")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		GovernanceBlocked: boolPtr(true),
	}))

	req := g.request("POST", "/v1/chat/completions", key, "application/json",
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-Ubiquum-Upstream-Authorization", "Bearer byok-caller-token")
	req.Header.Set("X-Ubiquum-Upstream-Provider", "scripted")
	resp := g.send(req)

	assert.Equal(t, http.StatusForbidden, resp.code, resp.body)
	assert.Equal(t, 0, g.scripts.callCount(), "a forwarded BYOK credential must never let a governance-blocked key reach the upstream")
}

// TestDeniedModels_RefusesThatModelButAllowsOthers pins denied_models taking
// precedence over the key's own allow-list — models is set below, so this is
// also proof the deny is not merely "no allow-list configured".
func TestDeniedModels_RefusesThatModelButAllowsOthers(t *testing.T) {
	g := newGateway(t, []string{"paid", "other"})
	key := g.mintKey(100, "paid", "other")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		DeniedModels: []string{"paid"},
	}))

	denied := g.post("/v1/chat/completions", key,
		`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusForbidden, denied.code, denied.body)

	allowed := g.post("/v1/chat/completions", key,
		`{"model":"other","messages":[{"role":"user","content":"hi"}]}`)
	assert.Equal(t, http.StatusOK, allowed.code, allowed.body)

	assert.Equal(t, 1, g.scripts.callCount(), "only the allowed model's request should have reached the upstream")
}

// TestDeniedProviders_RoutesAroundTheDeniedDeploymentWithoutCallingIt seeds
// two deployments of the SAME client-facing model with different provider
// names — the shape provider_allowlist's own doc comments describe (a model
// reachable through at least one allowed provider still goes through) — and
// denies one of them. Every attempt, including whatever the router's own
// retry/ordering would otherwise try, must land on the allowed deployment;
// the denied one's scripted provider must never be called. Builds its own
// registry/server rather than newGatewayOpts: that helper hard-codes one
// deployment per model name and "scripted" as every deployment's provider
// name, neither of which this scenario can use.
func TestDeniedProviders_RoutesAroundTheDeniedDeploymentWithoutCallingIt(t *testing.T) {
	factory := &scriptedFactory{scripts: map[string]*script{
		"paid-via-blocked": {complete: textAnswer("should never be seen")},
		"paid-via-allowed": {complete: textAnswer("ok")},
	}}
	modelCfgs := []config.ModelConfig{
		{Name: "paid", Provider: "blocked-provider", ProviderModel: "paid-via-blocked"},
		{Name: "paid", Provider: "allowed-provider", ProviderModel: "paid-via-allowed"},
	}
	registry, err := provider.NewRegistry(modelCfgs, factory)
	require.NoError(t, err)

	db, err := store.Open(config.DatabaseConfig{Driver: "sqlite", URL: filepath.Join(t.TempDir(), "e2e.db")})
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background()))
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Server: config.ServerConfig{MasterKey: masterKey, MaxRequestSizeMB: 8, MaxConcurrent: 64}}
	srv := server.New(cfg, logger)
	authMw := auth.NewMiddleware(masterKey, db)
	authMw.SetGatewayBilledFunc(registry.IsGatewayBilled)
	server.RegisterRoutes(srv, registry, nil, authMw, db, nil, nil, nil, nil, nil)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	g := &gateway{t: t, url: ts.URL, scripts: factory, db: db}

	key := g.mintKey(100, "paid")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		DeniedProviders: []string{"blocked-provider"},
	}))

	for i := 0; i < 8; i++ {
		resp := g.post("/v1/chat/completions", key,
			`{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
		require.Equal(t, http.StatusOK, resp.code, resp.body)
	}

	seen := factory.lastRequest()
	require.NotNil(t, seen)
	assert.Equal(t, "paid-via-allowed", seen.Model, "the allowed deployment must have served the request")
	assert.Equal(t, 8, factory.callCount(), "every attempt must land on the allowed deployment — none silently dropped")
}

// TestDeniedModels_AppliesToEveryMediaAndModerationEndpoint pins GW-02
// ("allineare il comportamento dei percorsi ... media e file"): moderations
// and every media handler (image generation, TTS, transcription) either
// re-implemented model access as an allow-list-only check that never
// consulted denied_models/denied_providers/residency at all, or — audio
// speech and transcription — had no model-access check whatsoever, going
// straight to CheckBudget. A denied model must refuse with the same
// access_denied shape completions/embeddings/responses already produce,
// not the "does not support X" 400 a missing-capability deployment would
// give (which is what these requests would get if checkModelAccess were
// removed and the scripted test provider — which implements none of
// Moderator/ImageGenerator/TextToSpeech/Transcriber — were reached
// instead: 403 access_denied here is specifically proof the gate ran
// first, not that the call failed for some other reason).
func TestDeniedModels_AppliesToEveryMediaAndModerationEndpoint(t *testing.T) {
	g := newGateway(t, []string{"paid"})
	key := g.mintKey(100, "paid")
	require.NoError(t, g.db.UpdateKey(context.Background(), auth.HashKey(key), store.UpdateKeyParams{
		DeniedModels: []string{"paid", "whisper-1"},
	}))

	cases := []struct {
		name, path, contentType, body string
	}{
		{"moderations", "/v1/moderations", "application/json", `{"model":"paid","input":"hi"}`},
		{"image generation", "/v1/images/generations", "application/json", `{"model":"paid","prompt":"hi"}`},
		{"audio speech", "/v1/audio/speech", "application/json", `{"model":"paid","input":"hi","voice":"alloy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := g.post(tc.path, key, tc.body)
			assert.Equal(t, http.StatusForbidden, resp.code, "%s: %s", tc.name, resp.body)
			assert.Contains(t, resp.body, "access_denied", "%s: %s", tc.name, resp.body)
		})
	}

	t.Run("audio transcription", func(t *testing.T) {
		contentType, body := multipartFile(t, "clip.wav", "not really audio")
		req := g.request("POST", "/v1/audio/transcriptions", key, contentType, body)
		resp := g.send(req)
		assert.Equal(t, http.StatusForbidden, resp.code, resp.body)
		assert.Contains(t, resp.body, "access_denied", resp.body)
	})
}
