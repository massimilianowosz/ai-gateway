package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"
)

const providerName = "github_copilot"

const (
	defaultAPIBase             = "https://api.githubcopilot.com"
	defaultUserAgent           = "GitHubCopilotChat/0.35.0"
	defaultEditorVersion       = "vscode/1.107.0"
	defaultEditorPluginVersion = "copilot-chat/0.35.0"
	defaultIntegrationID       = "vscode-chat"
)

// Provider implements the Ubiquum provider interface for GitHub Copilot's
// hosted model API. It expects a Copilot bearer token supplied either through
// the scoped upstream-token context or, for single-user/test deployments, as a
// configured API key.
type Provider struct {
	apiBase string
	apiKey  string
	client  *http.Client
}

// New creates a GitHub Copilot provider.
func New(apiBase, apiKey string) *Provider {
	apiBase = strings.TrimRight(apiBase, "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}

	return &Provider{
		apiBase: apiBase,
		apiKey:  strings.TrimSpace(apiKey),
		client:  perf.NewStreamingClient(60 * time.Second),
	}
}

func (p *Provider) Name() string { return providerName }

// Complete sends a non-streaming chat completion request.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	req.Stream = false

	httpReq, err := p.buildChatRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("github copilot request failed: %w", err)
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
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	req.Stream = true

	httpReq, err := p.buildChatRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("github copilot stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, sse.ParseOpenAIError(resp)
	}

	return sse.NewReader(resp.Body, resp.Header), nil
}

func (p *Provider) buildChatRequest(ctx context.Context, req *provider.CompletionRequest) (*http.Request, error) {
	if req.MaxTokens != nil && req.MaxCompletionTokens == nil {
		req.MaxCompletionTokens = req.MaxTokens
		req.MaxTokens = nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatCompletionsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	p.setCopilotHeaders(httpReq)

	token := auth.UpstreamTokenForProvider(ctx, providerName)
	if token == "" {
		token = p.apiKey
	}
	if token == "" {
		return nil, &provider.UpstreamError{
			StatusCode: http.StatusUnauthorized,
			Message:    "github_copilot provider requires a scoped upstream bearer token",
			Type:       "authentication_error",
			Code:       "missing_upstream_token",
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	return httpReq, nil
}

func (p *Provider) setCopilotHeaders(req *http.Request) {
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Editor-Version", defaultEditorVersion)
	req.Header.Set("Editor-Plugin-Version", defaultEditorPluginVersion)
	req.Header.Set("Copilot-Integration-Id", defaultIntegrationID)
}

func (p *Provider) chatCompletionsURL() string {
	return stripV1Suffix(p.apiBase) + "/chat/completions"
}

func stripV1Suffix(base string) string {
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
}
