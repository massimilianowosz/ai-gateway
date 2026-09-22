package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- handleHealth ---

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handleHealth(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
}

// --- handleReady ---

func TestHandleReady_NoModels(t *testing.T) {
	registry, err := provider.NewRegistry(nil, &routeMockFactory{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	handleReady(registry, nil)(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "not_ready", body["status"])
}

func TestHandleReady_WithModels(t *testing.T) {
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name: "gpt-4o", Provider: "mock", ProviderModel: "gpt-4o",
	}}, &routeMockFactory{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	handleReady(registry, nil)(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "ready", body["status"])
	assert.Equal(t, float64(1), body["models"])
}

type readinessStub struct{ err error }

func (r readinessStub) GovernanceReadiness(context.Context) error { return r.err }

func TestHandleReady_RejectsReplicaWithoutGovernanceStore(t *testing.T) {
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name: "gpt-4o", Provider: "mock", ProviderModel: "gpt-4o",
	}}, &routeMockFactory{})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()

	handleReady(registry, readinessStub{err: fmt.Errorf("schema is stale")})(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), "governance store unavailable")
}

// --- writeJSON ---

func TestWriteJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusTeapot, map[string]string{"hello": "world"})

	assert.Equal(t, http.StatusTeapot, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"hello":"world"}`, rr.Body.String())
}

// --- registerSwagger ---

func TestRegisterSwagger_ServesUIAndSpec(t *testing.T) {
	mux := http.NewServeMux()
	spec := []byte("openapi: 3.0.0\ninfo:\n  title: test\n")
	registerSwagger(mux, spec)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docs", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "swagger-ui")

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docs/openapi.yaml", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/yaml", rr.Header().Get("Content-Type"))
	assert.Equal(t, "*", rr.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, spec, rr.Body.Bytes())
}

// --- Server.Run ---

func TestServer_New_MuxIsUsable(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 0}}
	s := New(cfg, slog.Default())
	require.NotNil(t, s.Mux())

	s.Mux().HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	s.Mux().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestServer_Run_GracefulShutdownOnContextCancel(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 0}}
	s := New(cfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	// Give the server a moment to start listening, then request shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Run did not return after context cancellation")
	}
}

func TestServer_Run_ListenError(t *testing.T) {
	// Occupy a real port first, on the same wildcard address form
	// Server.Run itself binds (":<port>"), so the second bind reliably
	// collides regardless of IPv4/IPv6 dual-stack quirks.
	ln, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &config.Config{Server: config.ServerConfig{Port: port}}
	s := New(cfg, slog.Default())

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(context.Background()) }()

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Run did not return a listen error in time")
	}
}
