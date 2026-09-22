package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func testCanonicalModel(model string) string {
	if model == "gpt-4o" {
		return "azure-gpt-4o"
	}
	return model
}

// The public alias points at a metered deployment, which is the right answer
// for everyone except a caller paying with their own subscription: for them
// the same name means the pass-through model, and their token says so.
func TestModelAliases_ASubscribersNameBeatsThePublicAlias(t *testing.T) {
	resolve := func(ctx context.Context, model string) string {
		if auth.ScopedUpstreamProvider(ctx) == "anthropic" && model == "claude-opus-5" {
			return "claude-code-opus-5"
		}
		return testCanonicalModel(model)
	}

	var seen string
	handler := ModelAliases(testCanonicalModel, resolve)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		seen, _ = payload["model"].(string)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.ContextWithScopedUpstreamToken(req.Context(), "anthropic", "oauth-token"))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "claude-code-opus-5", seen)
}

func TestModelAliases_WithoutASubscriptionThePublicAliasStands(t *testing.T) {
	resolve := func(ctx context.Context, model string) string {
		if auth.ScopedUpstreamProvider(ctx) == "anthropic" && model == "claude-opus-5" {
			return "claude-code-opus-5"
		}
		if model == "claude-opus-5" {
			return "azure-claude-opus-5"
		}
		return testCanonicalModel(model)
	}

	var seen string
	handler := ModelAliases(testCanonicalModel, resolve)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		seen, _ = payload["model"].(string)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "azure-claude-opus-5", seen)
}

func TestModelAliases_RewritesJSONBeforeDownstream(t *testing.T) {
	handler := ModelAliases(testCanonicalModel, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		assert.Equal(t, "azure-gpt-4o", payload["model"])
		assert.Equal(t, "hello", payload["prompt"])
		assert.Equal(t, "gpt-4o", RequestedModelFromContext(r.Context()))
		assert.Equal(t, int64(len(body)), r.ContentLength)
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","prompt":"hello"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestModelAliases_LeavesUnknownAndInvalidJSONUntouched(t *testing.T) {
	tests := []string{
		`{"model":"unknown","prompt":"hello"}`,
		`{"model":`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			handler := ModelAliases(testCanonicalModel, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				assert.Equal(t, body, string(got))
				assert.Empty(t, RequestedModelFromContext(r.Context()))
			}))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}

func TestModelAliases_RewritesFileHeaderAndCanonicalizesKeyModels(t *testing.T) {
	handler := ModelAliases(testCanonicalModel, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "azure-gpt-4o", r.Header.Get("X-Ubiquum-Model"))
		assert.Equal(t, store.StringList{"azure-gpt-4o"}, auth.KeyInfoFromContext(r.Context()).Models)
		assert.Equal(t, "gpt-4o", RequestedModelFromContext(r.Context()))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/files", nil)
	req.Header.Set("X-Ubiquum-Model", "gpt-4o")
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{Models: []string{"gpt-4o"}}))
	handler.ServeHTTP(httptest.NewRecorder(), req)
}
