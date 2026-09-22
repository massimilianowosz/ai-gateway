package bedrock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- Bedrock Converse API types ---

type converseRequest struct {
	Messages        []converseMessage `json:"messages"`
	System          []systemBlock     `json:"system,omitempty"`
	InferenceConfig *inferenceConfig  `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig       `json:"toolConfig,omitempty"`
}

type converseMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Text       string      `json:"text,omitempty"`
	Image      *imageBlock `json:"image,omitempty"`
	ToolUse    *toolUse    `json:"toolUse,omitempty"`
	ToolResult *toolResult `json:"toolResult,omitempty"`
	// CachePoint is Converse's spelling of a prompt-cache breakpoint: its own
	// block terminating the cacheable prefix, rather than a field on the block
	// before it as in the Anthropic API.
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type cachePoint struct {
	Type string `json:"type"`
}

// defaultCachePoint is the only breakpoint kind Converse defines.
func defaultCachePoint() *cachePoint { return &cachePoint{Type: "default"} }

type imageBlock struct {
	Format string      `json:"format"`
	Source imageSource `json:"source"`
}

type imageSource struct {
	Bytes string `json:"bytes"` // base64
}

type toolUse struct {
	ToolUseID string      `json:"toolUseId"`
	Name      string      `json:"name"`
	Input     interface{} `json:"input"`
}

type toolResult struct {
	ToolUseID string         `json:"toolUseId"`
	Content   []contentBlock `json:"content"`
	Status    string         `json:"status,omitempty"` // "success" or "error"
}

type systemBlock struct {
	Text       string      `json:"text,omitempty"`
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type inferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type toolConfig struct {
	Tools []toolSpec `json:"tools"`
}

type toolSpec struct {
	ToolSpec   *toolSpecDef `json:"toolSpec,omitempty"`
	CachePoint *cachePoint  `json:"cachePoint,omitempty"`
}

type toolSpecDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"inputSchema"`
}

// --- Bedrock Converse response types ---

type converseResponse struct {
	Output     converseOutput `json:"output"`
	StopReason string         `json:"stopReason"`
	Usage      *converseUsage `json:"usage,omitempty"`
}

type converseOutput struct {
	Message *converseMessage `json:"message,omitempty"`
}

type converseUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
	// Bedrock Converse reports cache tokens alongside inputTokens rather than
	// inside it, matching the Anthropic wire contract. Dropping them
	// undercounts billable input on every cached request.
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

// convertToConverse transforms an OpenAI request to Bedrock Converse format.
func convertToConverse(req *provider.CompletionRequest) *converseRequest {
	cr := &converseRequest{}

	// Extract system messages
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			text := extractTextContent(msg.Content)
			cr.System = append(cr.System, systemBlock{Text: text})
			if msg.CacheControl != nil {
				cr.System = append(cr.System, systemBlock{CachePoint: defaultCachePoint()})
			}
		case "user":
			cr.Messages = append(cr.Messages, converseMessage{
				Role:    "user",
				Content: withCachePoint(convertContentBlocks(msg), msg.CacheControl),
			})
		case "assistant":
			cm := convertAssistantBlocks(msg)
			cm.Content = withCachePoint(cm.Content, msg.CacheControl)
			cr.Messages = append(cr.Messages, cm)
		case "tool":
			cr.Messages = append(cr.Messages, converseMessage{
				Role: "user",
				Content: withCachePoint([]contentBlock{{
					ToolResult: &toolResult{
						ToolUseID: msg.ToolCallID,
						Content:   []contentBlock{{Text: extractTextContent(msg.Content)}},
						Status:    "success",
					},
				}}, msg.CacheControl),
			})
		}
	}

	// Inference config
	ic := &inferenceConfig{}
	hasConfig := false
	if req.MaxTokens != nil {
		ic.MaxTokens = req.MaxTokens
		hasConfig = true
	} else if req.MaxCompletionTokens != nil {
		ic.MaxTokens = req.MaxCompletionTokens
		hasConfig = true
	}
	if req.Temperature != nil {
		ic.Temperature = req.Temperature
		hasConfig = true
	}
	if req.TopP != nil {
		ic.TopP = req.TopP
		hasConfig = true
	}
	if req.Stop != nil {
		if stops := extractStopSequences(req.Stop); len(stops) > 0 {
			ic.StopSequences = stops
			hasConfig = true
		}
	}
	if hasConfig {
		cr.InferenceConfig = ic
	}

	// Tools
	if len(req.Tools) > 0 {
		tc := &toolConfig{}
		for _, t := range req.Tools {
			if t.Type == "function" {
				params := t.Function.Parameters
				if params == nil {
					params = map[string]interface{}{"type": "object"}
				}
				tc.Tools = append(tc.Tools, toolSpec{
					ToolSpec: &toolSpecDef{
						Name:        t.Function.Name,
						Description: t.Function.Description,
						InputSchema: map[string]interface{}{
							"json": params,
						},
					},
				})
				if t.CacheControl != nil {
					tc.Tools = append(tc.Tools, toolSpec{CachePoint: defaultCachePoint()})
				}
			}
		}
		if len(tc.Tools) > 0 {
			cr.ToolConfig = tc
		}
	}

	return cr
}

