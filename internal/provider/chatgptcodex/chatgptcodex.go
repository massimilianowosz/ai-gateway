// Package chatgptcodex implements a Provider that talks to OpenAI's
// ChatGPT-backed Codex API (the same backend used by Codex Desktop/CLI when
// signed in with a ChatGPT Plus/Pro/Business subscription instead of a
// pay-per-token API key).
//
// Unlike the public OpenAI API, this backend:
//   - is hosted at https://chatgpt.com/backend-api/codex (not api.openai.com)
//   - speaks the OpenAI Responses API wire format exclusively (not chat
//     completions)
//   - requires a ChatGPT OAuth access token (Bearer) plus a ChatGPT-Account-Id
//     header identifying the workspace/account the subscription belongs to
//
// This provider never holds its own API key: it exists purely to relay a
// Ubiquum request using a *client-supplied* upstream OAuth token and
// account id (see internal/auth.UpstreamTokenForProvider /
// UpstreamAccountIDForProvider), forwarded by ubiquum-cli's `codex` proxy
// after the user completes `ubiquum-cli codex login`. This mirrors the
// Claude Pro/Max OAuth pass-through pattern in internal/provider/anthropic.
package chatgptcodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"
)

const (
	// providerName is used both as Provider.Name() and as the provider
	// identifier clients must send in the X-Ubiquum-Upstream-Provider
	// header when forwarding an upstream OAuth token/account id.
	providerName = "chatgpt_codex"

	defaultBaseURL = "https://chatgpt.com/backend-api/codex"

	// codexOriginator/codexUserAgent mirror the identity Codex's own Rust CLI
	// sends. The ChatGPT backend is not a documented public API; without an
	// originator/User-Agent resembling the real client, requests have been
	// observed being rejected as anomalous traffic.
	codexOriginator = "codex_cli_rs"
	codexUserAgent  = "codex_cli_rs/0.1.0 (ubiquum-ai-gateway)"
)

func newClient() *http.Client {
	transport := perf.NewHighPerfTransport()
	transport.ResponseHeaderTimeout = 600 * time.Second
	return &http.Client{Timeout: 0, Transport: transport}
}

// Provider implements the Provider interface for the ChatGPT-backed Codex
// Responses API.
type Provider struct {
	baseURL string
	modelID string
	client  *http.Client
}

// New creates a new ChatGPT/Codex subscription pass-through provider.
// modelID is the upstream Codex model id (e.g. "gpt-5.1-codex"); an empty
// modelID falls back to whatever model name the deployment was registered
// under.
func New(modelID string) *Provider {
	return &Provider{
		baseURL: defaultBaseURL,
		modelID: modelID,
		client:  newClient(),
	}
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) modelFor(req *provider.CompletionRequest) string {
	if p.modelID != "" {
		return p.modelID
	}
	return req.Model
}

// Complete sends a non-streaming request to the Responses API.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	wireReq := convertToResponsesWire(req, p.modelFor(req))
	wireReq.Stream = false

	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: creating request: %w", err)
	}
	p.setHeaders(httpReq, false)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, sse.ParseOpenAIError(resp)
	}

	var wireResp responsesWireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wireResp); err != nil {
		return nil, fmt.Errorf("chatgpt_codex: decoding response: %w", err)
	}

	return responsesWireToOpenAI(&wireResp, req.Model, resp.Header), nil
}

// Stream sends a streaming request to the Responses API.
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	wireReq := convertToResponsesWire(req, p.modelFor(req))
	wireReq.Stream = true

	body, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: creating request: %w", err)
	}
	p.setHeaders(httpReq, true)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, sse.ParseOpenAIError(resp)
	}

	return &streamReader{
		reader:  bufio.NewReader(resp.Body),
		body:    resp.Body,
		headers: resp.Header,
		model:   req.Model,
	}, nil
}

func (p *Provider) setHeaders(req *http.Request, streaming bool) {
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	if token := auth.UpstreamTokenForProvider(req.Context(), providerName); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accountID := auth.UpstreamAccountIDForProvider(req.Context(), providerName); accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("originator", codexOriginator)
	req.Header.Set("User-Agent", codexUserAgent)
}

// DoResponsesRequest forwards a client's Responses request to the
// ChatGPT-backed Codex endpoint untouched, so parameters the gateway does not
// model still reach the backend. It implements provider.ResponsesAPI, which
// lets /v1/responses skip the lossy Responses -> chat -> Responses round trip
// this provider would otherwise perform via Complete/Stream.
func (p *Provider) DoResponsesRequest(ctx context.Context, body io.Reader, _ int64) (*http.Response, error) {
	payload, streaming, err := prepareCodexResponsesBody(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: creating responses request: %w", err)
	}
	httpReq.ContentLength = int64(len(payload))
	p.setHeaders(httpReq, streaming)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chatgpt_codex: responses request failed: %w", err)
	}
	return resp, nil
}

