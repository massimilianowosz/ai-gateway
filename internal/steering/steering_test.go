package steering

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func testCfg() config.SteeringConfig {
	c := config.SteeringConfig{Enabled: true}
	c.ApplyDefaults()
	return c
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run drives the middleware and returns the body the downstream handler
// actually received — the only thing that says what the provider will see.
func run(t *testing.T, cfg config.SteeringConfig, path string, body []byte) ([]byte, http.Header) {
	t.Helper()
	var forwarded []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
	})
	h := Middleware(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return forwarded, rec.Header()
}

// --- verbosity ---

// The note must land at the very end of the system prompt: everything ahead of
// it stays byte-identical, which is what keeps a provider prefix cache warm.
func TestVerbosityNoteGoesAtTheEndOfInstructions(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":        "codex-gpt-5.5",
		"instructions": "You are a coding agent.",
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]string{"type": "input_text", "text": "hi"}}}},
	})

	got, hdr := run(t, testCfg(), "/v1/responses", body)

	var env struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Instructions, "You are a coding agent.") {
		t.Errorf("the caller's own instructions were disturbed: %q", env.Instructions)
	}
	if !strings.Contains(env.Instructions, "be concise") {
		t.Errorf("the note never arrived: %q", env.Instructions)
	}
	if hdr.Get("x-steering-verbosity") != "applied" {
		t.Error("expected a diagnostic header reporting the change")
	}
}

func TestVerbosityNoteAppendsToAnthropicSystemBlocks(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"system": []any{
			map[string]any{"type": "text", "text": "You are a coding agent.",
				"cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	got, _ := run(t, testCfg(), "/v1/messages", body)

	var env struct {
		System []struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.System) != 2 {
		t.Fatalf("expected the note as a second block, got %d", len(env.System))
	}
	if len(env.System[0].CacheControl) == 0 {
		t.Error("the caller's cache breakpoint was dropped")
	}
	if !strings.Contains(env.System[1].Text, "be concise") {
		t.Errorf("the note never arrived: %q", env.System[1].Text)
	}
}

// Appending twice would move the prompt on every turn, which costs a cache
// miss each time — exactly what this lever exists to avoid.
func TestVerbosityNoteIsNotAppendedTwice(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":        "codex-gpt-5.5",
		"instructions": "You are a coding agent.",
		"input":        []any{},
	})
	once, _ := run(t, testCfg(), "/v1/responses", body)
	twice, _ := run(t, testCfg(), "/v1/responses", once)

	if len(twice) != 0 && string(twice) != string(once) {
		t.Errorf("a second pass changed the body again:\n%s", twice)
	}
}

// Without a system message there is nothing to append to: inventing one would
// change the first token of the prompt on every request.
func TestVerbosityLeavesOpenAIAloneWithoutASystemMessage(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	got, _ := run(t, testCfg(), "/v1/chat/completions", body)
	if len(got) != len(body) {
		t.Errorf("body was rewritten with no system message present:\n%s", got)
	}
}

// --- gates ---

func TestDisabledForwardsBytesUnchanged(t *testing.T) {
	cfg := testCfg()
	cfg.Enabled = false
	body := mustJSON(t, map[string]any{"model": "codex-gpt-5.5", "instructions": "x", "input": []any{}})

	got, _ := run(t, cfg, "/v1/responses", body)
	if string(got) != string(body) {
		t.Error("body was rewritten with steering off")
	}
}

func TestOptOutHeaderWins(t *testing.T) {
	body := mustJSON(t, map[string]any{"model": "codex-gpt-5.5", "instructions": "x", "input": []any{}})

	var forwarded []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
	})
	h := Middleware(testCfg(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-steering", "false")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if string(forwarded) != string(body) {
		t.Error("an explicit opt-out was overridden")
	}
}

func TestUnhandledPathsPassThrough(t *testing.T) {
	body := mustJSON(t, map[string]any{"model": "x", "instructions": "y"})
	got, _ := run(t, testCfg(), "/v1/embeddings", body)
	if string(got) != string(body) {
		t.Error("a path this package does not model was rewritten")
	}
}

func TestMalformedBodyPassesThrough(t *testing.T) {
	body := []byte(`not json at all`)
	got, _ := run(t, testCfg(), "/v1/responses", body)
	if string(got) != string(body) {
		t.Error("an unparseable body was not forwarded verbatim")
	}
}
