package livezone

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func testCfg(enabled, lossy bool) config.LiveCompressionConfig {
	c := config.LiveCompressionConfig{Enabled: enabled, AllowLossy: lossy}
	c.ApplyDefaults()
	return c
}

// run drives the middleware and returns the body the downstream handler
// actually received — the only thing that tells us what the provider sees.
func run(t *testing.T, cfg config.LiveCompressionConfig, path string, body []byte, authed bool) ([]byte, http.Header) {
	t.Helper()
	var forwarded []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
	})
	h := Middleware(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if authed {
		// KeyHash carries the test name so the shared frozen-prefix tracker
		// (a real, persistent package var) cannot let two unrelated tests that
		// happen to send the same literal team/key collide with each other.
		req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
			&store.APIKey{TeamID: "team-1", KeyHash: "key-hash-1:" + t.Name()}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return forwarded, rec.Header()
}

func shellBody(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the build"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]string{"name": "bash", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": bigLog()},
		},
	})
}

func responsesShellBody(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "run the build"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": bigLog()},
		},
	})
}

func TestMiddleware_CompressesLiveToolOutput(t *testing.T) {
	body := shellBody(t)
	got, hdr := run(t, testCfg(true, true), "/v1/chat/completions", body, true)

	if len(got) >= len(body) {
		t.Fatalf("body was not compressed: %d -> %d", len(body), len(got))
	}
	if !json.Valid(got) {
		t.Fatal("forwarded body is not valid JSON")
	}
	if !strings.Contains(string(got), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	if hdr.Get("x-livezone-bytes-saved") == "" {
		t.Error("expected a diagnostic header reporting the saving")
	}
	t.Logf("%d -> %d bytes", len(body), len(got))
}

// Codex CLI/Desktop talk to /v1/responses, not /v1/chat/completions — this is
// the wiring that regressed silently until RewriteResponses existed.
func TestMiddleware_CompressesResponsesLiveToolOutput(t *testing.T) {
	body := responsesShellBody(t)
	got, hdr := run(t, testCfg(true, true), "/v1/responses", body, true)

	if len(got) >= len(body) {
		t.Fatalf("body was not compressed: %d -> %d", len(body), len(got))
	}
	if !json.Valid(got) {
		t.Fatal("forwarded body is not valid JSON")
	}
	if !strings.Contains(string(got), "412 tests passed") {
		t.Error("the meaningful line was lost")
	}
	if hdr.Get("x-livezone-bytes-saved") == "" {
		t.Error("expected a diagnostic header reporting the saving")
	}
	t.Logf("%d -> %d bytes", len(body), len(got))
}

// Disabled must mean byte-for-byte passthrough.
func TestMiddleware_DisabledForwardsBytesUnchanged(t *testing.T) {
	body := shellBody(t)
	got, hdr := run(t, testCfg(false, true), "/v1/chat/completions", body, true)
	if !bytes.Equal(got, body) {
		t.Fatal("a disabled middleware must not touch the body")
	}
	if hdr.Get("x-livezone-blocks") != "" {
		t.Error("a disabled middleware must not emit its headers")
	}
}

// The caller's opt-out outranks the operator's setting: it is an assertion
// about the bytes they want delivered.
func TestMiddleware_OptOutHeaderWins(t *testing.T) {
	body := shellBody(t)
	var forwarded []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
	})
	h := Middleware(testCfg(true, true), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-live-compression", "false")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !bytes.Equal(forwarded, body) {
		t.Fatal("opt-out was ignored")
	}
}

// Without opt-in, only encoding overhead goes; content stays.
func TestMiddleware_LosslessOnlyKeepsAllContent(t *testing.T) {
	body := shellBody(t)
	got, _ := run(t, testCfg(true, false), "/v1/chat/completions", body, true)
	if strings.Contains(string(got), "repeated") {
		t.Error("lossy collapse ran without opt-in")
	}
	if !strings.Contains(string(got), "downloading module cache") {
		t.Error("content was dropped with lossy disabled")
	}
}

// Paths the middleware does not handle must pass straight through.
func TestMiddleware_UnhandledPathsPassThrough(t *testing.T) {
	body := shellBody(t)
	for _, p := range []string{"/v1/embeddings", "/v1/models", "/healthz"} {
		got, _ := run(t, testCfg(true, true), p, body, true)
		if !bytes.Equal(got, body) {
			t.Errorf("%s: body was modified", p)
		}
	}
}

// A canary excludes callers deterministically: the same key must always get
// the same answer, or a session would flicker between compressed and not.
func TestMiddleware_CanaryIsStablePerKey(t *testing.T) {
	cfg := testCfg(true, true)
	cfg.CanaryPercent = 50

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyHash: "stable-key"}))
	first := inCanary(req, 50)
	for i := 0; i < 20; i++ {
		if inCanary(req, 50) != first {
			t.Fatal("canary decision is not stable for a given key")
		}
	}

	// No identity: stay out rather than flip-flop.
	anon := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if inCanary(anon, 50) {
		t.Error("an unidentified caller must be excluded from a partial canary")
	}
	// 0 and 100 are unconditional.
	if !inCanary(anon, 0) || !inCanary(anon, 100) {
		t.Error("0 and 100 percent must include everyone")
	}
}

// Malformed input must never fail the request.
func TestMiddleware_MalformedBodyPassesThrough(t *testing.T) {
	for _, body := range []string{`not json`, `{"messages":`, `{}`, ``} {
		got, _ := run(t, testCfg(true, true), "/v1/chat/completions", []byte(body), true)
		if string(got) != body {
			t.Errorf("%q: body was altered to %q", body, got)
		}
	}
}

