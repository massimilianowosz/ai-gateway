package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewClient / basic accessors ---

func TestNewClient_DefaultsBaseURL(t *testing.T) {
	c := NewClient("", "sk-ubq-x")
	assert.Equal(t, defaultBaseURL, c.BaseURL())
	assert.True(t, c.HasKey())
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://example.com/", "")
	assert.Equal(t, "http://example.com", c.BaseURL())
	assert.False(t, c.HasKey())
}

// --- parseAPIError ---

func TestParseAPIError_StringError(t *testing.T) {
	err := parseAPIError(400, []byte(`{"error":"bad request"}`))
	assert.Equal(t, "request failed with HTTP 400: bad request", err.Error())
}

func TestParseAPIError_ObjectErrorWithMessage(t *testing.T) {
	err := parseAPIError(404, []byte(`{"error":{"message":"model not found","type":"model_not_found"}}`))
	assert.Equal(t, "request failed with HTTP 404: model not found", err.Error())
}

func TestParseAPIError_FallsBackToRawBody(t *testing.T) {
	err := parseAPIError(500, []byte("internal server error"))
	assert.Equal(t, "request failed with HTTP 500: internal server error", err.Error())
}

func TestParseAPIError_EmptyMessage(t *testing.T) {
	err := &apiError{Status: 503}
	assert.Equal(t, "request failed with HTTP 503", err.Error())
}

// --- test server helper ---

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewClient(ts.URL, "sk-ubq-test"), ts
}

// --- Health / Ready ---

func TestClient_Health(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/health", r.URL.Path)
		assert.Equal(t, "Bearer sk-ubq-test", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(StatusResponse{Status: "ok"})
	})
	resp, err := c.Health(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Status)
}

func TestClient_Ready_NotReady(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/ready", r.URL.Path)
		_ = json.NewEncoder(w).Encode(StatusResponse{Status: "not_ready", Reason: "no models"})
	})
	resp, err := c.Ready(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "not_ready", resp.Status)
	assert.Equal(t, "no models", resp.Reason)
}

func TestClient_Health_HTTPError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	_, err := c.Health(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestClient_Health_ConnectionRefused(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "")
	_, err := c.Health(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect to")
}

// --- Models ---

func TestClient_Models(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []Model{{ID: "gpt-4o", Object: "model", OwnedBy: "openai"}},
		})
	})
	models, err := c.Models(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "gpt-4o", models[0].ID)
}

// --- Keys ---

func TestClient_CreateKey(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/key/generate", r.URL.Path)
		var body CreateKeyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "my-key", body.Name)
		_ = json.NewEncoder(w).Encode(APIKey{ID: "key-1", Name: body.Name, KeyPrefix: "sk-ab"})
	})
	key, err := c.CreateKey(context.Background(), CreateKeyRequest{Name: "my-key"})
	require.NoError(t, err)
	assert.Equal(t, "key-1", key.ID)
	assert.Equal(t, "sk-ab", key.KeyPrefix)
}

func TestClient_ListKeys_WithTeamFilter(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/key/list", r.URL.Path)
		assert.Equal(t, "team-1", r.URL.Query().Get("team_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []APIKey{{ID: "k1"}, {ID: "k2"}},
		})
	})
	keys, err := c.ListKeys(context.Background(), "team-1")
	require.NoError(t, err)
	assert.Len(t, keys, 2)
}

func TestClient_ListKeys_NoTeamFilter(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.Query().Get("team_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []APIKey{}})
	})
	_, err := c.ListKeys(context.Background(), "")
	require.NoError(t, err)
}

func TestClient_KeyInfo(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/key/info", r.URL.Path)
		assert.Equal(t, "sk-ubq-raw", r.URL.Query().Get("key"))
		_ = json.NewEncoder(w).Encode(APIKey{Name: "found"})
	})
	key, err := c.KeyInfo(context.Background(), "sk-ubq-raw")
	require.NoError(t, err)
	assert.Equal(t, "found", key.Name)
}

func TestClient_DeleteKey(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/key/delete", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "sk-ubq-raw", body["key"])
		w.WriteHeader(http.StatusOK)
	})
	err := c.DeleteKey(context.Background(), "sk-ubq-raw")
	require.NoError(t, err)
}

func TestClient_UpdateKey(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/key/update", r.URL.Path)
		var body UpdateKeyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "sk-ubq-raw", body.Key)
		require.NotNil(t, body.Params.Name)
		assert.Equal(t, "renamed", *body.Params.Name)
		w.WriteHeader(http.StatusOK)
	})
	name := "renamed"
	err := c.UpdateKey(context.Background(), "sk-ubq-raw", UpdateKeyParams{Name: &name})
	require.NoError(t, err)
}

// --- Teams ---

func TestClient_CreateTeam(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/team/create", r.URL.Path)
		_ = json.NewEncoder(w).Encode(TeamResponse{ID: "team-1", Name: "eng"})
	})
	team, err := c.CreateTeam(context.Background(), CreateTeamRequest{Name: "eng"})
	require.NoError(t, err)
	assert.Equal(t, "team-1", team.ID)
}

func TestClient_ListTeams(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/team/list", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teams": []TeamInfo{{ID: "t1", Name: "eng"}},
		})
	})
	teams, err := c.ListTeams(context.Background())
	require.NoError(t, err)
	require.Len(t, teams, 1)
	assert.Equal(t, "eng", teams[0].Name)
}

// --- Spend ---

