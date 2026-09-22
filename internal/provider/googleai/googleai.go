package googleai

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

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// defaultHeaderTimeout bounds the wait for response headers, not the
	// generation: this provider streams, and a whole-request deadline would
	// truncate a long answer mid-stream.
	defaultHeaderTimeout = 60 * time.Second
)

// Provider implements the Provider interface for Google AI Studio (Gemini direct).
type Provider struct {
	apiKey  string
	baseURL string
	modelID string
	client  *http.Client
}

// New creates a new Google AI Studio provider.
func New(apiKey, modelID string) *Provider {
	return &Provider{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		modelID: modelID,
		client:  perf.NewStreamingClient(defaultHeaderTimeout),
	}
}

func (p *Provider) Name() string { return "google_ai" }

// Complete sends a non-streaming chat completion request.
func (p *Provider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	geminiReq := convertToGemini(req)

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", p.baseURL, p.modelID, p.apiKey)

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("google_ai: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("google_ai: creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("google_ai: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseErrorResponse(resp)
	}

	var result geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("google_ai: decoding response: %w", err)
	}

	return geminiToOpenAI(&result, p.modelID, resp.Header), nil
}

// Stream sends a streaming chat completion request.
func (p *Provider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	geminiReq := convertToGemini(req)

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", p.baseURL, p.modelID, p.apiKey)

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("google_ai: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("google_ai: creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("google_ai: stream request failed: %w", err)
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

// --- Gemini request/response types ---

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text         string        `json:"text,omitempty"`
	InlineData   *inlineData   `json:"inlineData,omitempty"`
	FunctionCall *functionCall `json:"functionCall,omitempty"`
	FunctionResp *functionResp `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type functionResp struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations,omitempty"`
}

type geminiFuncDecl struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// --- Conversion ---

func convertToGemini(req *provider.CompletionRequest) *geminiRequest {
	gr := &geminiRequest{}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			text := extractTextContent(msg.Content)
			gr.SystemInstruction = &geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: text}},
			}
		case "assistant":
			parts := messageToGeminiParts(msg)
			gr.Contents = append(gr.Contents, geminiContent{Role: "model", Parts: parts})
		case "tool":
			var result map[string]interface{}
			text := extractTextContent(msg.Content)
			_ = json.Unmarshal([]byte(text), &result)
			if result == nil {
				result = map[string]interface{}{"result": text}
			}
			gr.Contents = append(gr.Contents, geminiContent{
				Role: "function",
				Parts: []geminiPart{{
					FunctionResp: &functionResp{Name: msg.Name, Response: result},
				}},
			})
		default: // "user"
			parts := messageToGeminiParts(msg)
			gr.Contents = append(gr.Contents, geminiContent{Role: "user", Parts: parts})
		}
	}

	gc := &geminiGenerationConfig{}
	hasConfig := false
	if req.Temperature != nil {
		gc.Temperature = req.Temperature
		hasConfig = true
	}
	if req.TopP != nil {
		gc.TopP = req.TopP
		hasConfig = true
	}
	if req.MaxTokens != nil {
		gc.MaxOutputTokens = req.MaxTokens
		hasConfig = true
	} else if req.MaxCompletionTokens != nil {
		gc.MaxOutputTokens = req.MaxCompletionTokens
		hasConfig = true
	}
	if req.Stop != nil {
		if stops := extractStopSequences(req.Stop); len(stops) > 0 {
			gc.StopSequences = stops
			hasConfig = true
		}
	}
	if req.N != nil {
		gc.CandidateCount = req.N
		hasConfig = true
	}
	if hasConfig {
		gr.GenerationConfig = gc
	}

	if len(req.Tools) > 0 {
		var decls []geminiFuncDecl
		for _, t := range req.Tools {
			if t.Type == "function" {
				decls = append(decls, geminiFuncDecl{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				})
			}
		}
		if len(decls) > 0 {
			gr.Tools = []geminiTool{{FunctionDeclarations: decls}}
		}
	}

	return gr
}

func messageToGeminiParts(msg provider.Message) []geminiPart {
	if len(msg.ToolCalls) > 0 {
		var parts []geminiPart
		for _, tc := range msg.ToolCalls {
			var args map[string]interface{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			parts = append(parts, geminiPart{
				FunctionCall: &functionCall{Name: tc.Function.Name, Args: args},
			})
		}
		return parts
	}

	switch v := msg.Content.(type) {
	case string:
		return []geminiPart{{Text: v}}
	case []interface{}:
		var parts []geminiPart
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				partType, _ := m["type"].(string)
				switch partType {
				case "text":
					text, _ := m["text"].(string)
					parts = append(parts, geminiPart{Text: text})
				case "image_url":
					if imgURL, ok := m["image_url"].(map[string]interface{}); ok {
						url, _ := imgURL["url"].(string)
						if mime, data, ok := parseDataURI(url); ok {
							parts = append(parts, geminiPart{
								InlineData: &inlineData{MimeType: mime, Data: data},
							})
						}
					}
				}
			}
		}
		return parts
	default:
		return []geminiPart{{Text: fmt.Sprintf("%v", v)}}
	}
}

func geminiToOpenAI(resp *geminiResponse, model string, headers http.Header) *provider.CompletionResponse {
	openaiResp := &provider.CompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Headers: headers,
	}

	for i, cand := range resp.Candidates {
		choice := provider.Choice{
			Index:        i,
			FinishReason: mapGeminiFinishReason(cand.FinishReason),
		}

		msg := &provider.Message{Role: "assistant"}
		var toolCalls []provider.ToolCall
		var textParts []string
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				args, _ := json.Marshal(part.FunctionCall.Args)
				toolCalls = append(toolCalls, provider.ToolCall{
					ID:   fmt.Sprintf("call_%d_%s", i, part.FunctionCall.Name),
					Type: "function",
					Function: provider.FunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(args),
					},
				})
			} else if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}

		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		if len(textParts) > 0 {
			msg.Content = strings.Join(textParts, "")
		}

		choice.Message = msg
		openaiResp.Choices = append(openaiResp.Choices, choice)
	}

	if resp.UsageMetadata != nil {
		openaiResp.Usage = &provider.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	return openaiResp
}

func mapGeminiFinishReason(reason string) *string {
	var mapped string
	switch reason {
	case "STOP":
		mapped = "stop"
	case "MAX_TOKENS":
		mapped = "length"
	case "SAFETY":
		mapped = "content_filter"
	case "RECITATION":
		mapped = "content_filter"
	default:
		mapped = "stop"
	}
	return &mapped
}

// --- Streaming ---

type streamReader struct {
	reader  *bufio.Reader
	body    io.ReadCloser
	headers http.Header
	model   string
	index   int
}

func (r *streamReader) Next() ([]byte, error) {
	for {
		line, err := sse.ReadLine(r.reader)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading google_ai stream: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}

		data := bytes.TrimPrefix(line, []byte("data: "))

		var chunk geminiResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
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

func (r *streamReader) convertChunk(resp *geminiResponse) interface{} {
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
			Message string `json:"message"`
			Code    int    `json:"code"`
			Status  string `json:"status"`
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