// fakeRecorder captures what the middleware reported.
type fakeRecorder struct {
	blocks map[string]int
	bytes  map[string]ByteDelta
	skips  []string
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{blocks: map[string]int{}, bytes: map[string]ByteDelta{}}
}

func (f *fakeRecorder) RecordLiveCompression(transformer string, blocks, before, after int) {
	f.blocks[transformer] += blocks
	d := f.bytes[transformer]
	d.Before += before
	d.After += after
	f.bytes[transformer] = d
}

func (f *fakeRecorder) RecordLiveCompressionSkip(reason string) {
	f.skips = append(f.skips, reason)
}

// errReader fails partway through, like a client that disconnects mid-body.
type errReader struct{ n int }

func (e *errReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, strings.Repeat("{", min(e.n, len(p))))
	e.n -= n
	return n, nil
}

// A failed body read is the one bail-out that used to leave no trace: the
// drained reader went downstream and surfaced as a JSON parse error, which says
// nothing about the cause.
func TestMiddleware_BodyReadErrorIsRecorded(t *testing.T) {
	rec := newFakeRecorder()
	var forwarded []byte
	var served bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		forwarded, _ = io.ReadAll(r.Body)
	})
	h := Middleware(testCfg(true, true), rec, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &errReader{n: 16})
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-hash-1:" + t.Name()}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !served {
		t.Fatal("the request was not forwarded")
	}
	if len(rec.skips) != 1 || rec.skips[0] != "body_read_error" {
		t.Errorf("skips = %v, want one body_read_error", rec.skips)
	}
	if len(forwarded) != 16 {
		t.Errorf("downstream read %d bytes, want the 16 that were read before the error", len(forwarded))
	}
}

// One record per transformer, carrying that transformer's own byte totals —
// not the whole-request aggregate repeated once per changed block, which
// multiplied the headline saving by the block count.
func TestMiddleware_ReportsBytesOncePerTransformer(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "run the build"}}
	const blocks = 3
	for i := 1; i <= blocks; i++ {
		id := "call_" + itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": id, "type": "function",
					"function": map[string]string{"name": "bash", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": bigLog()},
		)
	}
	body := mustJSON(t, map[string]any{"model": "gpt-4o", "messages": msgs})

	rec := newFakeRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	})
	h := Middleware(testCfg(true, true), rec, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-hash-1:" + t.Name()}))
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)

	counted, reported := 0, 0
	for _, n := range rec.blocks {
		counted += n
	}
	for _, d := range rec.bytes {
		reported += d.Before - d.After
	}
	if counted != blocks {
		t.Fatalf("counted %d compressed blocks, want %d", counted, blocks)
	}
	if reported <= 0 {
		t.Fatal("no byte saving was reported")
	}
	// Content bytes are counted before JSON escaping, so the reported figure
	// sits at or below what the request actually shed — never a multiple of it.
	actual := len(body) - len(mustCompress(t, body))
	if reported > actual {
		t.Errorf("reported %d bytes saved, but the request only shed %d — counted per block",
			reported, actual)
	}
}

func mustCompress(t *testing.T, body []byte) []byte {
	t.Helper()
	out, _, err := RewriteOpenAI(body, policyFrom(testCfg(true, true)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeMetricWriter captures the row the middleware files, and lets the test
// wait for the write that happens off the request path.
type fakeMetricWriter struct {
	written chan store.HiveStateMetric
}

func newFakeMetricWriter() *fakeMetricWriter {
	return &fakeMetricWriter{written: make(chan store.HiveStateMetric, 4)}
}

func (f *fakeMetricWriter) LogHiveStateMetric(_ context.Context, m store.HiveStateMetric) error {
	f.written <- m
	return nil
}

// The saving has to land somewhere durable and attributable. In-process
// counters cannot serve that: they reset with the pod and carry no key, so
// nothing can be tied back to the customer whose bill it reduced.
func TestMiddleware_RecordsTheSavingAgainstTheKey(t *testing.T) {
	writer := newFakeMetricWriter()
	body := shellBody(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
	})
	h := Middleware(testCfg(true, true), nil, writer, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-request-id", "req-live-1")
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyPrefix: "sk-ubq-abcd", KeyHash: "key-hash-1:" + t.Name()}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case m := <-writer.written:
		// "live" is what the savings aggregation sums alongside "state".
		if m.Mode != "live" {
			t.Errorf("mode = %q, want live", m.Mode)
		}
		if m.KeyPrefix != "sk-ubq-abcd" {
			t.Errorf("key_prefix = %q: the saving cannot be attributed", m.KeyPrefix)
		}
		if m.RequestID != "req-live-1" {
			t.Errorf("request_id = %q, want req-live-1", m.RequestID)
		}
		if m.OriginalTokens <= m.ResultTokens {
			t.Errorf("no saving recorded: original=%d result=%d", m.OriginalTokens, m.ResultTokens)
		}
		if m.Ratio <= 0 {
			t.Errorf("ratio = %v, want positive", m.Ratio)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nessuna riga scritta per una richiesta compressa")
	}
}

// A request that saved nothing must not file a row: an empty saving priced by
// the aggregation is still a row inflating the compression count.
func TestMiddleware_RecordsNothingWhenNothingWasSaved(t *testing.T) {
	writer := newFakeMetricWriter()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
	})
	h := Middleware(testCfg(true, true), nil, writer, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	// Already compact, and below the floor: nothing to remove.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"ciao"}]}`)))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyPrefix: "sk-ubq-abcd"}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case m := <-writer.written:
		t.Fatalf("riga scritta senza risparmio: original=%d result=%d", m.OriginalTokens, m.ResultTokens)
	case <-time.After(300 * time.Millisecond):
	}
}
