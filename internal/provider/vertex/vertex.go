package vertex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func newStreamSafeClient() *http.Client {
	transport := perf.NewHighPerfTransport()
	transport.ResponseHeaderTimeout = 600 * time.Second
	return &http.Client{
		Timeout:   0,
		Transport: transport,
	}
}

// Provider implements the Provider interface for Google Cloud Vertex AI.
// Supports both Gemini models (native format) and partner models like Claude
// (via rawPredict with Anthropic Messages API format).
type Provider struct {
	project    string
	location   string
	modelID    string
	client     *http.Client
	isPartner  bool   // true for anthropic/etc via rawPredict
	partnerPub string // e.g., "anthropic"

	// OAuth2 token management
	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
	credentials *google.Credentials
}

// Config holds the Vertex AI provider configuration.
type Config struct {
	Project         string // GCP project ID
	Location        string // e.g., "europe-west1"
	CredentialsJSON string // service account JSON (optional, uses ADC if empty)
}

// New creates a new Vertex AI provider for the given model.
// Model format: "gemini-2.5-flash" or "claude-sonnet-4" (partner model).
func New(cfg Config, modelID string) (*Provider, error) {
	if cfg.Project == "" {
		return nil, fmt.Errorf("vertex: project is required")
	}
	if cfg.Location == "" {
		return nil, fmt.Errorf("vertex: location is required")
	}

	p := &Provider{
		project:  cfg.Project,
		location: cfg.Location,
		modelID:  modelID,
		client:   newStreamSafeClient(),
	}

	// Detect partner models (Claude, Llama, etc.)
	if isAnthropicModel(modelID) {
		p.isPartner = true
		p.partnerPub = "anthropic"
	}

	// Initialize credentials
	var err error
	scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}
	if cfg.CredentialsJSON != "" {
		p.credentials, err = google.CredentialsFromJSON(context.Background(), []byte(cfg.CredentialsJSON), scopes...)
	} else {
		// Use Application Default Credentials
		p.credentials, err = google.FindDefaultCredentials(context.Background(), scopes...)
	}
	if err != nil {
		return nil, fmt.Errorf("vertex: failed to initialize credentials: %w", err)
	}

	return p, nil
}

func (p *Provider) Name() string { return "vertex" }

// Complete sends a non-streaming request.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if p.isPartner {
		return p.completePartner(ctx, req)
	}
	return p.completeGemini(ctx, req)
}

// Stream sends a streaming request.
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	if p.isPartner {
		return p.streamPartner(ctx, req)
	}
	return p.streamGemini(ctx, req)
}

// --- Gemini native format ---

func (p *Provider) completeGemini(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	geminiReq := convertToGemini(req)
	url := p.geminiURL("generateContent")

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshaling gemini request: %w", err)
	}

	httpReq, err := p.newAuthedRequest(ctx, "POST", url, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseVertexError(resp)
	}

	var geminiResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		return nil, fmt.Errorf("vertex: decoding gemini response: %w", err)
	}

	return geminiToOpenAI(&geminiResp, req.Model, resp.Header), nil
}

func (p *Provider) streamGemini(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	geminiReq := convertToGemini(req)
	// For streaming, add alt=sse to get SSE format
	url := p.geminiURL("streamGenerateContent") + "&alt=sse"

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshaling gemini stream request: %w", err)
	}

	httpReq, err := p.newAuthedRequest(ctx, "POST", url, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: gemini stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseVertexError(resp)
	}

	return &geminiStreamReader{
		reader:  bufio.NewReader(resp.Body),
		body:    resp.Body,
		headers: resp.Header,
		model:   req.Model,
	}, nil
}

// --- Partner models (Claude via rawPredict) ---

func (p *Provider) completePartner(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	anthropicReq := convertToAnthropic(req)
	url := p.partnerURL("rawPredict")

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshaling anthropic request: %w", err)
	}

	httpReq, err := p.newAuthedRequest(ctx, "POST", url, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: partner request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseVertexError(resp)
	}

	var anthropicResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("vertex: decoding partner response: %w", err)
	}

	return anthropicToOpenAI(&anthropicResp, req.Model, resp.Header), nil
}

