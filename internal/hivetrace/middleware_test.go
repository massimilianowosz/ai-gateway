package hivetrace

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func testConfig() config.HiveTraceConfig {
	cfg := config.HiveTraceConfig{
		Enabled:       true,
		CaptureBodies: true,
		FlushInterval: 20 * time.Millisecond,
	}
	cfg.ApplyDefaults()
	return cfg
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serve runs one request through the middleware and waits for the recorder to
// have persisted it, so assertions read the fully processed event.
func serve(t *testing.T, ts TrafficStore, db store.Store, req *http.Request, handler http.HandlerFunc) []Event {
	t.Helper()

	rec := NewRecorder(ts, db, testConfig(), nil, nil, discardLogger())
	t.Cleanup(func() { _ = rec.Close() })

	w := httptest.NewRecorder()
	Middleware(rec)(handler).ServeHTTP(w, req)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, err := ts.ListEvents(context.Background(), Filter{})
		require.NoError(t, err)
		if len(events) > 0 {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recorder did not persist the captured turn")
	return nil
}

func chatRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("User-Agent", "claude-code/2.1.0")
	return req
}

func TestMiddleware_CapturesATurnEndToEnd(t *testing.T) {
	db, ts := openTestStore(t)

	body := `{"model":"claude-sonnet","messages":[
		{"role":"system","content":"you are helpful"},
		{"role":"user","content":"read /src/a.go, my token is ghp_abcdefghijklmnopqrstuvwxyz0123456789"}
	]}`

	events := serve(t, ts, db, chatRequest(body), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ubiquum-Provider", "anthropic")
		w.Header().Set("X-Ubiquum-Cost", "0.0042")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sure","tool_calls":[
			{"id":"c1","function":{"name":"Read","arguments":"{\"file_path\":\"/src/a.go\"}"}}
		]}}]}`))
	})

	require.Len(t, events, 1)
	e := events[0]

	assert.Equal(t, APIChatCompletions, e.API)
	assert.Equal(t, "claude-sonnet", e.Model)
	assert.Equal(t, "anthropic", e.Provider)
	assert.Equal(t, "claude-code", e.ClientProduct)
	assert.Equal(t, "2.1.0", e.ClientVersion)
	assert.Equal(t, http.StatusOK, e.Status)
	assert.NotEmpty(t, e.SessionID, "a session id is derived when the client sends no header")
	assert.InDelta(t, 0.0042, e.Cost, 1e-9)

	require.Len(t, e.Tools, 1)
	assert.Equal(t, "Read", e.Tools[0].Tool)
	require.Len(t, e.Files, 1)
	assert.Equal(t, "/src/a.go", e.Files[0].Path)

	f, ok := findingFor(e.Findings, KindSecret, "GITHUB_TOKEN")
	require.True(t, ok, "a token pasted into the prompt must be flagged, got %+v", e.Findings)
	assert.Equal(t, OriginRequest, f.Origin)
}

// Capture is passthrough: the client must receive exactly what the handler
// wrote, byte for byte, with the same number of flushes.
func TestMiddleware_StreamingResponseIsUnchanged(t *testing.T) {
	db, ts := openTestStore(t)

	chunks := []string{
		`data: {"choices":[{"delta":{"content":"hel"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	rec := NewRecorder(ts, db, testConfig(), nil, nil, discardLogger())
	defer rec.Close()

	w := newFlushRecorder()
	req := chatRequest(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	Middleware(rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})).ServeHTTP(w, req)

	assert.Equal(t, strings.Join(chunks, ""), w.Body.String())
	assert.Equal(t, len(chunks), w.flushes)
}

func TestMiddleware_ReassemblesStreamedAnswerAndFlagsSecretsInIt(t *testing.T) {
	db, ts := openTestStore(t)

	events := serve(t, ts, db,
		chatRequest(`{"model":"m","stream":true,"messages":[{"role":"user","content":"give me a key"}]}`),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// Split across frames so a per-chunk scan would miss it.
			for _, c := range []string{
				`data: {"choices":[{"delta":{"content":"use AKIAIOSF"}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"ODNN7EXAMPLE"}}]}` + "\n\n",
				"data: [DONE]\n\n",
			} {
				_, _ = w.Write([]byte(c))
			}
		})

	require.Len(t, events, 1)
	assert.True(t, events[0].Streaming)
	assert.Equal(t, "use [AWS_ACCESS_KEY]", events[0].ResponseBody,
		"the answer is reassembled from its frames, then redacted before storage")

	f, ok := findingFor(events[0].Findings, KindSecret, "AWS_ACCESS_KEY")
	require.True(t, ok, "a key split across two SSE frames must still be detected")
	assert.Equal(t, OriginResponse, f.Origin)
}

func TestMiddleware_SkipsNonModelRoutes(t *testing.T) {
	db, ts := openTestStore(t)

	rec := NewRecorder(ts, db, testConfig(), nil, nil, discardLogger())
	defer rec.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	Middleware(rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})).ServeHTTP(w, req)

	time.Sleep(80 * time.Millisecond)
	events, err := ts.ListEvents(context.Background(), Filter{})
	require.NoError(t, err)
	assert.Empty(t, events, "listing models is not a conversation to account for")
}