// prepareCodexResponsesBody forces store:false — see responsesWireRequest.Store
// for why this backend requires it — and reports whether the caller asked for a
// stream, which decides the Accept header.
func prepareCodexResponsesBody(body io.Reader) ([]byte, bool, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, false, fmt.Errorf("chatgpt_codex: reading request body: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, fmt.Errorf("chatgpt_codex: invalid Responses request: %w", err)
	}
	fields["store"] = json.RawMessage("false")

	var streaming bool
	if encoded, ok := fields["stream"]; ok {
		_ = json.Unmarshal(encoded, &streaming)
	}

	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, false, fmt.Errorf("chatgpt_codex: encoding request body: %w", err)
	}
	return payload, streaming, nil
}

// --- Responses API wire types (request) ---

type responsesWireRequest struct {
	Model             string               `json:"model"`
	Input             []responsesWireInput `json:"input"`
	Instructions      string               `json:"instructions,omitempty"`
	Stream            bool                 `json:"stream,omitempty"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              *float64             `json:"top_p,omitempty"`
	MaxOutputTokens   *int                 `json:"max_output_tokens,omitempty"`
	Tools             []responsesWireTool  `json:"tools,omitempty"`
	ToolChoice        interface{}          `json:"tool_choice,omitempty"`
	ParallelToolCalls bool                 `json:"parallel_tool_calls"`
	// Store must always be false: the ChatGPT-backed Codex backend rejects
	// requests with HTTP 400 ("Store must be set to false") when this field
	// is omitted, since it defaults to true in the public Responses API.
	Store bool `json:"store"`
}

type responsesWireTool struct {
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type responsesWireContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesWireInput struct {
	Type      string                     `json:"type"`
	Role      string                     `json:"role,omitempty"`
	Content   []responsesWireContentPart `json:"content,omitempty"`
	CallID    string                     `json:"call_id,omitempty"`
	Name      string                     `json:"name,omitempty"`
	Arguments string                     `json:"arguments,omitempty"`
	Output    string                     `json:"output,omitempty"`
}

// MarshalJSON serializes a responsesWireInput, ensuring the fields required
// by each "type" discriminant are always present even when their value is
// the empty string. The ChatGPT-backed Codex backend rejects requests with
// HTTP 400 ("Missing required parameter: 'input[N].output'") when a
// function_call_output item's "output" key is entirely absent (e.g. a tool
// call that produced no output text) -- plain `omitempty` would drop such
// keys for a zero-value string, so we marshal each type's required fields
// explicitly instead of relying on struct tags for all of them.
func (i responsesWireInput) MarshalJSON() ([]byte, error) {
	switch i.Type {
	case "function_call_output":
		return json.Marshal(struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}{Type: i.Type, CallID: i.CallID, Output: i.Output})
	case "function_call":
		return json.Marshal(struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Type: i.Type, CallID: i.CallID, Name: i.Name, Arguments: i.Arguments})
	default: // "message"
		return json.Marshal(struct {
			Type    string                     `json:"type"`
			Role    string                     `json:"role,omitempty"`
			Content []responsesWireContentPart `json:"content,omitempty"`
		}{Type: i.Type, Role: i.Role, Content: i.Content})
	}
}

// --- Responses API wire types (response) ---

type responsesWireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// InputTokensDetails carries how much of the prompt was served from the
	// provider cache. Without it every turn reads as a full-price miss.
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// OutputTokensDetails carries the thinking half of the output, which on a
	// reasoning model is most of it.
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type responsesWireOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesWireOutputItem struct {
	Type      string                       `json:"type"`
	ID        string                       `json:"id"`
	Role      string                       `json:"role,omitempty"`
	Content   []responsesWireOutputContent `json:"content,omitempty"`
	CallID    string                       `json:"call_id,omitempty"`
	Name      string                       `json:"name,omitempty"`
	Arguments string                       `json:"arguments,omitempty"`
}

type responsesWireResponse struct {
	ID     string                    `json:"id"`
	Status string                    `json:"status"`
	Model  string                    `json:"model"`
	Output []responsesWireOutputItem `json:"output"`
	Usage  *responsesWireUsage       `json:"usage,omitempty"`
}

// --- Conversion: internal -> Responses wire request ---

func convertToResponsesWire(req *provider.CompletionRequest, modelID string) *responsesWireRequest {
	wr := &responsesWireRequest{
		Model:             modelID,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		ParallelToolCalls: true,
		Store:             false,
	}
	if req.MaxTokens != nil {
		wr.MaxOutputTokens = req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		wr.MaxOutputTokens = req.MaxCompletionTokens
	}

	var instructions []string
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			if text := messageContentToText(msg.Content); text != "" {
				instructions = append(instructions, text)
			}
			continue

		case "tool":
			wr.Input = append(wr.Input, responsesWireInput{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: messageContentToText(msg.Content),
			})
			continue
		}

		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				wr.Input = append(wr.Input, responsesWireInput{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			continue
		}

		// The Responses API requires "output_text" (or "refusal") for
		// assistant-authored content parts, and "input_text" for
		// user/developer content parts; sending "input_text" for an
		// assistant message is rejected with HTTP 400.
		contentType := "input_text"
		if msg.Role == "assistant" {
			contentType = "output_text"
		}
		wr.Input = append(wr.Input, responsesWireInput{
			Type:    "message",
			Role:    msg.Role,
			Content: []responsesWireContentPart{{Type: contentType, Text: messageContentToText(msg.Content)}},
		})
	}
	if len(instructions) > 0 {
		wr.Instructions = strings.Join(instructions, "\n\n")
	}

	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, responsesWireTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	wr.ToolChoice = req.ToolChoice

	return wr
}

func messageContentToText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []provider.ContentPart:
		var sb strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		return ""
	}
}

// --- Conversion: Responses wire response -> internal ---

func responsesWireToOpenAI(resp *responsesWireResponse, model string, headers http.Header) *provider.CompletionResponse {
	out := &provider.CompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Headers: headers,
	}

	msg := &provider.Message{Role: "assistant"}
	var textBuf strings.Builder
	hasToolCalls := false

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					textBuf.WriteString(c.Text)
				}
			}
		case "function_call":
			hasToolCalls = true
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: provider.FunctionCall{
					Name:      item.Name,
					Arguments: args,
				},
			})
		}
	}
	msg.Content = textBuf.String()

	finish := "stop"
	if hasToolCalls {
		finish = "tool_calls"
	}
	if resp.Status == "incomplete" {
		finish = "length"
	}

	out.Choices = []provider.Choice{{Index: 0, Message: msg, FinishReason: &finish}}

	if resp.Usage != nil {
		out.Usage = &provider.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
		if cached := resp.Usage.InputTokensDetails.CachedTokens; cached > 0 {
			// This API reports no cache-creation counter, so the write side stays 0.
			out.Usage.SetCacheUsage(cached, 0)
		}
		if reasoning := resp.Usage.OutputTokensDetails.ReasoningTokens; reasoning > 0 {
			out.Usage.CompletionTokensDetails = &provider.CompletionTokensDetails{ReasoningTokens: reasoning}
		}
	}

	return out
}

// --- Streaming: Responses wire SSE -> synthetic OpenAI chat.completion.chunk ---

type streamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
}

func (r *streamReader) Next() ([]byte, error) {
	for {
		line, err := sse.ReadLine(r.reader)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading chatgpt_codex stream: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data: "))

		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
			Response struct {
				Status string              `json:"status"`
				Usage  *responsesWireUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.created":
			chunk := r.makeRoleChunk()
			result, _ := json.Marshal(chunk)
			return result, nil

		case "response.output_text.delta":
			if event.Delta == "" {
				continue
			}
			chunk := r.makeChunk(event.Delta, nil, nil)
			result, _ := json.Marshal(chunk)
			return result, nil

		case "response.output_item.added":
			if event.Item.Type == "function_call" {
				tc := &provider.ToolCall{
					ID:   event.Item.CallID,
					Type: "function",
					Function: provider.FunctionCall{
						Name: event.Item.Name,
					},
				}
				chunk := r.makeChunk("", nil, tc)
				result, _ := json.Marshal(chunk)
				return result, nil
			}
			continue

		case "response.function_call_arguments.delta":
			if event.Delta == "" {
				continue
			}
			chunk := r.makeToolArgChunk(event.Delta)
			result, _ := json.Marshal(chunk)
			return result, nil

		case "response.completed", "response.incomplete", "response.failed":
			reason := "stop"
			if event.Response.Status == "incomplete" {
				reason = "length"
			}
			chunk := r.makeChunk("", &reason, nil)
			result, _ := json.Marshal(chunk)
			return result, nil

		default:
			// response.in_progress, response.output_item.done,
			// response.content_part.*, response.output_text.done, etc. carry
			// no new information the OpenAI chat-completions chunk shape
			// needs to represent.
			continue
		}
	}
}

func (r *streamReader) makeRoleChunk() interface{} {
	type delta struct {
		Role string `json:"role"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	return map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.model,
		"choices": []choice{{Index: 0, Delta: delta{Role: "assistant"}}},
	}
}

func (r *streamReader) makeChunk(text string, finishReason *string, toolCall *provider.ToolCall) interface{} {
	type delta struct {
		Content   string              `json:"content,omitempty"`
		ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}

	d := delta{}
	if text != "" {
		d.Content = text
	}
	if toolCall != nil {
		d.ToolCalls = []provider.ToolCall{*toolCall}
	}

	return map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.model,
		"choices": []choice{{Index: 0, Delta: d, FinishReason: finishReason}},
	}
}

func (r *streamReader) makeToolArgChunk(partialJSON string) interface{} {
	type funcDelta struct {
		Arguments string `json:"arguments"`
	}
	type tcDelta struct {
		Index    int       `json:"index"`
		Function funcDelta `json:"function"`
	}
	type delta struct {
		ToolCalls []tcDelta `json:"tool_calls,omitempty"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}

	return map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.model,
		"choices": []choice{{Index: 0, Delta: delta{
			ToolCalls: []tcDelta{{Index: 0, Function: funcDelta{Arguments: partialJSON}}},
		}}},
	}
}

func (r *streamReader) Close() error         { return r.body.Close() }
func (r *streamReader) Headers() http.Header { return r.headers }
