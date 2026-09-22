package vertex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- Anthropic Messages API types (for Claude via Vertex) ---

type anthropicRequest struct {
	Model    string             `json:"-"` // Vertex uses model in URL, not body
	Messages []anthropicMessage `json:"messages"`
	// System holds either a plain string or a []anthropicSystemBlock, the
	// latter when a prompt-cache breakpoint needs a block to sit on.
	System        interface{}     `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthropicTool `json:"tools,omitempty"`
	// Vertex AI requires this header field
	AnthropicVersion string `json:"anthropic_version"`
}

type anthropicSystemBlock struct {
	Type         string      `json:"type"`
	Text         string      `json:"text"`
	CacheControl interface{} `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []anthropicContentBlock
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

// --- Anthropic response types ---

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
	// Anthropic-on-Vertex reports cache tokens as siblings of input_tokens,
	// NOT as a subset of it. Dropping them undercounts billable input on
	// every cached request; see provider.Usage for the normalization contract.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// toProviderUsage normalizes the split counters into the inclusive internal
// model: PromptTokens covers uncached + cache read + cache creation.
func (u *anthropicUsage) toProviderUsage() *provider.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := &provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
	out.SetCacheUsage(u.CacheReadInputTokens, u.CacheCreationInputTokens)
	return out
}

