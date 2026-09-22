package middleware

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func hooksLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func okHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if called != nil {
			*called = true
		}
		w.WriteHeader(http.StatusOK)
	})
}

// --- PreRequest ---

func TestHooks_PreRequest_Nil_PassesThrough(t *testing.T) {
	h := NewHooks(nil, nil, hooksLogger())
	called := false
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(&called)).ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHooks_PreRequest_HookAllows_PassesThrough(t *testing.T) {
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{URL: hookServer.URL}, nil, hooksLogger())
	called := false
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(&called)).ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHooks_PreRequest_HookRejects_4xx_Returns403(t *testing.T) {
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 401 → hook says no
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{URL: hookServer.URL}, nil, hooksLogger())
	called := false
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(&called)).ServeHTTP(rec, req)

	assert.False(t, called, "handler must NOT be called when hook rejects")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "hook_rejected")
}

func TestHooks_PreRequest_HookRejects_5xx_Returns403(t *testing.T) {
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{URL: hookServer.URL}, nil, hooksLogger())
	called := false
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(&called)).ServeHTTP(rec, req)

	assert.False(t, called)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHooks_PreRequest_HookUnreachable_FailOpen(t *testing.T) {
	// Port 1 is unreachable
	h := NewHooks(&config.HookEndpoint{
		URL:     "http://127.0.0.1:1",
		Timeout: 100 * time.Millisecond,
	}, nil, hooksLogger())

	called := false
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(&called)).ServeHTTP(rec, req)

	// Fail-open: request passes through even when hook is unreachable
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHooks_PreRequest_SendsPayload(t *testing.T) {
	var payload map[string]any

	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{URL: hookServer.URL}, nil, hooksLogger())
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Custom", "value")
	rec := httptest.NewRecorder()

	h.PreRequest(okHandler(nil)).ServeHTTP(rec, req)

	require.NotNil(t, payload)
	assert.Equal(t, "POST", payload["method"])
	assert.Equal(t, "/v1/chat/completions", payload["path"])
	assert.NotNil(t, payload["headers"])
	assert.NotNil(t, payload["remote_addr"])
}

func TestHooks_PreRequest_ForwardsCustomHeaders(t *testing.T) {
	var gotHeader string

	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Auth-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{
		URL:     hookServer.URL,
		Headers: map[string]string{"X-Auth-Token": "mysecret"},
	}, nil, hooksLogger())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.PreRequest(okHandler(nil)).ServeHTTP(rec, req)

	assert.Equal(t, "mysecret", gotHeader)
}

func TestHooks_PreRequest_DefaultTimeout_Used(t *testing.T) {
	// Omitting Timeout should default to 3s without panicking
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(&config.HookEndpoint{URL: hookServer.URL, Timeout: 0}, nil, hooksLogger())
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		h.PreRequest(okHandler(nil)).ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- PostRequest ---

func TestHooks_PostRequest_Nil_PassesThrough(t *testing.T) {
	h := NewHooks(nil, nil, hooksLogger())
	called := false
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()

	h.PostRequest(okHandler(&called)).ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHooks_PostRequest_FiresAsync_ResponseNotDelayed(t *testing.T) {
	// Hook is slow — response must not be blocked
	started := make(chan struct{})
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(nil, &config.HookEndpoint{URL: hookServer.URL}, hooksLogger())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	begin := time.Now()
	h.PostRequest(okHandler(nil)).ServeHTTP(rec, req)
	elapsed := time.Since(begin)

	// Response must come back well before the hook finishes
	assert.Less(t, elapsed, 150*time.Millisecond)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Wait for hook goroutine to be triggered
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("post hook was never called")
	}
}

func TestHooks_PostRequest_IncludesStatusCode(t *testing.T) {
	var mu sync.Mutex
	var payload map[string]any
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		mu.Lock()
		payload = p
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(nil, &config.HookEndpoint{URL: hookServer.URL}, hooksLogger())

	// Handler returns 201
	req := httptest.NewRequest("POST", "/v1/keys", nil)
	rec := httptest.NewRecorder()
	h.PostRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	// Poll briefly for async hook delivery
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return payload != nil
	}, 500*time.Millisecond, 10*time.Millisecond)

	mu.Lock()
	assert.Equal(t, float64(http.StatusCreated), payload["status"])
	assert.Equal(t, "POST", payload["method"])
	assert.Equal(t, "/v1/keys", payload["path"])
	mu.Unlock()
}

func TestHooks_PostRequest_HookError_DoesNotAffectResponse(t *testing.T) {
	// Hook server returns 500 — the client response must be unaffected
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hookServer.Close()

	h := NewHooks(nil, &config.HookEndpoint{URL: hookServer.URL}, hooksLogger())

	rec := httptest.NewRecorder()
	h.PostRequest(okHandler(nil)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	// Client gets its normal 200 regardless of hook failure
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHooks_PostRequest_ForwardsCustomHeaders(t *testing.T) {
	var gotHeader string
	var called int32

	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hook-Key")
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()

	h := NewHooks(nil, &config.HookEndpoint{
		URL:     hookServer.URL,
		Headers: map[string]string{"X-Hook-Key": "hook-value"},
	}, hooksLogger())

	rec := httptest.NewRecorder()
	h.PostRequest(okHandler(nil)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&called) > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	assert.Equal(t, "hook-value", gotHeader)
}

// --- flattenHeaders ---

func TestFlattenHeaders(t *testing.T) {
	h := http.Header{
		"Content-Type":                     []string{"application/json"},
		"Authorization":                    []string{"Bearer sk-test"},
		"X-Ubiquum-Upstream-Authorization": []string{"Bearer tid-test"},
		"X-GitHub-Token":                   []string{"ghu_test"},
		"X-Provider-Secret":                []string{"secret-test"},
		"X-Provider-ApiKey":                []string{"apikey-test"},
		"Multi-Value":                      []string{"first", "second"},
	}
	flat := flattenHeaders(h)

	assert.Equal(t, "application/json", flat["Content-Type"])
	assert.Equal(t, redactedHeaderValue, flat["Authorization"])
	assert.Equal(t, redactedHeaderValue, flat["X-Ubiquum-Upstream-Authorization"])
	assert.Equal(t, redactedHeaderValue, flat["X-GitHub-Token"])
	assert.Equal(t, redactedHeaderValue, flat["X-Provider-Secret"])
	assert.Equal(t, redactedHeaderValue, flat["X-Provider-ApiKey"])
	// Only first value is taken
	assert.Equal(t, "first", flat["Multi-Value"])
}

func TestFlattenHeaders_Empty(t *testing.T) {
	flat := flattenHeaders(http.Header{})
	assert.Empty(t, flat)
}