func (p *Provider) streamPartner(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	anthropicReq := convertToAnthropic(req)
	anthropicReq.Stream = true
	url := p.partnerURL("streamRawPredict")

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshaling anthropic stream request: %w", err)
	}

	httpReq, err := p.newAuthedRequest(ctx, "POST", url, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: partner stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseVertexError(resp)
	}

	return &anthropicStreamReader{
		reader:  bufio.NewReader(resp.Body),
		body:    resp.Body,
		headers: resp.Header,
		model:   req.Model,
	}, nil
}

// --- URL construction ---

// apiHost returns the regional endpoint, or the unprefixed one for the
// "global" location (global-aiplatform.googleapis.com does not exist).
func (p *Provider) apiHost() string {
	if p.location == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + p.location + "-aiplatform.googleapis.com"
}

func (p *Provider) geminiURL(method string) string {
	return fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
		p.apiHost(), p.project, p.location, p.modelID, method,
	)
}

func (p *Provider) partnerURL(method string) string {
	return fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s",
		p.apiHost(), p.project, p.location, p.partnerPub, p.modelID, method,
	)
}

// --- Auth ---

func (p *Provider) newAuthedRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	token, err := p.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex: getting access token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vertex: creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	return httpReq, nil
}

func (p *Provider) getAccessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return cached token if still valid (with 60s margin)
	if p.accessToken != "" && time.Now().Before(p.tokenExpiry.Add(-60*time.Second)) {
		return p.accessToken, nil
	}

	tok, err := p.credentials.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("refreshing token: %w", err)
	}

	p.accessToken = tok.AccessToken
	p.tokenExpiry = tok.Expiry
	return p.accessToken, nil
}

// --- Helpers ---

func isAnthropicModel(model string) bool {
	return strings.HasPrefix(model, "claude")
}

func parseVertexError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
		return &provider.UpstreamError{
			StatusCode: resp.StatusCode,
			Message:    errResp.Error.Message,
			Type:       errResp.Error.Status,
			Code:       fmt.Sprintf("%d", errResp.Error.Code),
		}
	}

	return &provider.UpstreamError{
		StatusCode: resp.StatusCode,
		Message:    string(body),
	}
}

// Embed sends an embeddings request to Vertex AI (Gemini text-embedding models).
func (p *Provider) Embed(ctx context.Context, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	// Vertex AI embedding uses predict endpoint
	url := fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		p.location, p.project, p.location, p.modelID,
	)

	// Convert to Vertex format
	instances := convertEmbeddingInput(req.Input)
	vertexReq := map[string]interface{}{
		"instances": instances,
	}
	if req.Dimensions != nil {
		vertexReq["parameters"] = map[string]interface{}{
			"outputDimensionality": *req.Dimensions,
		}
	}

	body, err := json.Marshal(vertexReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshaling embedding request: %w", err)
	}

	httpReq, err := p.newAuthedRequest(ctx, "POST", url, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseVertexError(resp)
	}

	var vertexResp struct {
		Predictions []struct {
			Embeddings struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
		Metadata struct {
			BillableCharacterCount int `json:"billableCharacterCount"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&vertexResp); err != nil {
		return nil, fmt.Errorf("vertex: decoding embedding response: %w", err)
	}

	// Convert to OpenAI format
	result := &provider.EmbeddingResponse{
		Object: "list",
		Model:  req.Model,
	}
	for i, pred := range vertexResp.Predictions {
		result.Data = append(result.Data, provider.EmbeddingObject{
			Object:    "embedding",
			Index:     i,
			Embedding: pred.Embeddings.Values,
		})
	}
	// Approximate token count from characters (rough heuristic)
	approxTokens := vertexResp.Metadata.BillableCharacterCount / 4
	if approxTokens == 0 {
		approxTokens = 1
	}
	result.Usage = provider.EmbeddingUsage{
		PromptTokens: approxTokens,
		TotalTokens:  approxTokens,
	}

	return result, nil
}

func convertEmbeddingInput(input interface{}) []map[string]string {
	switch v := input.(type) {
	case string:
		return []map[string]string{{"content": v}}
	case []interface{}:
		var instances []map[string]string
		for _, item := range v {
			if s, ok := item.(string); ok {
				instances = append(instances, map[string]string{"content": s})
			}
		}
		return instances
	case []string:
		var instances []map[string]string
		for _, s := range v {
			instances = append(instances, map[string]string{"content": s})
		}
		return instances
	default:
		return []map[string]string{{"content": fmt.Sprintf("%v", input)}}
	}
}
