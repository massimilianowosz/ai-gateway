package bedrock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// Provider implements the Provider interface for AWS Bedrock.
// Uses the Converse API for a unified interface across all Bedrock models.
// Supports two auth modes:
//   - Bearer token: set BearerToken in Config (no SigV4)
//   - SigV4: set AccessKeyID + SecretAccessKey (+ optional SessionToken)
type Provider struct {
	region          string
	modelID         string
	bearerToken     string // if set, use Authorization: Bearer instead of SigV4
	accessKeyID     string
	secretAccessKey string
	sessionToken    string // optional, for temporary credentials
	client          *http.Client
}

// Config holds AWS Bedrock configuration.
type Config struct {
	Region          string
	BearerToken     string // Bedrock API Key (ABSK...) — uses Authorization: Bearer
	AccessKeyID     string // for SigV4
	SecretAccessKey string // for SigV4
	SessionToken    string // optional, for SigV4 temporary credentials
}

// New creates a new Bedrock provider for the given model.
func New(cfg Config, modelID string) (*Provider, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("bedrock: region is required")
	}
	if cfg.BearerToken == "" && cfg.AccessKeyID == "" {
		return nil, fmt.Errorf("bedrock: either bearer_token or access_key_id is required")
	}
	if cfg.BearerToken == "" && cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("bedrock: secret_access_key is required when using SigV4")
	}

	return &Provider{
		region:          cfg.Region,
		modelID:         modelID,
		bearerToken:     cfg.BearerToken,
		accessKeyID:     cfg.AccessKeyID,
		secretAccessKey: cfg.SecretAccessKey,
		sessionToken:    cfg.SessionToken,
		client:          newBedrockClient(),
	}, nil
}

func newBedrockClient() *http.Client {
	transport := perf.NewHighPerfTransport()
	// 60s, not 600: this bounds the wait for response *headers*, which is what
	// makes a dead endpoint fail over quickly. Ten minutes made the router sit
	// on an endpoint that accepted the connection and then said nothing. The
	// body has no deadline, so long streams are unaffected.
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{
		Timeout:   0,
		Transport: transport,
	}
}

func (p *Provider) Name() string { return "bedrock" }

// Complete sends a non-streaming Converse request.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	converseReq := convertToConverse(req)

	url := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse",
		p.region, p.modelID)

	body, err := json.Marshal(converseReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrock: creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	if err := p.authRequest(httpReq, body); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseBedrockError(resp)
	}

	var converseResp converseResponse
	if err := json.NewDecoder(resp.Body).Decode(&converseResp); err != nil {
		return nil, fmt.Errorf("bedrock: decoding response: %w", err)
	}

	return converseToOpenAI(&converseResp, req.Model, resp.Header), nil
}

// Stream sends a streaming ConverseStream request.
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	converseReq := convertToConverse(req)

	url := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse-stream",
		p.region, p.modelID)

	body, err := json.Marshal(converseReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock: marshaling stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrock: creating stream request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")

	if err := p.authRequest(httpReq, body); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseBedrockError(resp)
	}

	return &bedrockStreamReader{
		reader:  bufio.NewReader(resp.Body),
		body:    resp.Body,
		headers: resp.Header,
		model:   req.Model,
	}, nil
}

// authRequest adds authentication to the request (bearer token or SigV4).
func (p *Provider) authRequest(req *http.Request, payload []byte) error {
	if p.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearerToken)
		return nil
	}
	return p.signRequest(req, payload)
}

// --- SigV4 Signing ---

func (p *Provider) signRequest(req *http.Request, payload []byte) error {
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)
	if p.sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", p.sessionToken)
	}

	// Create canonical request
	service := "bedrock"
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, p.region, service)

	canonicalURI := req.URL.Path
	canonicalQueryString := req.URL.RawQuery

	// Canonical headers (sorted)
	signedHeaders := p.getSignedHeaders(req)
	canonicalHeaders := p.getCanonicalHeaders(req, signedHeaders)

	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")

	// Create string to sign
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// Calculate signature
	signingKey := p.deriveSigningKey(datestamp, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Set Authorization header
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.accessKeyID, credentialScope, strings.Join(signedHeaders, ";"), signature)
	req.Header.Set("Authorization", authHeader)

	return nil
}

func (p *Provider) getSignedHeaders(req *http.Request) []string {
	headers := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}
	if p.sessionToken != "" {
		headers = append(headers, "x-amz-security-token")
	}
	sort.Strings(headers)
	return headers
}

func (p *Provider) getCanonicalHeaders(req *http.Request, signedHeaders []string) string {
	var sb strings.Builder
	for _, h := range signedHeaders {
		var val string
		switch h {
		case "host":
			val = req.URL.Host
		default:
			val = req.Header.Get(h)
		}
		sb.WriteString(h)
		sb.WriteString(":")
		sb.WriteString(strings.TrimSpace(val))
		sb.WriteString("\n")
	}
	return sb.String()
}

func (p *Provider) deriveSigningKey(datestamp, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+p.secretAccessKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(p.region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func parseBedrockError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var errResp struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Message != "" {
		return &provider.UpstreamError{
			StatusCode: resp.StatusCode,
			Message:    errResp.Message,
			Type:       errResp.Type,
		}
	}

	return &provider.UpstreamError{
		StatusCode: resp.StatusCode,
		Message:    string(body),
	}
}