// withCachePoint terminates a message's cacheable prefix. Dropping the marker
// makes Bedrock re-read the whole prefix at full price on every turn instead
// of about a tenth of it.
func withCachePoint(blocks []contentBlock, cc interface{}) []contentBlock {
	if cc == nil {
		return blocks
	}
	return append(blocks, contentBlock{CachePoint: defaultCachePoint()})
}

func convertContentBlocks(msg provider.Message) []contentBlock {
	switch v := msg.Content.(type) {
	case string:
		return []contentBlock{{Text: v}}
	case []interface{}:
		var blocks []contentBlock
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				partType, _ := m["type"].(string)
				switch partType {
				case "text":
					text, _ := m["text"].(string)
					blocks = append(blocks, contentBlock{Text: text})
				case "image_url":
					if imgURL, ok := m["image_url"].(map[string]interface{}); ok {
						url, _ := imgURL["url"].(string)
						if format, data, ok := parseDataURI(url); ok {
							blocks = append(blocks, contentBlock{
								Image: &imageBlock{
									Format: format,
									Source: imageSource{Bytes: data},
								},
							})
						}
					}
				}
			}
		}
		if len(blocks) == 0 {
			return []contentBlock{{Text: fmt.Sprintf("%v", msg.Content)}}
		}
		return blocks
	default:
		return []contentBlock{{Text: fmt.Sprintf("%v", v)}}
	}
}

func convertAssistantBlocks(msg provider.Message) converseMessage {
	cm := converseMessage{Role: "assistant"}

	// Add text content if present and non-empty
	if msg.Content != nil {
		if text := extractTextContent(msg.Content); text != "" && text != "<nil>" {
			cm.Content = append(cm.Content, contentBlock{Text: text})
		}
	}

	// Add tool calls
	for _, tc := range msg.ToolCalls {
		var input interface{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		cm.Content = append(cm.Content, contentBlock{
			ToolUse: &toolUse{
				ToolUseID: tc.ID,
				Name:      tc.Function.Name,
				Input:     input,
			},
		})
	}

	if len(cm.Content) == 0 {
		cm.Content = []contentBlock{{Text: ""}}
	}

	return cm
}

// converseToOpenAI converts a Bedrock Converse response to OpenAI format.
func converseToOpenAI(resp *converseResponse, model string, headers http.Header) *provider.CompletionResponse {
	openaiResp := &provider.CompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Headers: headers,
	}

	if resp.Output.Message != nil {
		msg := &provider.Message{Role: "assistant"}
		var textParts []string
		var toolCalls []provider.ToolCall

		for _, block := range resp.Output.Message.Content {
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
			if block.ToolUse != nil {
				args, _ := json.Marshal(block.ToolUse.Input)
				toolCalls = append(toolCalls, provider.ToolCall{
					ID:   block.ToolUse.ToolUseID,
					Type: "function",
					Function: provider.FunctionCall{
						Name:      block.ToolUse.Name,
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

		finishReason := mapBedrockStopReason(resp.StopReason)
		openaiResp.Choices = []provider.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}}
	}

	if resp.Usage != nil {
		// Normalize to the inclusive internal model: PromptTokens covers
		// uncached + cache read + cache write. totalTokens from Bedrock is
		// recomputed rather than trusted, since it predates cache reporting
		// on some model families.
		prompt := resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheWriteInputTokens
		openaiResp.Usage = &provider.Usage{
			PromptTokens:     prompt,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      prompt + resp.Usage.OutputTokens,
		}
		openaiResp.Usage.SetCacheUsage(resp.Usage.CacheReadInputTokens, resp.Usage.CacheWriteInputTokens)
	}

	return openaiResp
}

func mapBedrockStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	case "content_filtered":
		return "content_filter"
	default:
		return "stop"
	}
}

// --- Streaming ---

// bedrockStreamReader reads the AWS EventStream binary format and converts to OpenAI chunks.
// AWS EventStream frame format:
//   - 4 bytes: total byte length (big-endian uint32)
//   - 4 bytes: headers byte length (big-endian uint32)
//   - 4 bytes: prelude CRC32
//   - N bytes: headers (binary-encoded)
//   - M bytes: payload (JSON)
//   - 4 bytes: message CRC32
type bedrockStreamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
	started bool
}

