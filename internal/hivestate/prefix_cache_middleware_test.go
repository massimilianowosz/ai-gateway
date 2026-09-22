package hivestate

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// newTestStore returns an in-memory store for middleware wiring.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"})
	if err != nil {
		t.Skipf("sqlite store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runMiddleware sends body through the HiveState middleware and returns the
// bytes the downstream handler actually received, plus the response headers.
// Asserting on the forwarded body — not on Process() output — is the only way
// to know what the provider would really see.
func runMiddleware(t *testing.T, cfg config.HiveStateConfig, path string, body []byte) ([]byte, http.Header) {
	t.Helper()
	cfg.ApplyDefaults()

	hs, err := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var forwarded []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	h := Middleware(hs, newTestStore(t), nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return forwarded, rec.Header()
}

// The core guarantee: a long Anthropic conversation carrying cache_control
// must reach the provider byte-for-byte unchanged, so the cached prefix
// survives.
func TestMiddleware_AnthropicWithCacheControlForwardsBytesUnchanged(t *testing.T) {
	body := buildAnthropicBody(t, 20, true)

	forwarded, hdr := runMiddleware(t, config.HiveStateConfig{
		Enabled:    true,
		Threshold:  100,
		StepWindow: 4,
	}, "/v1/messages", body)

	if !bytes.Equal(forwarded, body) {
		t.Fatalf("body was modified despite an active prefix cache:\nsent %d bytes, forwarded %d bytes",
			len(body), len(forwarded))
	}
	if got := hdr.Get("x-hivestate-prefix-cache"); got != "prefix_cache_cheaper" {
		t.Fatalf("expected the guard to fire, got x-hivestate-prefix-cache=%q", got)
	}
	if got := hdr.Get("x-hivestate-fallback"); got != "prefix_cache_cheaper" {
		t.Fatalf("expected fallback reason to name the guard, got %q", got)
	}
	if hdr.Get("x-hivestate-cached-prefix-tokens") == "" {
		t.Fatal("expected the guard to report how many tokens it protected")
	}
}

// Without cache markers there is no Anthropic prefix to protect, so the guard
// must stand aside and let HiveState make its own decision.
func TestMiddleware_AnthropicWithoutMarkersDoesNotTriggerGuard(t *testing.T) {
	body := buildAnthropicBody(t, 20, false)

	_, hdr := runMiddleware(t, config.HiveStateConfig{
		Enabled:    true,
		Threshold:  100,
		StepWindow: 4,
	}, "/v1/messages", body)

	if got := hdr.Get("x-hivestate-prefix-cache"); got != "no_cached_prefix" {
		t.Fatalf("expected no protected prefix, got %q", got)
	}
}

// A short conversation sits entirely inside the preserved window: the rewriter
// would be a no-op, so the middleware must skip it without paying for a state
// extraction.
func TestMiddleware_NoopRewriteSkipsExtraction(t *testing.T) {
	body := buildAnthropicBody(t, 2, false)

	forwarded, hdr := runMiddleware(t, config.HiveStateConfig{
		Enabled:    true,
		Threshold:  100,
		StepWindow: 4,
	}, "/v1/messages", body)

	if !bytes.Equal(forwarded, body) {
		t.Fatal("a no-op rewrite must forward the original bytes")
	}
	if got := hdr.Get("x-hivestate-fallback"); got != "rewrite_would_be_noop" {
		t.Fatalf("expected rewrite_would_be_noop, got %q", got)
	}
}

// The guard is a safety net, not a cage: disabling it restores the previous
// behaviour exactly.
func TestMiddleware_GuardDisabledRestoresLegacyBehaviour(t *testing.T) {
	disabled := false
	body := buildAnthropicBody(t, 20, true)

	_, hdr := runMiddleware(t, config.HiveStateConfig{
		Enabled:          true,
		Threshold:        100,
		StepWindow:       4,
		PrefixCacheGuard: config.PrefixCacheGuardConfig{Enabled: &disabled},
	}, "/v1/messages", body)

	if hdr.Get("x-hivestate-prefix-cache") != "" {
		t.Fatal("a disabled guard must not emit its header")
	}
}

// OpenAI's shallower cache discount must let a rewrite through where Anthropic
// would block it — the guard is provider-aware, not a blanket off switch.
func TestMiddleware_OpenAIShallowDiscountAllowsRewrite(t *testing.T) {
	body := buildOpenAIBody(t, 20)

	_, hdr := runMiddleware(t, config.HiveStateConfig{
		Enabled:      true,
		Threshold:    100,
		StepWindow:   4,
		RecentWindow: 4,
	}, "/v1/chat/completions", body)

	if got := hdr.Get("x-hivestate-prefix-cache"); got == "prefix_cache_cheaper" {
		t.Fatalf("OpenAI's 0.5x discount should not block this rewrite (got %q)", got)
	}
}
