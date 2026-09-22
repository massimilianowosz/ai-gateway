// Package e2e exercises the gateway through its real route chain.
//
// These tests wire the actual middlewares, a real store and the real handlers,
// and drive them over HTTP. They exist because the unit tests do not answer the
// question that matters at release time: does the assembled system behave, on
// the wire, the way a client needs it to. Three regressions in one day —
// a column named in an INSERT that the table does not have, five paid routes
// with no funding check, and a test whose assertion was too weak to notice —
// all passed unit tests, the linter, the race detector and two reviews.
//
// The provider is scripted rather than real, so the suite runs anywhere with no
// credentials, and so a test can assert what the gateway does with an upstream
// that behaves in a particular way.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/livezone"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/server"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const masterKey = "sk-master-e2e"

// script is what the upstream will answer with, per model.
type script struct {
	// complete is the non-streaming answer.
	complete *provider.CompletionResponse
	// chunks are raw OpenAI-shaped SSE payloads, sent in order.
	chunks [][]byte
	// err, when set, is returned instead of answering.
	err error
}

type scriptedFactory struct {
	mu       sync.Mutex
	scripts  map[string]*script
	seen     *provider.CompletionRequest
	fileSeen []provider.FileRequest
	calls    int
}

func (f *scriptedFactory) Create(cfg config.ModelConfig) (provider.Provider, error) {
	return &scriptedProvider{factory: f, model: cfg.ProviderModel}, nil
}

func (f *scriptedFactory) set(model string, s *script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[model] = s
}

func (f *scriptedFactory) get(model string) *script {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.scripts[model]; ok {
		return s
	}
	return &script{complete: textAnswer("ok")}
}

type scriptedProvider struct {
	factory *scriptedFactory
	model   string
}

// lastRequest is what the provider was last asked for. Compression rewrites the
// body on its way here, and this is the only place to see the result.
func (f *scriptedFactory) lastRequest() *provider.CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

func (f *scriptedFactory) record(req *provider.CompletionRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = req
	f.calls++
}

// callCount is how many times the scripted upstream was actually invoked —
// the counter a governance denial (invalid scope, deny-only allowlist) must
// keep at zero: a gate that runs too late still shows up here even if the
// HTTP status code alone looked right.
func (f *scriptedFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.factory.record(req)
	s := p.factory.get(p.model)
	if s.err != nil {
		return nil, s.err
	}
	return s.complete, nil
}

func (p *scriptedProvider) Stream(_ context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	p.factory.record(req)
	s := p.factory.get(p.model)
	if s.err != nil {
		return nil, s.err
	}
	return &scriptedStream{chunks: s.chunks}, nil
}

type scriptedStream struct {
	chunks [][]byte
	i      int
}

func (r *scriptedStream) Next() ([]byte, error) {
	if r.i >= len(r.chunks) {
		return nil, io.EOF
	}
	c := r.chunks[r.i]
	r.i++
	return c, nil
}
func (r *scriptedStream) Close() error         { return nil }
func (r *scriptedStream) Headers() http.Header { return http.Header{} }