func TestMiddleware_ExplicitSessionHeaderGroupsTurns(t *testing.T) {
	db, ts := openTestStore(t)

	rec := NewRecorder(ts, db, testConfig(), nil, nil, discardLogger())
	defer rec.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})

	for _, prompt := range []string{"first", "second"} {
		req := chatRequest(`{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}`)
		req.Header.Set("x-ubiquum-session", "session-xyz")
		Middleware(rec)(handler).ServeHTTP(httptest.NewRecorder(), req)
	}

	deadline := time.Now().Add(3 * time.Second)
	var events []Event
	for time.Now().Before(deadline) {
		var err error
		events, err = ts.ListEvents(context.Background(), Filter{SessionID: "session-xyz"})
		require.NoError(t, err)
		if len(events) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, events, 2, "both turns belong to the session the client named")
}

func TestRecorder_NilIsSafe(t *testing.T) {
	var rec *Recorder
	rec.Record(capture{})
	assert.NoError(t, rec.Close())
}

func TestRecorder_RedactsCapturedBodies(t *testing.T) {
	db, ts := openTestStore(t)

	events := serve(t, ts, db,
		chatRequest(`{"model":"m","messages":[{"role":"user","content":"mail ada@example.com"}]}`),
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"noted"}}]}`))
		})

	require.Len(t, events, 1)
	assert.NotContains(t, events[0].RequestBody, "ada@example.com")
	assert.Contains(t, events[0].RequestBody, "[EMAIL_ADDRESS]")
	assert.Contains(t, events[0].Redactions, "EMAIL_ADDRESS")
}

// With capture_bodies off the extractors still run: tool, file and sensitive
// data analysis works without retaining any conversation content.
func TestRecorder_WithoutBodiesStillExtracts(t *testing.T) {
	db, ts := openTestStore(t)

	cfg := testConfig()
	cfg.CaptureBodies = false
	rec := NewRecorder(ts, db, cfg, nil, nil, discardLogger())
	defer rec.Close()

	req := chatRequest(`{"model":"m","messages":[{"role":"user","content":"token ghp_abcdefghijklmnopqrstuvwxyz0123456789"}]}`)
	Middleware(rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})).ServeHTTP(httptest.NewRecorder(), req)

	deadline := time.Now().Add(3 * time.Second)
	var events []Event
	for time.Now().Before(deadline) {
		var err error
		events, err = ts.ListEvents(context.Background(), Filter{})
		require.NoError(t, err)
		if len(events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, events, 1)

	assert.Empty(t, events[0].RequestBody)
	assert.Empty(t, events[0].ResponseBody)
	_, ok := findingFor(events[0].Findings, KindSecret, "GITHUB_TOKEN")
	assert.True(t, ok)
}

// A forwarded header is client-supplied past its first entry, so only the
// address the edge itself saw is trusted.
func TestCallerIP(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"connection only", "10.0.0.7:51234", nil, "10.0.0.7"},
		{"forwarded wins", "10.0.0.1:443", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "203.0.113.9"},
		{"first hop only", "10.0.0.1:443", map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}, "203.0.113.9"},
		{"real ip fallback", "10.0.0.1:443", map[string]string{"X-Real-IP": "198.51.100.4"}, "198.51.100.4"},
		{"loopback", "127.0.0.1:60122", nil, "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", nil)
			r.RemoteAddr = c.remote
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			assert.Equal(t, c.want, callerIP(r))
		})
	}
}