func (r *bedrockStreamReader) Next() ([]byte, error) {
	for {
		// Read a single EventStream frame
		eventType, payload, err := r.readFrame()
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading bedrock stream frame: %w", err)
		}

		// Parse JSON payload based on event type
		switch eventType {
		case "messageStart":
			var ev struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			r.started = true
			chunk := r.makeOpenAIChunk("", "assistant", nil, nil)
			result, _ := json.Marshal(chunk)
			return result, nil

		case "contentBlockDelta":
			var ev struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Delta             struct {
					Text    string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
				} `json:"delta"`
			}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			if ev.Delta.Text != "" {
				chunk := r.makeOpenAIChunk(ev.Delta.Text, "", nil, nil)
				result, _ := json.Marshal(chunk)
				return result, nil
			}
			if ev.Delta.ToolUse != nil {
				chunk := r.makeToolArgChunk(ev.Delta.ToolUse.Input)
				result, _ := json.Marshal(chunk)
				return result, nil
			}

		case "contentBlockStart":
			var ev struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Start             struct {
					ToolUse *struct {
						ToolUseID string `json:"toolUseId"`
						Name      string `json:"name"`
					} `json:"toolUse"`
				} `json:"start"`
			}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			if ev.Start.ToolUse != nil {
				tu := ev.Start.ToolUse
				tc := &provider.ToolCall{
					ID:   tu.ToolUseID,
					Type: "function",
					Function: provider.FunctionCall{
						Name: tu.Name,
					},
				}
				chunk := r.makeOpenAIChunk("", "", tc, nil)
				result, _ := json.Marshal(chunk)
				return result, nil
			}

		case "messageStop":
			var ev struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			reason := mapBedrockStopReason(ev.StopReason)
			chunk := r.makeOpenAIChunk("", "", nil, &reason)
			result, _ := json.Marshal(chunk)
			return result, nil

		case "metadata":
			// The frame that closes the stream is also the only one carrying
			// usage; discarding it billed every streamed turn off an estimate.
			var ev struct {
				Usage *converseUsage `json:"usage"`
			}
			if json.Unmarshal(payload, &ev) != nil || ev.Usage == nil {
				return nil, io.EOF
			}
			prompt := ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens + ev.Usage.CacheWriteInputTokens
			u := &provider.Usage{
				PromptTokens:     prompt,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      prompt + ev.Usage.OutputTokens,
			}
			u.SetCacheUsage(ev.Usage.CacheReadInputTokens, ev.Usage.CacheWriteInputTokens)
			result, _ := json.Marshal(map[string]interface{}{
				"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   r.model,
				"choices": []interface{}{},
				"usage":   u.WireMap(),
			})
			return result, nil

		case "contentBlockStop":
			// Ignore, no data needed
			continue

		default:
			continue
		}
	}
}