func textAnswer(text string) *provider.CompletionResponse {
	stop := "stop"
	return &provider.CompletionResponse{
		ID: "chatcmpl-e2e", Object: "chat.completion", Created: time.Now().Unix(),
		Model: "scripted",
		Choices: []provider.Choice{{
			Index: 0, Message: &provider.Message{Role: "assistant", Content: text},
			FinishReason: &stop,
		}},
		Usage: &provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
}

// gateway is a running gateway with a real store and route chain.
type gateway struct {
	t       *testing.T
	url     string
	scripts *scriptedFactory
	db      store.Store
}

// options tune what middleware the gateway runs.
type options struct {
	flat       []string
	restricted []string
	liveZone   *config.LiveCompressionConfig
	hiveState  *config.HiveStateConfig
	// dropParams names, per model, the request fields the deployment cannot
	// accept — the config mechanism for a provider that rejects a parameter
	// rather than ignoring it.
	dropParams map[string][]string
	// byokProviders enables scoped upstream-token forwarding (BYOK: a real,
	// governed virtual key that also forwards its own separate provider
	// credential) for exactly these provider names. nil/empty leaves it
	// disabled, matching every other test in this package.
	byokProviders []string
}

// newGateway starts one. models lists the client-facing model names; a name in
// flat is served by a deployment the gateway does not pay for.
func newGateway(t *testing.T, models []string, flat ...string) *gateway {
	return newGatewayWith(t, models, flat, nil)
}

// newGatewayWith is newGateway plus a set of models marked restricted.
func newGatewayWith(t *testing.T, models []string, flat []string, restricted []string) *gateway {
	return newGatewayOpts(t, models, options{flat: flat, restricted: restricted})
}

// newGatewayOpts is the full form, with the optional compression middlewares.
func newGatewayOpts(t *testing.T, models []string, opts options) *gateway {
	flat, restricted := opts.flat, opts.restricted
	t.Helper()

	isFlat := map[string]bool{}
	for _, m := range flat {
		isFlat[m] = true
	}
	isRestricted := map[string]bool{}
	for _, m := range restricted {
		isRestricted[m] = true
	}

	cfg := &config.Config{Server: config.ServerConfig{
		MasterKey: masterKey, MaxRequestSizeMB: 8, MaxConcurrent: 64,
	}}
	if len(opts.byokProviders) > 0 {
		cfg.Server.UpstreamTokenForwarding = config.UpstreamTokenForwardingConfig{
			Enabled: true, AllowedProviders: opts.byokProviders,
		}
	}

	modelCfgs := make([]config.ModelConfig, 0, len(models))
	for _, m := range models {
		mc := config.ModelConfig{Name: m, Provider: "scripted", ProviderModel: m}
		if isFlat[m] {
			mc.BillingMode = config.BillingModeFlat
			mc.AuthMode = config.AuthModeOAuthPassthrough
		}
		mc.Restricted = isRestricted[m]
		mc.DropParams = opts.dropParams[m]
		modelCfgs = append(modelCfgs, mc)
	}

	factory := &scriptedFactory{scripts: map[string]*script{}}
	registry, err := provider.NewRegistry(modelCfgs, factory)
	require.NoError(t, err)

	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "e2e.db"),
	})
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background()))
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(cfg, logger)
	authMw := auth.NewMiddleware(masterKey, db)
	authMw.SetGatewayBilledFunc(registry.IsGatewayBilled)

	// The optional middlewares, in the order main.go wires them: live
	// compression first, so history compression sees an already-slimmer delta.
	var extra []middleware.Middleware
	if opts.liveZone != nil {
		cfg := *opts.liveZone
		cfg.ApplyDefaults()
		extra = append(extra, livezone.Middleware(cfg, nil, nil, logger))
	}
	if opts.hiveState != nil {
		hsCfg := *opts.hiveState
		hsCfg.ApplyDefaults()
		hs, err := hivestate.New(hsCfg, registry, logger)
		require.NoError(t, err)
		extra = append(extra, hivestate.Middleware(hs, db, nil, nil, registry, nil, logger))
	}

	server.RegisterRoutes(srv, registry, nil, authMw, db, nil, nil, nil, nil, nil, extra...)

	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	return &gateway{t: t, url: ts.URL, scripts: factory, db: db}
}

// mintKey creates a key through the admin API and returns the raw secret.
func (g *gateway) mintKey(budget float64, models ...string) string {
	g.t.Helper()
	body := map[string]any{"name": "e2e", "budget": budget}
	if len(models) > 0 {
		body["models"] = models
	}
	raw, _ := json.Marshal(body)
	resp := g.do("POST", "/v1/key/generate", masterKey, "application/json", string(raw))
	require.Equal(g.t, http.StatusOK, resp.code, "mintKey: %s", resp.body)

	var out struct {
		Key string `json:"key"`
	}
	require.NoError(g.t, json.Unmarshal([]byte(resp.body), &out))
	require.NotEmpty(g.t, out.Key)
	return out.Key
}

type response struct {
	code int
	body string
	hdr  http.Header
}

func (g *gateway) do(method, path, key, contentType, body string) response {
	g.t.Helper()
	req, err := http.NewRequest(method, g.url+path, strings.NewReader(body))
	require.NoError(g.t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(g.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(g.t, err)
	return response{code: resp.StatusCode, body: string(raw), hdr: resp.Header}
}

func (g *gateway) post(path, key, body string) response {
	return g.do("POST", path, key, "application/json", body)
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	Name string
	Data map[string]any
}

// parseSSE turns an SSE body into events, keeping order.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	name := ""
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				out = append(out, sseEvent{Name: "[DONE]"})
				continue
			}
			var data map[string]any
			if json.Unmarshal([]byte(payload), &data) != nil {
				continue
			}
			if n, ok := data["type"].(string); ok && name == "" {
				name = n
			}
			out = append(out, sseEvent{Name: name, Data: data})
			name = ""
		}
	}
	return out
}

// openAIChunk builds one OpenAI-shaped streaming chunk.
func openAIChunk(delta map[string]any, finish *string) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-e2e", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": "scripted",
		"choices": []any{choice},
	})
	return raw
}

