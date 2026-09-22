package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"
)

const (
	headerContentType = "Content-Type"
	headerAPIKey      = "api-key"
	contentTypeJSON   = "application/json"
)

// OpenAI implements the Provider interface for Azure OpenAI Service.
type OpenAI struct {
	apiBase         string
	apiKey          string
	apiVersion      string
	client          *http.Client
	legacyMaxTokens bool // keep max_tokens as-is, don't convert to max_completion_tokens
	// nativeResponses is false for the AI Foundry compat flavour: those
	// deployments (Mistral, Llama, ...) expose /chat/completions only.
	nativeResponses bool
}

// Option configures an Azure OpenAI provider.
type Option func(*OpenAI)

// WithNativeResponses declares whether the deployment exposes Azure's native
// v1 Responses API. See provider.NativeResponsesCapability.
func WithNativeResponses(enabled bool) Option {
	return func(p *OpenAI) { p.nativeResponses = enabled }
}

// NewOpenAI creates a new Azure OpenAI provider.
func NewOpenAI(apiBase, apiKey, apiVersion string, opts ...Option) *OpenAI {
	p := &OpenAI{
		apiBase:         strings.TrimRight(apiBase, "/"),
		apiKey:          apiKey,
		apiVersion:      apiVersion,
		client:          perf.NewStreamingClient(60 * time.Second),
		nativeResponses: true,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// NewOpenAICompat creates an Azure OpenAI provider that keeps max_tokens as-is
// (for models like Mistral on Azure AI Foundry that don't accept max_completion_tokens).
func NewOpenAICompat(apiBase, apiKey, apiVersion string, opts ...Option) *OpenAI {
	p := &OpenAI{
		apiBase:         strings.TrimRight(apiBase, "/"),
		apiKey:          apiKey,
		apiVersion:      apiVersion,
		client:          perf.NewStreamingClient(60 * time.Second),
		legacyMaxTokens: true,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *OpenAI) Name() string { return "azure_openai" }

// SupportsNativeResponses implements provider.NativeResponsesCapability.
func (p *OpenAI) SupportsNativeResponses() bool { return p.nativeResponses }

// Complete sends a non-streaming chat completion request.
func (p *OpenAI) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	// Ensure stream is false for non-streaming
	req.Stream = false

	httpReq, err := p.buildRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure openai request failed: %w", err)
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

// Stream sends a streaming chat completion request and returns a StreamReader.
func (p *OpenAI) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	req.Stream = true

	httpReq, err := p.buildRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure openai stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, sse.ParseOpenAIError(resp)
	}

	return sse.NewReader(resp.Body, resp.Header), nil
}

func (p *OpenAI) buildRequest(ctx context.Context, req *provider.CompletionRequest) (*http.Request, error) {
	// Azure OpenAI uses the deployment name in the URL path
	url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		p.apiBase, req.Model, p.apiVersion)

	// Newer Azure models (GPT-5, o-series, nano) require max_completion_tokens
	// instead of max_tokens. Convert automatically for compatibility.
	// Skip conversion for providers using legacy max_tokens (e.g. Mistral on Azure AI Foundry).
	if !p.legacyMaxTokens && req.MaxTokens != nil && req.MaxCompletionTokens == nil {
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

	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAPIKey, p.apiKey)

	return httpReq, nil
}

// DoFileRequest forwards an Azure OpenAI Files API request without buffering
// the payload in the gateway. Azure file endpoints are account-scoped rather
// than deployment-scoped.
func (p *OpenAI) DoFileRequest(ctx context.Context, req provider.FileRequest) (*http.Response, error) {
	url := p.apiBase + "/openai" + req.Path
	query := req.RawQuery
	if query != "" {
		query += "&"
	}
	query += "api-version=" + p.apiVersion
	url += "?" + query

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return nil, fmt.Errorf("azure: creating files request: %w", err)
	}
	if req.ContentType != "" {
		httpReq.Header.Set(headerContentType, req.ContentType)
	}
	if req.ContentLength >= 0 {
		httpReq.ContentLength = req.ContentLength
	}
	httpReq.Header.Set(headerAPIKey, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure: files request failed: %w", err)
	}
	return resp, nil
}

// DoResponsesRequest forwards a request to Azure OpenAI's native v1 Responses
// endpoint. Unlike deployment-scoped Chat Completions, the deployment is
// selected by the request's model field.
func (p *OpenAI) DoResponsesRequest(ctx context.Context, body io.Reader, contentLength int64) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/openai/v1/responses", body)
	if err != nil {
		return nil, fmt.Errorf("azure: creating responses request: %w", err)
	}
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	if contentLength >= 0 {
		httpReq.ContentLength = contentLength
	}
	httpReq.Header.Set(headerAPIKey, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure: responses request failed: %w", err)
	}
	return resp, nil
}

// DoResponsesResourceRequest performs a retrieve/delete/cancel/input-items
// call against a stored response, e.g. path "/resp_123/cancel".
func (p *OpenAI) DoResponsesResourceRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, p.apiBase+"/openai/v1/responses"+path, body)
	if err != nil {
		return nil, fmt.Errorf("azure: creating responses resource request: %w", err)
	}
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAPIKey, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure: responses resource request failed: %w", err)
	}
	return resp, nil
}

// Embed sends an embeddings request to Azure OpenAI.
func (p *OpenAI) Embed(ctx context.Context, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	url := fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s",
		p.apiBase, req.Model, p.apiVersion)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("azure: marshaling embedding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("azure: creating embedding request: %w", err)
	}

	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAPIKey, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("azure: embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	var result provider.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("azure: decoding embedding response: %w", err)
	}
	return &result, nil
}
