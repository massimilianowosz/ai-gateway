package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

type routeMockFactory struct{}

func (f *routeMockFactory) Create(_ config.ModelConfig) (provider.Provider, error) {
	return &routeMockProvider{}, nil
}

type routeMockProvider struct{}

func (p *routeMockProvider) Name() string { return "mock" }

func (p *routeMockProvider) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "test-model-v1",
		Choices: []provider.Choice{{
			Index:   0,
			Message: &provider.Message{Role: "assistant", Content: "ok"},
		}},
	}, nil
}

func (p *routeMockProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, io.EOF
}

func TestChatCompletions_EnforcesPerKeyRateLimit(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{
			MasterKey:        "sk-master",
			MaxRequestSizeMB: 1,
			MaxConcurrent:    10,
		},
	}

	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "ubiquum-test.db"),
	})
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Migrate(context.Background()))

	rawKey := "sk-ubq-test-rate-limit"
	require.NoError(t, db.CreateKey(context.Background(), &store.APIKey{
		KeyHash:   auth.HashKey(rawKey),
		KeyPrefix: rawKey[:11],
		Budget:    100,
		RateLimit: 1,
		Active:    true,
	}))

	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          "test-model",
		Provider:      "mock",
		ProviderModel: "test-model-v1",
	}}, &routeMockFactory{})
	require.NoError(t, err)

	spender := spend.NewBatchWriter(db, logger, time.Hour)
	defer spender.Close()

	srv := New(cfg, logger)
	RegisterRoutes(srv, registry, nil, auth.NewMiddleware(cfg.Server.MasterKey, db), db, nil, spender, nil, nil, nil)

	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	first.Header.Set("Authorization", "Bearer "+rawKey)
	firstRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(firstRec, first)
	assert.Equal(t, http.StatusOK, firstRec.Code)

	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	second.Header.Set("Authorization", "Bearer "+rawKey)
	secondRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(secondRec, second)
	assert.Equal(t, http.StatusTooManyRequests, secondRec.Code)
	assert.Contains(t, secondRec.Body.String(), "rate limit exceeded")
}

func TestChatCompletions_ExtraMiddlewareRunsAfterScopedUpstreamForwarding(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{
			MasterKey:        "sk-master",
			MaxRequestSizeMB: 1,
			MaxConcurrent:    10,
			UpstreamTokenForwarding: config.UpstreamTokenForwardingConfig{
				Enabled:          true,
				Header:           "X-Ubiquum-Upstream-Authorization",
				AllowedProviders: []string{"github_copilot"},
			},
		},
	}

	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          "test-model",
		Provider:      "mock",
		ProviderModel: "test-model-v1",
	}}, &routeMockFactory{})
	require.NoError(t, err)

	var sawExtraMiddleware bool
	extra := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawExtraMiddleware = true
			assert.NotNil(t, auth.KeyInfoFromContext(r.Context()))
			assert.Equal(t, "tid-route", auth.UpstreamTokenForProvider(r.Context(), "github_copilot"))
			assert.Empty(t, r.Header.Get("X-Ubiquum-Upstream-Authorization"))
			next.ServeHTTP(w, r)
		})
	}

	srv := New(cfg, logger)
	RegisterRoutes(srv, registry, nil, auth.NewMiddleware(cfg.Server.MasterKey, nil), nil, nil, nil, nil, nil, nil, extra)

	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-master")
	req.Header.Set("X-Ubiquum-Upstream-Authorization", "Bearer tid-route")
	req.Header.Set("X-Ubiquum-Upstream-Provider", "github_copilot")
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)

	assert.True(t, sawExtraMiddleware)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestStatefulResponsesRoutesAreRegistered walks the stateful half of the