func toolCallChunk(index int, id, name, args string) []byte {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	tc := map[string]any{"index": index, "function": fn}
	if id != "" {
		tc["id"] = id
		tc["type"] = "function"
	}
	return openAIChunk(map[string]any{"tool_calls": []any{tc}}, nil)
}

var _ = fmt.Sprintf

// seedUnfundedKey writes a key with no budget straight to the store. The admin
// API refuses to mint one, which is exactly the state a key issued before the
// funding gate is in.
func (g *gateway) seedUnfundedKey(models ...string) string {
	g.t.Helper()
	raw := fmt.Sprintf("sk-ubq-unfunded-%d", time.Now().UnixNano())
	key := &store.APIKey{
		ID: raw[:20], KeyHash: auth.HashKey(raw), KeyPrefix: raw[:10],
		Name: "unfunded", Budget: 0, Active: true, Models: store.StringList(models),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(g.t, g.db.CreateKey(context.Background(), key))
	return raw
}

// setSpend marks a key as having spent an amount.
func (g *gateway) setSpend(rawKey string, spent float64) {
	g.t.Helper()
	require.NoError(g.t, g.db.LogSpend(context.Background(), store.SpendRecord{
		KeyHash: auth.HashKey(rawKey), Model: "paid", Provider: "scripted",
		Cost: spent, TotalTokens: 1, CreatedAt: time.Now(),
	}))
}

// seedTeam creates a team with the given granted models.
func (g *gateway) seedTeam(id string, granted ...string) {
	g.t.Helper()
	require.NoError(g.t, g.db.CreateTeam(context.Background(), &store.Team{
		ID: id, Name: id, GrantedModels: store.StringList(granted),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}))
}

// mintTeamKey mints a key that belongs to a team.
func (g *gateway) mintTeamKey(teamID string, budget float64, models ...string) string {
	g.t.Helper()
	body := map[string]any{"name": "e2e-team", "budget": budget, "team_id": teamID}
	if len(models) > 0 {
		body["models"] = models
	}
	raw, _ := json.Marshal(body)
	resp := g.do("POST", "/v1/key/generate", masterKey, "application/json", string(raw))
	require.Equal(g.t, http.StatusOK, resp.code, "mintTeamKey: %s", resp.body)
	var out struct {
		Key string `json:"key"`
	}
	require.NoError(g.t, json.Unmarshal([]byte(resp.body), &out))
	return out.Key
}

// DoFileRequest makes the scripted provider a Files-capable upstream, so the
// gateway's own file bookkeeping — tenant scoping, id mapping, the metadata it
// keeps and the bytes it does not — can be driven end to end.
func (p *scriptedProvider) DoFileRequest(_ context.Context, req provider.FileRequest) (*http.Response, error) {
	p.factory.recordFile(req)

	switch {
	case req.Method == http.MethodPost:
		body, _ := json.Marshal(map[string]any{
			"id": "file-upstream-1", "object": "file", "bytes": 12,
			"created_at": time.Now().Unix(), "filename": "doc.txt", "purpose": "assistants",
		})
		return jsonResponse(http.StatusOK, body), nil
	case strings.HasSuffix(req.Path, "/content"):
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("contenuto vero")),
		}, nil
	case req.Method == http.MethodDelete:
		body, _ := json.Marshal(map[string]any{"id": "file-upstream-1", "object": "file", "deleted": true})
		return jsonResponse(http.StatusOK, body), nil
	default:
		body, _ := json.Marshal(map[string]any{
			"id": "file-upstream-1", "object": "file", "bytes": 12,
			"created_at": time.Now().Unix(), "filename": "doc.txt", "purpose": "assistants",
		})
		return jsonResponse(http.StatusOK, body), nil
	}
}

func jsonResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func (f *scriptedFactory) recordFile(req provider.FileRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileSeen = append(f.fileSeen, req)
}

// fileRequests returns every file call the upstream received.
func (f *scriptedFactory) fileRequests() []provider.FileRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.FileRequest(nil), f.fileSeen...)
}

// multipartFile builds a small multipart upload body.
func multipartFile(t *testing.T, name, content string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("purpose", "assistants"))
	part, err := w.CreateFormFile("file", name)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return w.FormDataContentType(), buf.String()
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

func readAll(resp *http.Response) (string, error) {
	raw, err := io.ReadAll(resp.Body)
	return string(raw), err
}

// request builds a request the caller can adjust before sending.
func (g *gateway) request(method, path, key, contentType, body string) *http.Request {
	g.t.Helper()
	req, err := http.NewRequest(method, g.url+path, strings.NewReader(body))
	require.NoError(g.t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func (g *gateway) send(req *http.Request) response {
	g.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(g.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(g.t, err)
	return response{code: resp.StatusCode, body: string(raw), hdr: resp.Header}
}
