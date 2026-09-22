package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "http://localhost:4000"

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) HasKey() bool {
	return c.apiKey != ""
}

// ReloadKey re-reads the master key from the config file.
func (c *Client) ReloadKey() {
	if key := readMasterKeyFromConfig(); key != "" {
		c.apiKey = key
	}
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("request failed with HTTP %d", e.Status)
	}
	return fmt.Sprintf("request failed with HTTP %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return parseAPIError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func parseAPIError(status int, data []byte) error {
	var shaped struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(data, &shaped) == nil {
		switch v := shaped.Error.(type) {
		case string:
			return &apiError{Status: status, Message: v}
		case map[string]any:
			if msg, ok := v["message"].(string); ok {
				return &apiError{Status: status, Message: msg}
			}
		}
	}
	return &apiError{Status: status, Message: strings.TrimSpace(string(data))}
}

type StatusResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Models int    `json:"models,omitempty"`
}

// GetRaw performs a GET and returns the raw response body.
func (c *Client) GetRaw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp.StatusCode, data)
	}
	return data, nil
}

func (c *Client) Health(ctx context.Context) (StatusResponse, error) {
	var resp StatusResponse
	err := c.do(ctx, http.MethodGet, "/health", nil, nil, &resp)
	return resp, err
}

func (c *Client) Ready(ctx context.Context) (StatusResponse, error) {
	var resp StatusResponse
	err := c.do(ctx, http.MethodGet, "/ready", nil, nil, &resp)
	return resp, err
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var resp struct {
		Data []Model `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/models", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

type CreateKeyRequest struct {
	Name      string   `json:"name,omitempty"`
	TeamID    string   `json:"team_id,omitempty"`
	Budget    float64  `json:"budget,omitempty"`
	RateLimit int      `json:"rate_limit,omitempty"`
	Models    []string `json:"models,omitempty"`
}

type APIKey struct {
	ID        string    `json:"id"`
	Key       string    `json:"key,omitempty"`
	KeyPrefix string    `json:"key_prefix"`
	Name      string    `json:"name"`
	TeamID    string    `json:"team_id"`
	Budget    float64   `json:"budget"`
	Spend     float64   `json:"spend"`
	RateLimit int       `json:"rate_limit"`
	Models    []string  `json:"models"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) CreateKey(ctx context.Context, req CreateKeyRequest) (APIKey, error) {
	var key APIKey
	err := c.do(ctx, http.MethodPost, "/v1/key/generate", nil, req, &key)
	return key, err
}

func (c *Client) ListKeys(ctx context.Context, teamID string) ([]APIKey, error) {
	query := url.Values{}
	if teamID != "" {
		query.Set("team_id", teamID)
	}
	var resp struct {
		Keys []APIKey `json:"keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/key/list", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

func (c *Client) KeyInfo(ctx context.Context, rawKey string) (APIKey, error) {
	query := url.Values{"key": []string{rawKey}}
	var key APIKey
	err := c.do(ctx, http.MethodGet, "/v1/key/info", query, nil, &key)
	return key, err
}

func (c *Client) DeleteKey(ctx context.Context, rawKey string) error {
	return c.do(ctx, http.MethodPost, "/v1/key/delete", nil, map[string]string{"key": rawKey}, nil)
}

type UpdateKeyRequest struct {
	Key    string          `json:"key"`
	Params UpdateKeyParams `json:"params"`
}

type UpdateKeyParams struct {
	Name      *string    `json:"name,omitempty"`
	Budget    *float64   `json:"budget,omitempty"`
	RateLimit *int       `json:"rate_limit,omitempty"`
	Models    []string   `json:"models,omitempty"`
	Metadata  *string    `json:"metadata,omitempty"`
	Active    *bool      `json:"active,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (c *Client) UpdateKey(ctx context.Context, rawKey string, params UpdateKeyParams) error {
	return c.do(ctx, http.MethodPost, "/v1/key/update", nil, UpdateKeyRequest{Key: rawKey, Params: params}, nil)
}

// CreateTeamRequest holds parameters for creating a team via the admin API.
type CreateTeamRequest struct {
	Name   string  `json:"name"`
	Budget float64 `json:"budget,omitempty"`
}

// TeamResponse represents a team returned by the API.
type TeamResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Budget    float64   `json:"budget"`
	Spend     float64   `json:"spend"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) CreateTeam(ctx context.Context, req CreateTeamRequest) (TeamResponse, error) {
	var team TeamResponse
	err := c.do(ctx, http.MethodPost, "/v1/team/create", nil, req, &team)
	return team, err
}

type SpendSummary struct {
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Requests int     `json:"requests"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
	AvgMs    float64 `json:"avg_ms"`
}

func (c *Client) SpendLogs(ctx context.Context, rawKey, model string) ([]SpendSummary, error) {
	query := url.Values{}
	if rawKey != "" {
		query.Set("key", rawKey)
	}
	if model != "" {
		query.Set("model", model)
	}
	var resp struct {
		SpendLogs []SpendSummary `json:"spend_logs"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/spend/logs", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.SpendLogs, nil
}

type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	// Routing metadata (from response headers)
	Provider string // X-Ubiquum-Provider
	Attempts string // X-Ubiquum-Attempts
	Failed   string // X-Ubiquum-Failed
}

func (c *Client) Chat(ctx context.Context, model, prompt string) (ChatResponse, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	var resp ChatResponse

	data, err := json.Marshal(payload)
	if err != nil {
		return resp, fmt.Errorf("encode request: %w", err)
	}

	endpoint := c.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return resp, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return resp, fmt.Errorf("connect to %s: %w", c.baseURL, err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		return resp, parseAPIError(httpResp.StatusCode, body)
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return resp, fmt.Errorf("decode response: %w", err)
	}

	// Extract routing headers
	resp.Provider = httpResp.Header.Get("X-Ubiquum-Provider")
	resp.Attempts = httpResp.Header.Get("X-Ubiquum-Attempts")
	resp.Failed = httpResp.Header.Get("X-Ubiquum-Failed")

	return resp, nil
}

type TeamInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) ListTeams(ctx context.Context) ([]TeamInfo, error) {
	var resp struct {
		Teams []TeamInfo `json:"teams"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/team/list", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Teams, nil
}

// Cache admin methods

type CacheMetricResponse struct {
	ID          uint    `json:"ID"`
	TeamID      string  `json:"TeamID"`
	Model       string  `json:"Model"`
	Band        string  `json:"Band"`
	Score       float64 `json:"Score"`
	TokensSaved int     `json:"TokensSaved"`
	LatencyMs   int64   `json:"LatencyMs"`
	CreatedAt   string  `json:"CreatedAt"`
}

func (c *Client) CacheFlush(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/cache/flush", nil, nil, nil)
}

func (c *Client) CacheMetrics(ctx context.Context, limit int) ([]CacheMetricResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp struct {
		Metrics []CacheMetricResponse `json:"metrics"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/cache/metrics", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Metrics, nil
}

type StateMetricResponse struct {
	ID             uint    `json:"ID"`
	Model          string  `json:"Model"`
	RequestModel   string  `json:"RequestModel"`
	Mode           string  `json:"Mode"`
	Intent         string  `json:"Intent"`
	OriginalTokens int     `json:"OriginalTokens"`
	ResultTokens   int     `json:"ResultTokens"`
	Ratio          float64 `json:"Ratio"`
	LatencyMs      int64   `json:"LatencyMs"`
	FallbackReason string  `json:"FallbackReason"`
	RouteLevel     string  `json:"RouteLevel"`
	RouteModel     string  `json:"RouteModel"`
	RouteEffort    string  `json:"RouteEffort"`
	RouteSavedCost float64 `json:"RouteSavedCost"`
	CreatedAt      string  `json:"CreatedAt"`
}

func (c *Client) StateMetrics(ctx context.Context, limit int) ([]StateMetricResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp struct {
		Metrics []StateMetricResponse `json:"metrics"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/state/metrics", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Metrics, nil
}

func (c *Client) CacheStats(ctx context.Context) (int, error) {
	var resp struct {
		Entries int `json:"entries"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/cache/stats", nil, nil, &resp); err != nil {
		return 0, err
	}
	return resp.Entries, nil
}

// RouteSettingsResponse represents per-team route config from the API.
type RouteSettingsResponse struct {
	TeamID          string               `json:"team_id"`
	Enabled         *bool                `json:"enabled"`
	Levels          []RouteSettingsLevel `json:"levels"`
	ThinkingBudgets map[string]int       `json:"thinking_budgets"`
}

type RouteSettingsLevel struct {
	Name        string `json:"name"`
	Model       string `json:"model"`
	Description string `json:"description,omitempty"`
}

func (c *Client) GetRouteSettings(ctx context.Context, teamID string) (*RouteSettingsResponse, error) {
	query := url.Values{}
	query.Set("team_id", teamID)
	var resp RouteSettingsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/route/settings", query, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UpdateRouteSettings(ctx context.Context, req map[string]any) (*RouteSettingsResponse, error) {
	var resp RouteSettingsResponse
	if err := c.do(ctx, http.MethodPost, "/v1/route/settings", nil, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
