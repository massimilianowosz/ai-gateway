package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// budgetGateStore authenticates one unfunded key.
type budgetGateStore struct {
	store.Store
	key *store.APIKey
}

func (b budgetGateStore) GetKeyByHash(_ context.Context, _ string) (*store.APIKey, error) {
	return b.key, nil
}

func (budgetGateStore) GetTeam(_ context.Context, _ string) (*store.Team, error) {
	return nil, nil
}

// Every route that can reach a provider must refuse a key with no budget.
//
// The funding gate used to live in authentication, which sees every request. It
// now lives in the handlers, where the model — and so whether the gateway pays
// for it — is known. That is the right place, but it is fail-open: a route
// added without the check simply does not have it, and nothing says so. Four
// endpoints were left behind exactly that way.
//
// This walks the real mux, so a new paid route that forgets the check fails
// here instead of shipping.
//
// Both halves of the gate are walked. Exhaustion used to be caught in
// authentication, which covered every route whether or not it remembered to
// ask; now it is decided per model alongside the unfunded case, and is
// fail-open in exactly the same way.
func TestEveryPaidRouteRefusesAnUnfundedKey(t *testing.T) {
	t.Run("no budget", func(t *testing.T) {
		assertEveryPaidRouteRefuses(t, &store.APIKey{
			ID: "k1", KeyHash: auth.HashKey("sk-unfunded"), KeyPrefix: "sk-un",
			Name: "unfunded", Active: true, Budget: 0,
		}, "no budget")
	})
	t.Run("budget spent", func(t *testing.T) {
		assertEveryPaidRouteRefuses(t, &store.APIKey{
			ID: "k2", KeyHash: auth.HashKey("sk-unfunded"), KeyPrefix: "sk-un",
			Name: "exhausted", Active: true, Budget: 10, Spend: 10,
		}, "spent its budget")
	})
}

func assertEveryPaidRouteRefuses(t *testing.T, key *store.APIKey, wantReason string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{Server: config.ServerConfig{
		MasterKey: "sk-master", MaxRequestSizeMB: 1, MaxConcurrent: 10,
	}}

	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name: "paid-model", Provider: "mock", ProviderModel: "paid-v1",
	}}, &routeMockFactory{})
	require.NoError(t, err)

	// Authenticated the way production does.
	db := budgetGateStore{key: key}

	srv := New(cfg, logger)
	RegisterRoutes(srv, registry, nil, auth.NewMiddleware(cfg.Server.MasterKey, db), db, nil, nil, nil, nil, nil)

	routes := []struct {
		name, path, body, contentType string
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"paid-model","messages":[{"role":"user","content":"hi"}]}`, "application/json"},
		{"completions", "/v1/completions", `{"model":"paid-model","prompt":"hi"}`, "application/json"},
		{"messages", "/v1/messages", `{"model":"paid-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`, "application/json"},
		{"responses", "/v1/responses", `{"model":"paid-model","input":"hi"}`, "application/json"},
		{"embeddings", "/v1/embeddings", `{"model":"paid-model","input":"hi"}`, "application/json"},
		{"moderations", "/v1/moderations", `{"model":"paid-model","input":"hi"}`, "application/json"},
		{"images", "/v1/images/generations", `{"model":"paid-model","prompt":"hi"}`, "application/json"},
		{"audio speech", "/v1/audio/speech", `{"model":"paid-model","input":"hi"}`, "application/json"},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, rt.path, bytes.NewReader([]byte(rt.body)))
			req.Header.Set("Content-Type", rt.contentType)
			req.Header.Set("Authorization", "Bearer sk-unfunded")
			rec := httptest.NewRecorder()
			srv.Mux().ServeHTTP(rec, req)

			// Assert the specific refusal, not merely "not 200". These handlers
			// fail for their own reasons — an unsupported model, a bad body —
			// so a weaker assertion passes whether or not the budget was ever
			// checked, which is how four routes shipped without it.
			require.Equal(t, http.StatusPaymentRequired, rec.Code,
				"%s did not refuse a key that cannot pay, on a model the gateway pays for (body: %s)",
				rt.path, rec.Body.String())
			require.Contains(t, rec.Body.String(), wantReason,
				"%s refused for some other reason than funding", rt.path)
		})
	}
}