// convertToAnthropic transforms an OpenAI request to Anthropic Messages API format.
func convertToAnthropic(req *provider.CompletionRequest) *anthropicRequest {
	ar := &anthropicRequest{
		Model:            req.Model,
		AnthropicVersion: "vertex-2023-10-16",
		MaxTokens:        4096, // Anthropic requires max_tokens
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

	// Extract system message and convert messages
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			if text := extractTextContent(msg.Content); text != "" {
				if msg.CacheControl != nil {
					// A breakpoint needs a block to sit on, so plain text is promoted.
					ar.System = []anthropicSystemBlock{{Type: "text", Text: text, CacheControl: msg.CacheControl}}
				} else {
					ar.System = text
				}
			}
		case "user":
			ar.Messages = append(ar.Messages, anthropicMessage{
				Role:    "user",
				Content: applyCacheControl(convertContentToAnthropic(msg), msg.CacheControl),
			})
		case "assistant":
			am := convertAssistantToAnthropic(msg)
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

	// Tools
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
// somewhere to sit. Without it Claude re-reads the whole prefix at full price
// on every turn instead of about a tenth of it.
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

func convertContentToAnthropic(msg provider.Message) interface{} {
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
					blocks = append(blocks, anthropicContentBlock{
						Type:         "text",
						Text:         text,
						CacheControl: provider.PartCacheControl(m),
					})
				case "image_url":
					if imgURL, ok := m["image_url"].(map[string]interface{}); ok {
						url, _ := imgURL["url"].(string)
						if mime, data, ok := parseDataURI(url); ok {
							blocks = append(blocks, anthropicContentBlock{
								Type: "image",
								Source: &anthropicSource{
									Type:      "base64",
									MediaType: mime,
									Data:      data,
								},
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

func convertAssistantToAnthropic(msg provider.Message) anthropicMessage {
	if len(msg.ToolCalls) > 0 {
		var blocks []anthropicContentBlock
		// Add text content if present
		if text := extractTextContent(msg.Content); text != "" {
			blocks = append(blocks, anthropicContentBlock{
				Type: "text",
				Text: text,
			})
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

	return anthropicMessage{
		Role:    "assistant",
		Content: extractTextContent(msg.Content),
	}
}

// anthropicToOpenAI converts an Anthropic response to OpenAI format.
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
		combined := ""
		for _, t := range textParts {
			combined += t
		}
		msg.Content = combined
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	finishReason := mapAnthropicStopReason(resp.StopReason)
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

func mapAnthropicStopReason(reason string) string {
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

// geminiStreamReader reads SSE chunks from Gemini streaming responses.
type geminiStreamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
	index   int
}

func (r *geminiStreamReader) Next() ([]byte, error) {
	for {
		line, err := sse.ReadLine(r.reader)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading gemini stream: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimPrefix(line, []byte("data: "))

			// Parse gemini chunk and convert to OpenAI format
			var chunk geminiResponse
			if err := json.Unmarshal(data, &chunk); err != nil {
				continue // skip malformed chunks
			}

			openaiChunk := r.convertChunk(&chunk)
			result, err := json.Marshal(openaiChunk)
			if err != nil {
				continue
			}
			r.index++
			return result, nil
		}
	}
}

func (r *geminiStreamReader) convertChunk(resp *geminiResponse) interface{} {
	type delta struct {
		Role      string              `json:"role,omitempty"`
		Content   string              `json:"content,omitempty"`
		ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	type chunk struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		Created int64    `json:"created"`
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
	}

	c := chunk{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   r.model,
	}

	for i, cand := range resp.Candidates {
		ch := choice{Index: i}
		if r.index == 0 {
			ch.Delta.Role = "assistant"
		}

		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				ch.Delta.Content += part.Text
			}
			if part.FunctionCall != nil {
				args, _ := json.Marshal(part.FunctionCall.Args)
				ch.Delta.ToolCalls = append(ch.Delta.ToolCalls, provider.ToolCall{
					ID:   fmt.Sprintf("call_%d_%s", i, part.FunctionCall.Name),
					Type: "function",
					Function: provider.FunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(args),
					},
				})
			}
		}

		if cand.FinishReason != "" && cand.FinishReason != "FINISH_REASON_UNSPECIFIED" {
			ch.FinishReason = mapGeminiFinishReason(cand.FinishReason)
		}

		c.Choices = append(c.Choices, ch)
	}

	return c
}

func (r *geminiStreamReader) Close() error         { return r.body.Close() }
func (r *geminiStreamReader) Headers() http.Header { return r.headers }

// anthropicStreamReader reads SSE chunks from Anthropic streaming responses (via Vertex).
type anthropicStreamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
	index   int
	// toolOrdinal maps an Anthropic content-block index to the position of that
	// tool call in the OpenAI-shaped delta. They are not the same number:
	// Anthropic counts every content block, text included, while OpenAI counts
	// only tool calls. Emitting 0 for all of them made a relay treat the second
	// parallel tool call as an update to the first and concatenate both
	// argument streams into one unparseable blob.
	toolOrdinal map[int]int
	nextTool    int
	// usage accumulates across the stream: message_start carries the input and
	// cache counters, message_delta the output ones. Reading them without
	// forwarding them billed every streamed turn off an estimate.
	usage anthropicUsage
}

// attachUsage adds what the upstream has reported so far to an outgoing chunk.
func (r *anthropicStreamReader) attachUsage(chunk interface{}) interface{} {
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
}

func (r *anthropicStreamReader) Next() ([]byte, error) {
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

		// Anthropic SSE format: "event: <type>\ndata: <json>\n"
		if bytes.HasPrefix(line, []byte("data: ")) {
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
				// message_start nests its counters under message, not at the
				// top level where every later event puts them.
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
				// Send role delta
				r.usage.mergeUsage(event.Message.Usage)
				chunk := r.attachUsage(r.makeRoleChunk())
				result, _ := json.Marshal(chunk)
				r.index++
				return result, nil

			case "message_delta":
				if event.Delta.StopReason != "" {
					reason := mapAnthropicStopReason(event.Delta.StopReason)
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
}

func (r *anthropicStreamReader) makeRoleChunk() interface{} {
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

func (r *anthropicStreamReader) makeChunk(text string, finishReason *string, toolCall *provider.ToolCall) interface{} {
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
func (r *anthropicStreamReader) makeToolStartChunk(tc *provider.ToolCall, toolIndex int) interface{} {
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

func (r *anthropicStreamReader) makeToolArgChunk(partialJSON string, toolIndex int) interface{} {
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

func (r *anthropicStreamReader) Close() error         { return r.body.Close() }
func (r *anthropicStreamReader) Headers() http.Header { return r.headers }