func TestClient_SpendLogs_WithFilters(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/spend/logs", r.URL.Path)
		assert.Equal(t, "sk-ubq-raw", r.URL.Query().Get("key"))
		assert.Equal(t, "gpt-4o", r.URL.Query().Get("model"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"spend_logs": []SpendSummary{{Model: "gpt-4o", Requests: 5, Cost: 0.5}},
		})
	})
	logs, err := c.SpendLogs(context.Background(), "sk-ubq-raw", "gpt-4o")
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, 5, logs[0].Requests)
}

func TestClient_SpendLogs_NoFilters(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.Query().Get("key"))
		assert.Empty(t, r.URL.Query().Get("model"))
		_ = json.NewEncoder(w).Encode(map[string]any{"spend_logs": []SpendSummary{}})
	})
	_, err := c.SpendLogs(context.Background(), "", "")
	require.NoError(t, err)
}

// --- Chat ---

func TestClient_Chat_Success(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "gpt-4o", body["model"])

		w.Header().Set("X-Ubiquum-Provider", "azure")
		w.Header().Set("X-Ubiquum-Attempts", "2")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-1",
			"model": "gpt-4o",
			"choices": []map[string]any{
				{"message": map[string]any{"content": "hi there"}},
			},
		})
	})
	resp, err := c.Chat(context.Background(), "gpt-4o", "hello")
	require.NoError(t, err)
	assert.Equal(t, "chatcmpl-1", resp.ID)
	assert.Equal(t, "azure", resp.Provider)
	assert.Equal(t, "2", resp.Attempts)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "hi there", resp.Choices[0].Message.Content)
}

func TestClient_Chat_UpstreamError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	})
	_, err := c.Chat(context.Background(), "gpt-4o", "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream down")
}

func TestClient_Chat_ConnectionError(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "")
	_, err := c.Chat(context.Background(), "gpt-4o", "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect to")
}

// --- Cache admin ---

func TestClient_CacheFlush(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/cache/flush", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, c.CacheFlush(context.Background()))
}

func TestClient_CacheMetrics_WithLimit(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "10", r.URL.Query().Get("limit"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []CacheMetricResponse{{Band: "DIRECT"}},
		})
	})
	metrics, err := c.CacheMetrics(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, "DIRECT", metrics[0].Band)
}

func TestClient_CacheMetrics_NoLimit(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.Query().Get("limit"))
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": []CacheMetricResponse{}})
	})
	_, err := c.CacheMetrics(context.Background(), 0)
	require.NoError(t, err)
}

func TestClient_CacheStats(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/cache/stats", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": 42})
	})
	entries, err := c.CacheStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 42, entries)
}

func TestClient_StateMetrics(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "5", r.URL.Query().Get("limit"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metrics": []StateMetricResponse{{Mode: "state", RouteLevel: "trivial"}},
		})
	})
	metrics, err := c.StateMetrics(context.Background(), 5)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, "trivial", metrics[0].RouteLevel)
}

// --- Route settings ---

func TestClient_GetRouteSettings(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/route/settings", r.URL.Path)
		assert.Equal(t, "team-1", r.URL.Query().Get("team_id"))
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-1"})
	})
	resp, err := c.GetRouteSettings(context.Background(), "team-1")
	require.NoError(t, err)
	assert.Equal(t, "team-1", resp.TeamID)
}

func TestClient_UpdateRouteSettings(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/route/settings", r.URL.Path)
		_ = json.NewEncoder(w).Encode(RouteSettingsResponse{TeamID: "team-2"})
	})
	resp, err := c.UpdateRouteSettings(context.Background(), map[string]any{"team_id": "team-2"})
	require.NoError(t, err)
	assert.Equal(t, "team-2", resp.TeamID)
}

// --- GetRaw ---

func TestClient_GetRaw_Success(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/logs", r.URL.Path)
		assert.Equal(t, "Bearer sk-ubq-test", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("raw log line\n"))
	})
	data, err := c.GetRaw(context.Background(), "/v1/logs")
	require.NoError(t, err)
	assert.Equal(t, "raw log line\n", string(data))
}

func TestClient_GetRaw_Error(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	})
	_, err := c.GetRaw(context.Background(), "/v1/logs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

func TestClient_GetRaw_ConnectionError(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "")
	_, err := c.GetRaw(context.Background(), "/v1/logs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect to")
}

// --- do(): no-payload/no-out edge cases and encode error path ---

func TestClient_Do_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	c.apiKey = ""
	require.NoError(t, c.CacheFlush(context.Background()))
}

func TestClient_Do_EmptyBodyWithOutIgnored(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // no body written
	})
	_, err := c.Health(context.Background())
	require.NoError(t, err)
}

func TestClient_Do_InvalidJSONResponse(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	_, err := c.Health(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode response")
}

func TestClient_ReloadKey_NoConfigLeavesKeyUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UBIQUUM_HOME", dir)
	c := NewClient("http://example.com", "original-key")
	c.ReloadKey()
	assert.Equal(t, "original-key", c.apiKey)
}

// sanity: ensure query-string building doesn't leak into unrelated requests.
func TestClient_QueryEncoding(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasPrefix(r.URL.RawQuery, "key=") || r.URL.RawQuery == "")
		_ = json.NewEncoder(w).Encode(APIKey{})
	})
	_, err := c.KeyInfo(context.Background(), "sk-ubq-raw with space")
	require.NoError(t, err)
}
