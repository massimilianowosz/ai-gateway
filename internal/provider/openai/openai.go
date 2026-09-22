package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"
)

// Compatible implements the Provider interface for any OpenAI-compatible API
// (OpenAI, Groq, Together, Mistral, etc.).
type Compatible struct {
	apiBase string
	apiKey  string
	client  *http.Client
	// nativeResponses records whether this endpoint exposes /responses. It is
	// off by default because most OpenAI-compatible backends implement
	// /chat/completions only; the "openai" provider turns it on explicitly.
	nativeResponses bool
	// forwardTenant sends the caller's team id upstream. Only endpoints we own
	// may receive it: it is an identity assertion, and a third-party provider
	// has no business learning which tenant is behind a request.
	forwardTenant bool
}

// TenantHeader carries the authenticated team id to a Ubiquum-owned upstream.
const TenantHeader = "X-Ubiquum-Tenant"

// Option configures a Compatible provider.
type Option func(*Compatible)

// WithNativeResponses declares whether the endpoint exposes OpenAI's native
// Responses API. See provider.NativeResponsesCapability.
func WithNativeResponses(enabled bool) Option {
	return func(p *Compatible) { p.nativeResponses = enabled }
}

// WithTenantForwarding sends the authenticated team id upstream as
// TenantHeader. Use it only for Ubiquum-owned upstreams that must resolve a
// tenant-scoped resource — the Edge proxy, whose model names are global while
// the runtimes behind them are not, so without the caller's tenant it can only
// guess which one to route to.
func WithTenantForwarding() Option {
	return func(p *Compatible) { p.forwardTenant = true }
}

// NewCompatible creates a new OpenAI-compatible provider.
func NewCompatible(apiBase, apiKey string, opts ...Option) *Compatible {
	p := &Compatible{
		apiBase: strings.TrimRight(apiBase, "/"),
		apiKey:  apiKey,
		client:  perf.NewStreamingClient(60 * time.Second),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// SupportsNativeResponses implements provider.NativeResponsesCapability.
func (p *Compatible) SupportsNativeResponses() bool { return p.nativeResponses }

func (p *Compatible) Name() string { return "openai_compatible" }

// setAuthHeader sets the Authorization header, preferring the upstream token from
// context (pass-through mode) over the configured provider API key.
func (p *Compatible) setAuthHeader(ctx context.Context, req *http.Request) {
	if token := auth.UpstreamTokenFromContext(ctx); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if p.forwardTenant {
		if team := auth.TeamFromContext(ctx); team != nil {
			req.Header.Set(TenantHeader, team.ID)
		}
	}
}

// Complete sends a non-streaming chat completion request.
func (p *Compatible) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	req.Stream = false

	httpReq, err := p.buildRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai compatible request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	var result provider.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	result.Headers = resp.Header
	return &result, nil
}

// Stream sends a streaming chat completion request.
func (p *Compatible) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	req.Stream = true

	httpReq, err := p.buildRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai compatible stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, sse.ParseOpenAIError(resp)
	}

	return sse.NewReader(resp.Body, resp.Header), nil
}

func (p *Compatible) buildRequest(ctx context.Context, req *provider.CompletionRequest) (*http.Request, error) {
	url := p.apiBase + "/chat/completions"

	// Convert max_tokens → max_completion_tokens for newer OpenAI models.
	// max_completion_tokens is the newer standard; older providers still accept both.
	if req.MaxTokens != nil && req.MaxCompletionTokens == nil {
		req.MaxCompletionTokens = req.MaxTokens
		req.MaxTokens = nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	return httpReq, nil
}

// Embed sends an embeddings request to the OpenAI-compatible API.
func (p *Compatible) Embed(ctx context.Context, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	url := p.apiBase + "/embeddings"

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling embedding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating embedding request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	var result provider.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}
	return &result, nil
}

// Moderate sends a moderation request to the OpenAI-compatible API.
func (p *Compatible) Moderate(ctx context.Context, req *provider.ModerationRequest) (*provider.ModerationResponse, error) {
	url := p.apiBase + "/moderations"

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling moderation request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating moderation request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("moderation request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	var result provider.ModerationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding moderation response: %w", err)
	}
	return &result, nil
}

// GenerateImage sends an image generation request.
func (p *Compatible) GenerateImage(ctx context.Context, requestBody []byte) ([]byte, error) {
	return p.forwardJSON(ctx, "/images/generations", requestBody)
}

// Speak sends a TTS request and returns audio bytes.
func (p *Compatible) Speak(ctx context.Context, requestBody []byte) ([]byte, string, error) {
	url := p.apiBase + "/audio/speech"

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, "", fmt.Errorf("creating speech request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("speech request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", sse.ParseOpenAIError(resp)
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading speech response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	return audio, contentType, nil
}

// Transcribe sends an audio transcription request.
func (p *Compatible) Transcribe(ctx context.Context, req *provider.TranscriptionRequest) ([]byte, error) {
	url := p.apiBase + "/audio/transcriptions"

	// Build multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Model field
	_ = writer.WriteField("model", req.Model)
	if req.Language != "" {
		_ = writer.WriteField("language", req.Language)
	}
	if req.Format != "" {
		_ = writer.WriteField("response_format", req.Format)
	}

	// File field
	part, err := writer.CreateFormFile("file", req.Filename)
	if err != nil {
		return nil, fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(req.File); err != nil {
		return nil, fmt.Errorf("writing file data: %w", err)
	}
	writer.Close()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return nil, fmt.Errorf("creating transcription request: %w", err)
	}

	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("transcription request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	return io.ReadAll(resp.Body)
}

// DoFileRequest forwards an OpenAI Files API request without buffering the
// uploaded file in the gateway.
func (p *Compatible) DoFileRequest(ctx context.Context, req provider.FileRequest) (*http.Response, error) {
	url := p.apiBase + req.Path
	if req.RawQuery != "" {
		url += "?" + req.RawQuery
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return nil, fmt.Errorf("creating files request: %w", err)
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	if req.ContentLength >= 0 {
		httpReq.ContentLength = req.ContentLength
	}
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("files request failed: %w", err)
	}
	return resp, nil
}

// DoResponsesRequest forwards a native OpenAI Responses API request.
func (p *Compatible) DoResponsesRequest(ctx context.Context, body io.Reader, contentLength int64) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/responses", body)
	if err != nil {
		return nil, fmt.Errorf("creating responses request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if contentLength >= 0 {
		httpReq.ContentLength = contentLength
	}
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("responses request failed: %w", err)
	}
	return resp, nil
}

// DoResponsesResourceRequest performs a retrieve/delete/cancel/input-items
// call against a stored response, e.g. path "/resp_123/cancel".
func (p *Compatible) DoResponsesResourceRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, p.apiBase+"/responses"+path, body)
	if err != nil {
		return nil, fmt.Errorf("creating responses resource request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("responses resource request failed: %w", err)
	}
	return resp, nil
}

// Forward sends a raw request to the given endpoint path.
func (p *Compatible) Forward(ctx context.Context, endpoint string, contentType string, body io.Reader) ([]byte, string, error) {
	url := p.apiBase + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return nil, "", fmt.Errorf("creating forward request: %w", err)
	}

	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	} else {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("forward request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", sse.ParseOpenAIError(resp)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading forward response: %w", err)
	}

	return respBody, resp.Header.Get("Content-Type"), nil
}

func (p *Compatible) forwardJSON(ctx context.Context, path string, requestBody []byte) ([]byte, error) {
	url := p.apiBase + path

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(ctx, httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	return io.ReadAll(resp.Body)
}