// readFrame reads a single AWS EventStream binary frame.
func (r *bedrockStreamReader) readFrame() (eventType string, payload []byte, err error) {
	// Read prelude: total_length (4) + headers_length (4) + prelude_crc (4) = 12 bytes
	prelude := make([]byte, 12)
	if _, err = io.ReadFull(r.reader, prelude); err != nil {
		return "", nil, err
	}

	totalLen := beUint32(prelude[0:4])
	headersLen := beUint32(prelude[4:8])
	// prelude CRC at prelude[8:12] — skip validation for performance

	// Sanity check to prevent huge allocations
	if totalLen > 1<<20 { // 1MB max frame
		return "", nil, fmt.Errorf("frame too large: %d bytes", totalLen)
	}

	// Read the rest of the frame: headers + payload + message_crc
	// remaining = totalLen - 12 (prelude already read)
	remaining := int(totalLen) - 12
	if remaining < 4 { // at minimum need 4 bytes for message CRC
		return "", nil, fmt.Errorf("invalid frame: remaining %d bytes", remaining)
	}

	frameBuf := make([]byte, remaining)
	if _, err = io.ReadFull(r.reader, frameBuf); err != nil {
		return "", nil, err
	}

	// Parse headers
	headersBytes := frameBuf[:headersLen]
	eventType = parseEventType(headersBytes)

	// Payload is between headers and message CRC (last 4 bytes)
	payloadLen := remaining - int(headersLen) - 4 // subtract message CRC
	if payloadLen > 0 {
		payload = frameBuf[headersLen : headersLen+uint32(payloadLen)]
	}

	return eventType, payload, nil
}

// parseEventType extracts the :event-type header value from binary headers.
func parseEventType(headers []byte) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		// Header name length (1 byte)
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen

		// Header value type (1 byte)
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		// For type 7 (string): 2 bytes length + value
		if valueType == 7 {
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(beUint16(headers[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen

			if name == ":event-type" {
				return value
			}
		} else {
			// Skip other types (we only care about strings)
			// Type 0 (bool true): 0 bytes
			// Type 1 (bool false): 0 bytes
			// Type 2 (byte): 1 byte
			// Type 3 (short): 2 bytes
			// Type 4 (int): 4 bytes
			// Type 5 (long): 8 bytes
			// Type 6 (bytes): 2 bytes len + value
			// Type 8 (timestamp): 8 bytes
			// Type 9 (uuid): 16 bytes
			switch valueType {
			case 0, 1:
				// no additional bytes
			case 2:
				offset++
			case 3:
				offset += 2
			case 4:
				offset += 4
			case 5, 8:
				offset += 8
			case 6:
				if offset+2 > len(headers) {
					return ""
				}
				vl := int(beUint16(headers[offset : offset+2]))
				offset += 2 + vl
			case 9:
				offset += 16
			default:
				return ""
			}
		}
	}
	return ""
}

func beUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func beUint16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}

func (r *bedrockStreamReader) makeOpenAIChunk(text, role string, toolCall *provider.ToolCall, finishReason *string) interface{} {
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

	d := delta{Role: role, Content: text}
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

func (r *bedrockStreamReader) makeToolArgChunk(input string) interface{} {
	// For tool argument streaming, we send partial function arguments
	type funcDelta struct {
		Arguments string `json:"arguments"`
	}
	type tcDelta struct {
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
			ToolCalls: []tcDelta{{Function: funcDelta{Arguments: input}}},
		}}},
	}
}

func (r *bedrockStreamReader) Close() error         { return r.body.Close() }
func (r *bedrockStreamReader) Headers() http.Header { return r.headers }

// --- Helpers ---

func extractTextContent(content interface{}) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, ok := m["text"].(string); ok {
						return text
					}
				}
			}
		}
		return ""
	}
	return fmt.Sprintf("%v", content)
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

func parseDataURI(uri string) (format, data string, ok bool) {
	if len(uri) < 5 || uri[:5] != "data:" {
		return "", "", false
	}
	rest := uri[5:]
	semicol := -1
	for i, c := range rest {
		if c == ';' {
			semicol = i
			break
		}
	}
	if semicol < 0 {
		return "", "", false
	}
	mime := rest[:semicol]
	// Extract format from mime type (e.g., "image/png" -> "png")
	parts := splitMime(mime)
	if len(parts) < 2 {
		return "", "", false
	}
	format = parts[1]

	dataStart := semicol + 8 // len(";base64,")
	if dataStart >= len(rest) {
		return "", "", false
	}
	return format, rest[dataStart:], true
}

func splitMime(mime string) []string {
	idx := -1
	for i, c := range mime {
		if c == '/' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{mime}
	}
	return []string{mime[:idx], mime[idx+1:]}
}