// Responses API end to end through the real mux, which is the only place the
// path patterns and their methods are actually exercised.
func TestStatefulResponsesRoutesAreRegistered(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{MasterKey: "sk-master", MaxRequestSizeMB: 1, MaxConcurrent: 10},
	}
	cfg.Responses.ApplyDefaults()

	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "ubiquum-routes.db"),
	})
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Migrate(context.Background()))

	rawKey := "sk-ubq-test-responses"
	require.NoError(t, db.CreateKey(context.Background(), &store.APIKey{
		KeyHash: auth.HashKey(rawKey), KeyPrefix: rawKey[:11], Budget: 100, Active: true,
	}))

	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name: "test-model", Provider: "mock", ProviderModel: "test-model-v1",
	}}, &routeMockFactory{})
	require.NoError(t, err)

	spender := spend.NewBatchWriter(db, logger, time.Hour)
	defer spender.Close()

	srv := New(cfg, logger)
	RegisterRoutes(srv, registry, nil, auth.NewMiddleware(cfg.Server.MasterKey, db), db, nil, spender, nil, nil, nil)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		rec := httptest.NewRecorder()
		srv.Mux().ServeHTTP(rec, req)
		return rec
	}

	// A conversation, its items, and a turn that joins it.
	created := call(http.MethodPost, "/v1/conversations", `{"metadata":{"topic":"routes"}}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var conversation struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &conversation))
	require.NotEmpty(t, conversation.ID)

	items := call(http.MethodPost, "/v1/conversations/"+conversation.ID+"/items",
		`{"items":[{"type":"message","role":"user","content":"ciao"}]}`)
	require.Equal(t, http.StatusOK, items.Code, items.Body.String())

	listed := call(http.MethodGet, "/v1/conversations/"+conversation.ID+"/items", "")
	require.Equal(t, http.StatusOK, listed.Code)
	assert.Contains(t, listed.Body.String(), "ciao")

	turn := call(http.MethodPost, "/v1/responses",
		`{"model":"test-model","input":"e poi?","conversation":"`+conversation.ID+`"}`)
	require.Equal(t, http.StatusOK, turn.Code, turn.Body.String())
	var response struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(turn.Body.Bytes(), &response))

	assert.Equal(t, http.StatusOK, call(http.MethodGet, "/v1/responses/"+response.ID, "").Code)
	assert.Equal(t, http.StatusOK, call(http.MethodGet, "/v1/responses/"+response.ID+"/input_items", "").Code)
	assert.Equal(t, http.StatusOK, call(http.MethodPost, "/v1/responses/"+response.ID+"/cancel", "").Code)
	assert.Equal(t, http.StatusOK, call(http.MethodDelete, "/v1/responses/"+response.ID, "").Code)
	assert.Equal(t, http.StatusOK, call(http.MethodDelete, "/v1/conversations/"+conversation.ID, "").Code)
}

func TestModelAlias_IsCanonicalBeforeExtraMiddlewareAndWhitelistCheck(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{
			MasterKey:        "sk-master",
			MaxRequestSizeMB: 1,
			MaxConcurrent:    10,
		},
	}
	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "model-alias-test.db"),
	})
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.Migrate(context.Background()))

	rawKey := "sk-ubq-model-alias"
	require.NoError(t, db.CreateKey(context.Background(), &store.APIKey{
		KeyHash:   auth.HashKey(rawKey),
		KeyPrefix: rawKey[:11],
		Budget:    100,
		Models:    []string{"gpt-4o"},
		Active:    true,
	}))

	registry, err := provider.NewRegistryWithAliases([]config.ModelConfig{{
		Name:          "azure-gpt-4o",
		Provider:      "mock",
		ProviderModel: "gpt-4o-upstream",
	}}, map[string]string{"gpt-4o": "azure-gpt-4o"}, &routeMockFactory{})
	require.NoError(t, err)

	var middlewareModel string
	extra := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, readErr := io.ReadAll(r.Body)
			require.NoError(t, readErr)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var payload struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.Unmarshal(body, &payload))
			middlewareModel = payload.Model
			next.ServeHTTP(w, r)
		})
	}

	spender := spend.NewBatchWriter(db, logger, time.Hour)
	defer spender.Close()
	srv := New(cfg, logger)
	RegisterRoutes(srv, registry, nil, auth.NewMiddleware(cfg.Server.MasterKey, db), db, nil, spender, nil, nil, nil, extra)

	for _, requestedModel := range []string{"gpt-4o", "azure-gpt-4o"} {
		body := []byte(`{"model":"` + requestedModel + `","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Mux().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "azure-gpt-4o", middlewareModel)
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer sk-master")
	modelsRec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(modelsRec, modelsReq)
	assert.Equal(t, http.StatusOK, modelsRec.Code)
	assert.NotContains(t, modelsRec.Body.String(), `"id":"azure-gpt-4o"`)
	assert.Contains(t, modelsRec.Body.String(), `"id":"gpt-4o"`)
}
