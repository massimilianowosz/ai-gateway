package anthropic

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

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	defaultVersion   = "2023-06-01"
	defaultMaxTokens = 4096

	// defaultClaudeCodeVersion is the fallback Claude Code CLI version used in
	// the User-Agent and billing system block when the caller does not identify
	// itself. Anthropic's OAuth (Claude Pro/Max subscription) requests are
	// billing-routed on this identity, not on a pay-per-token API key.
	defaultClaudeCodeVersion = "2.1.75"

	claudeCodeIdentityBlock = "You are Claude Code, Anthropic's official CLI for Claude."
)

// claudeCodeVersion returns the version the caller reported, falling back to a
// known-good one. A version the gateway pins ages out of support while the
// user's CLI keeps moving, so the caller's own is preferred.
func claudeCodeVersion(ctx context.Context) string {
	if id, ok := provider.ClientIdentityFrom(ctx); ok && id.Version != "" {
		return id.Version
	}
	return defaultClaudeCodeVersion
}

// claudeCodeBillingBlock MUST be the first system block on OAuth pass-through
// requests: Anthropic uses it to route usage against the Claude Pro/Max
// subscription instead of treating the request as unauthorized/anomalous
// traffic (observed as spurious HTTP 429s without it).
func claudeCodeBillingBlock(version string) string {
	return "x-anthropic-billing-header: cc_version=" + version + "; cc_entrypoint=sdk-cli;"
}

func newStreamSafeClient() *http.Client {
	transport := perf.NewHighPerfTransport()
	transport.ResponseHeaderTimeout = 600 * time.Second
	return &http.Client{
		Timeout:   0,
		Transport: transport,
	}
}

// Provider implements the Provider interface for direct Anthropic API.
type Provider struct {
	apiKey    string
	baseURL   string
	version   string
	modelID   string
	client    *http.Client
	azureAuth bool // use Authorization: Bearer instead of x-api-key
}

// New creates a new direct Anthropic provider.
func New(apiKey, modelID string) *Provider {
	return &Provider{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		version: defaultVersion,
		modelID: modelID,
		client:  newStreamSafeClient(),
	}
}

// NewAzure creates an Anthropic provider for Azure AI Foundry endpoints.
// Azure uses Authorization: Bearer auth and the x-ms-model-mesh-model-name header.
func NewAzure(apiKey, baseURL, modelID string) *Provider {
	return &Provider{
		apiKey: apiKey,
		// Trimmed like every other provider does: a configured api_base with a
		// trailing slash otherwise yields a doubled separator that some
		// gateways answer with a 404.
		baseURL:   strings.TrimRight(baseURL, "/"),
		version:   defaultVersion,
		modelID:   modelID,
		client:    newStreamSafeClient(),
		azureAuth: true,
	}
}

func (p *Provider) Name() string { return "anthropic" }

// Complete sends a non-streaming chat completion request.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	oauthPassthrough := !p.azureAuth && auth.UpstreamTokenForProvider(ctx, p.Name()) != ""
	ar := convertToAnthropic(ctx, req, p.modelID, oauthPassthrough)
	ar.Stream = false

	body, err := json.Marshal(ar)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: creating request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseErrorResponse(resp)
	}

	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w", err)
	}

	return anthropicToOpenAI(&result, p.modelID, resp.Header), nil
}

// Stream sends a streaming chat completion request.
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	oauthPassthrough := !p.azureAuth && auth.UpstreamTokenForProvider(ctx, p.Name()) != ""
	ar := convertToAnthropic(ctx, req, p.modelID, oauthPassthrough)
	ar.Stream = true

	body, err := json.Marshal(ar)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: creating request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, parseErrorResponse(resp)
	}

	return &streamReader{
		reader:  bufio.NewReader(resp.Body),
		body:    resp.Body,
		headers: resp.Header,
		model:   p.modelID,
	}, nil
}

func (p *Provider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.azureAuth {
		// Azure AI Foundry uses Bearer auth and model name header
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		req.Header.Set("x-ms-model-mesh-model-name", p.modelID)
	} else if token := auth.UpstreamTokenForProvider(req.Context(), p.Name()); token != "" {
		// Claude Pro/Max OAuth access tokens are Bearer tokens, not API keys.
		// They must be sent as Authorization: Bearer with the oauth beta flag,
		// never as x-api-key (Anthropic rejects OAuth tokens on that header).
		// The identity headers (User-Agent/x-app) and browser-access flag are
		// required to make the request look like the real Claude Code CLI;
		// without them Anthropic has been observed returning spurious 429s.
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
		req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
		req.Header.Set("User-Agent", "claude-cli/"+claudeCodeVersion(req.Context()))
		req.Header.Set("x-app", "cli")
	} else {
		req.Header.Set("x-api-key", p.apiKey)
	}
	req.Header.Set("anthropic-version", p.version)
	req.Header.Set("Accept", "application/json")
}

