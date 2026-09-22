package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ── /v1/models ───────────────────────────────────────────────────────────────

type nopFactory struct{}

func (f *nopFactory) Create(_ config.ModelConfig) (provider.Provider, error) {
	return nil, nil // no real provider needed for list-models tests
}

func buildRegistry(t *testing.T, names ...string) *provider.Registry {
	t.Helper()
	models := make([]config.ModelConfig, len(names))
	for i, n := range names {
		models[i] = config.ModelConfig{Name: n, Provider: "openai"}
	}
	reg, err := provider.NewRegistry(models, &nopFactory{})
	require.NoError(t, err)
	return reg
}

func TestModelsHandler_ReturnsConfiguredModels(t *testing.T) {
	reg := buildRegistry(t, "gpt-4o", "claude-3-5-sonnet")
	h := NewModelsHandler(reg, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "list", resp["object"])

	data, _ := resp["data"].([]any)
	require.Len(t, data, 2)

	ids := make([]string, 0, 2)
	for _, item := range data {
		m, _ := item.(map[string]any)
		ids = append(ids, m["id"].(string))
		assert.Equal(t, "model", m["object"])
		assert.Equal(t, "ubiquum-ai-gateway", m["owned_by"])
	}
	assert.ElementsMatch(t, []string{"gpt-4o", "claude-3-5-sonnet"}, ids)
}

func TestModelsHandler_EmptyRegistry(t *testing.T) {
	reg := buildRegistry(t) // no models
	h := NewModelsHandler(reg, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	data, _ := resp["data"].([]any)
	assert.Len(t, data, 0)
}

func modelIDs(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	data, _ := resp["data"].([]any)
	ids := make([]string, 0, len(data))
	for _, item := range data {
		m, _ := item.(map[string]any)
		ids = append(ids, m["id"].(string))
	}
	return ids
}

// The tenant admin UI reads this endpoint to render its model switches. Served
// through the tenant's own whitelist it only ever returned the models already
// enabled, so a model that got disabled disappeared from the page and could
// never be switched back on.
func TestModelsHandler_MasterIncludeDisabled_ShowsWhitelistedOffModels(t *testing.T) {
	reg := buildRegistry(t, "gpt-4o", "claude-3-5-sonnet")
	db := openResponsesTestStore(t)
	ctx := context.Background()
	require.NoError(t, db.CreateTeam(ctx, &store.Team{
		ID:            "team-1",
		Name:          "Tenant",
		AllowedModels: store.StringList{"gpt-4o"},
	}))
	h := NewModelsHandler(reg, db)

	masterReq := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		return req.WithContext(auth.ContextWithKeyInfo(req.Context(),
			&store.APIKey{KeyHash: "master", Active: true}))
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, masterReq("/v1/models?team_id=team-1"))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, []string{"gpt-4o"}, modelIDs(t, rr), "the tenant view stays filtered")

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, masterReq("/v1/models?team_id=team-1&include_disabled=true"))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.ElementsMatch(t, []string{"gpt-4o", "claude-3-5-sonnet"}, modelIDs(t, rr),
		"the admin view has to show what the whitelist turned off")
}

func TestModelAliasesHandler_MasterOnly(t *testing.T) {
	reg, err := provider.NewRegistryWithAliases(
		[]config.ModelConfig{{Name: "azure-gpt-4o", Provider: "openai"}},
		map[string]string{"gpt-4o": "azure-gpt-4o"},
		&nopFactory{},
	)
	require.NoError(t, err)
	h := NewModelAliasesHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/model/aliases", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyHash: "master", Active: true})))
	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, map[string]string{"gpt-4o": "azure-gpt-4o"}, resp["aliases"])

	// A tenant key must not read the operator's model topology.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyHash: "tenant-key", Active: true})))
	assert.Equal(t, http.StatusForbidden, rr.Code)
}