// --- Request/Response types ---

type anthropicRequest struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`
	// System holds either a plain string or a []anthropicSystemBlock (the
	// latter is required for OAuth pass-through so the billing/identity
	// blocks can precede the user-supplied system prompt).
	System        interface{}     `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthropicTool `json:"tools,omitempty"`
}

type anthropicSystemBlock struct {
	Type         string      `json:"type"`
	Text         string      `json:"text"`
	CacheControl interface{} `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type anthropicContentBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	ID           string           `json:"id,omitempty"`
	Name         string           `json:"name,omitempty"`
	Input        interface{}      `json:"input,omitempty"`
	ToolUseID    string           `json:"tool_use_id,omitempty"`
	Content      string           `json:"content,omitempty"`
	Source       *anthropicSource `json:"source,omitempty"`
	CacheControl interface{}      `json:"cache_control,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name         string      `json:"name"`
	Description  string      `json:"description,omitempty"`
	InputSchema  interface{} `json:"input_schema"`
	CacheControl interface{} `json:"cache_control,omitempty"`
}

type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        *anthropicUsage         `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Anthropic reports cache tokens as siblings of input_tokens, NOT as a
	// subset of it. Dropping them undercounts billable input on every cached
	// request; see provider.Usage for the normalization contract.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// OutputTokensDetails.ThinkingTokens is the subset of output_tokens the
	// model spent reasoning. Unlike the cache counters this one *is* a subset,
	// so it is reported alongside rather than added.
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// toProviderUsage normalizes Anthropic's split counters into the inclusive
// internal model: PromptTokens covers uncached + cache read + cache creation.
func (u *anthropicUsage) toProviderUsage() *provider.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := &provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
	out.SetCacheUsage(u.CacheReadInputTokens, u.CacheCreationInputTokens)
	// Anthropic calls it thinking, OpenAI calls it reasoning; they measure the
	// same thing, so they land in the same field and stay comparable.
	if thinking := u.OutputTokensDetails.ThinkingTokens; thinking > 0 {
		out.CompletionTokensDetails = &provider.CompletionTokensDetails{ReasoningTokens: thinking}
	}
	return out
}

// --- Conversion ---

func convertToAnthropic(ctx context.Context, req *provider.CompletionRequest, modelID string, oauthPassthrough bool) *anthropicRequest {
	ar := &anthropicRequest{
		Model:     modelID,
		MaxTokens: defaultMaxTokens,
	}

	if req.MaxTokens != nil {
		ar.MaxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		ar.MaxTokens = *req.MaxCompletionTokens
	}
	if req.Temperature != nil {
		ar.Temperature = req.Temperature
	}
	if req.TopP != nil {
		ar.TopP = req.TopP
	}
	if req.Stop != nil {
		ar.StopSequences = extractStopSequences(req.Stop)
	}

	var systemText string
	var systemCache interface{}
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			systemText = extractTextContent(msg.Content)
			systemCache = msg.CacheControl
		case "user":
			ar.Messages = append(ar.Messages, anthropicMessage{
				Role:    "user",
				Content: applyCacheControl(convertContent(msg), msg.CacheControl),
			})
		case "assistant":
			am := convertAssistant(msg)
			am.Content = applyCacheControl(am.Content, msg.CacheControl)
			ar.Messages = append(ar.Messages, am)
		case "tool":
			ar.Messages = append(ar.Messages, anthropicMessage{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type:         "tool_result",
					ToolUseID:    msg.ToolCallID,
					Content:      extractTextContent(msg.Content),
					CacheControl: msg.CacheControl,
				}},
			})
		}
	}

	if oauthPassthrough {
		// Anthropic requires these two blocks first (in this order) on
		// OAuth pass-through requests to route usage against the Claude
		// Pro/Max subscription and identify the caller as Claude Code.
		blocks := []anthropicSystemBlock{
			{Type: "text", Text: claudeCodeBillingBlock(claudeCodeVersion(ctx))},
			{Type: "text", Text: claudeCodeIdentityBlock},
		}
		if systemText != "" {
			blocks = append(blocks, anthropicSystemBlock{Type: "text", Text: systemText, CacheControl: systemCache})
		}
		ar.System = blocks
	} else if systemText != "" {
		if systemCache != nil {
			// A breakpoint needs a block to sit on, so plain text is promoted.
			ar.System = []anthropicSystemBlock{{Type: "text", Text: systemText, CacheControl: systemCache}}
		} else {
			ar.System = systemText
		}
	}

	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.Type == "function" {
				schema := t.Function.Parameters
				if schema == nil {
					schema = map[string]interface{}{"type": "object"}
				}
				ar.Tools = append(ar.Tools, anthropicTool{
					Name:         t.Function.Name,
					Description:  t.Function.Description,
					InputSchema:  schema,
					CacheControl: t.CacheControl,
				})
			}
		}
	}

	return ar
}

// applyCacheControl re-attaches a prompt-cache breakpoint to the last content
// block, promoting plain string content to block form so the marker has
// somewhere to sit. Without it Anthropic re-reads the whole prefix at full
// price on every turn instead of about a tenth of it.
func applyCacheControl(content interface{}, cc interface{}) interface{} {
	if cc == nil {
		return content
	}
	switch v := content.(type) {
	case string:
		if v == "" {
			return content
		}
		return []anthropicContentBlock{{Type: "text", Text: v, CacheControl: cc}}
	case []anthropicContentBlock:
		if len(v) == 0 {
			return content
		}
		v[len(v)-1].CacheControl = cc
		return v
	}
	return content
}

func convertContent(msg provider.Message) interface{} {
	switch v := msg.Content.(type) {
	case string:
		return v
	case []interface{}:
		var blocks []anthropicContentBlock
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				partType, _ := m["type"].(string)
				switch partType {
				case "text":
					text, _ := m["text"].(string)
					blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text, CacheControl: provider.PartCacheControl(m)})
				case "image_url":
					if imgURL, ok := m["image_url"].(map[string]interface{}); ok {
						url, _ := imgURL["url"].(string)
						if mime, data, ok := parseDataURI(url); ok {
							blocks = append(blocks, anthropicContentBlock{
								Type:         "image",
								Source:       &anthropicSource{Type: "base64", MediaType: mime, Data: data},
								CacheControl: provider.PartCacheControl(m),
							})
						}
					}
				}
			}
		}
		return blocks
	default:
		return fmt.Sprintf("%v", v)
	}
}

func convertAssistant(msg provider.Message) anthropicMessage {
	if len(msg.ToolCalls) > 0 {
		var blocks []anthropicContentBlock
		if text := extractTextContent(msg.Content); text != "" {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
		}
		for _, tc := range msg.ToolCalls {
			var input interface{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			blocks = append(blocks, anthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		return anthropicMessage{Role: "assistant", Content: blocks}
	}
	return anthropicMessage{Role: "assistant", Content: extractTextContent(msg.Content)}
}

func anthropicToOpenAI(resp *anthropicResponse, model string, headers http.Header) *provider.CompletionResponse {
	openaiResp := &provider.CompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Headers: headers,
	}

	msg := &provider.Message{Role: "assistant"}
	var textParts []string
	var toolCalls []provider.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, provider.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: provider.FunctionCall{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}

	if len(textParts) > 0 {
		combined := strings.Join(textParts, "")
		msg.Content = combined
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	finishReason := mapStopReason(resp.StopReason)
	openaiResp.Choices = []provider.Choice{{
		Index:        0,
		Message:      msg,
		FinishReason: &finishReason,
	}}

	if resp.Usage != nil {
		openaiResp.Usage = resp.Usage.toProviderUsage()
	}

	return openaiResp
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

// --- Streaming ---

type streamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
	index   int
	// toolOrdinal maps an Anthropic content-block index to the position of
	// that tool call in the OpenAI-shaped delta. They are not the same number:
	// Anthropic counts every content block, text included, while OpenAI counts
	// only tool calls. Emitting 0 for all of them made a relay treat the second
	// parallel tool call as an update to the first and concatenate both
	// argument streams into one unparseable blob.
	toolOrdinal map[int]int
	nextTool    int
	// usage accumulates across the stream: message_start carries the input and
	// cache counters, message_delta the output ones. Reading them without
	// forwarding them billed every streamed turn off an estimate and told the
	// client it had spent no input at all.
	usage anthropicUsage
}

// attachUsage adds what the upstream has reported so far to an outgoing chunk.
func (r *streamReader) attachUsage(chunk interface{}) interface{} {
	m, ok := chunk.(map[string]interface{})
	if !ok {
		return chunk
	}
	m["usage"] = r.usage.toProviderUsage().WireMap()
	return m
}

// mergeUsage keeps only the counters an event actually reported: Anthropic
// omits the input side on message_delta and the output side on message_start.
func (u *anthropicUsage) mergeUsage(in *anthropicUsage) {
	if in == nil {
		return
	}
	if in.InputTokens > 0 {
		u.InputTokens = in.InputTokens
	}
	if in.OutputTokens > 0 {
		u.OutputTokens = in.OutputTokens
	}
	if in.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = in.CacheReadInputTokens
	}
	if in.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = in.CacheCreationInputTokens
	}
	// The thinking breakdown arrives only on the final message_delta, so it
	// has to survive the merge like the rest.
	if in.OutputTokensDetails.ThinkingTokens > 0 {
		u.OutputTokensDetails.ThinkingTokens = in.OutputTokensDetails.ThinkingTokens
	}
}

func (r *streamReader) Next() ([]byte, error) {
	for {
		line, err := sse.ReadLine(r.reader)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading anthropic stream: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}

		data := bytes.TrimPrefix(line, []byte("data: "))

		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock struct {
				Type  string      `json:"type"`
				ID    string      `json:"id"`
				Name  string      `json:"name"`
				Input interface{} `json:"input"`
			} `json:"content_block"`
			Usage *anthropicUsage `json:"usage"`
			// message_start nests its counters under message, not at the top
			// level where every later event puts them.
			Message struct {
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			continue
		}
		r.usage.mergeUsage(event.Usage)

		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
				chunk := r.makeToolArgChunk(event.Delta.PartialJSON, r.toolOrdinal[event.Index])
				result, _ := json.Marshal(chunk)
				r.index++
				return result, nil
			}
			if event.Delta.Text != "" {
				chunk := r.makeChunk(event.Delta.Text, nil, nil)
				result, _ := json.Marshal(chunk)
				r.index++
				return result, nil
			}
			continue

		case "message_start":
			r.usage.mergeUsage(event.Message.Usage)
			chunk := r.attachUsage(r.makeRoleChunk())
			result, _ := json.Marshal(chunk)
			r.index++
			return result, nil

		case "message_delta":
			if event.Delta.StopReason != "" {
				reason := mapStopReason(event.Delta.StopReason)
				chunk := r.attachUsage(r.makeChunk("", &reason, nil))
				result, _ := json.Marshal(chunk)
				r.index++
				return result, nil
			}

		case "message_stop":
			return nil, io.EOF

		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				if r.toolOrdinal == nil {
					r.toolOrdinal = map[int]int{}
				}
				ordinal := r.nextTool
				r.toolOrdinal[event.Index] = ordinal
				r.nextTool++
				tc := &provider.ToolCall{
					ID:   event.ContentBlock.ID,
					Type: "function",
					Function: provider.FunctionCall{
						Name: event.ContentBlock.Name,
					},
				}
				chunk := r.makeToolStartChunk(tc, ordinal)
				result, _ := json.Marshal(chunk)
				r.index++
				return result, nil
			}
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

// makeToolStartChunk opens a tool call in the OpenAI delta shape, carrying the
// call's ordinal. provider.ToolCall has no index field of its own, and the
// relay needs one to tell parallel calls apart.
func (r *streamReader) makeToolStartChunk(tc *provider.ToolCall, toolIndex int) interface{} {
	type funcDelta struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	}
	type tcDelta struct {
		Index    int       `json:"index"`
		ID       string    `json:"id,omitempty"`
		Type     string    `json:"type,omitempty"`
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
		"choices": []choice{{Index: 0, Delta: delta{ToolCalls: []tcDelta{{
			Index:    toolIndex,
			ID:       tc.ID,
			Type:     tc.Type,
			Function: funcDelta{Name: tc.Function.Name},
		}}}}},
	}
}

func (r *streamReader) makeToolArgChunk(partialJSON string, toolIndex int) interface{} {
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
			ToolCalls: []tcDelta{{Index: toolIndex, Function: funcDelta{Arguments: partialJSON}}},
		}}},
	}
}

func (r *streamReader) Close() error         { return r.body.Close() }
func (r *streamReader) Headers() http.Header { return r.headers }

// --- Helpers ---

func extractTextContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, _ := m["text"].(string); text != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}

func extractStopSequences(stop interface{}) []string {
	switch v := stop.(type) {
	case string:
		return []string{v}
	case []interface{}:
		var result []string
		for _, s := range v {
			if str, ok := s.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}
	return nil
}

func parseDataURI(uri string) (mime, data string, ok bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false
	}
	uri = strings.TrimPrefix(uri, "data:")
	parts := strings.SplitN(uri, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	mime = strings.TrimSuffix(parts[0], ";base64")
	return mime, parts[1], true
}

func parseErrorResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var errResp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return &provider.UpstreamError{
			StatusCode: resp.StatusCode,
			Message:    errResp.Error.Message,
		}
	}
	return &provider.UpstreamError{
		StatusCode: resp.StatusCode,
		Message:    string(body),
	}
}
