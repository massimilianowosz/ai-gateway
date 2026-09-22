package vertex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

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

// --- Gemini response types ---

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

// convertToGemini transforms an OpenAI-format request to Gemini format.
func convertToGemini(req *provider.CompletionRequest) *geminiRequest {
	gr := &geminiRequest{}

	// Extract system messages and convert the rest
	for _, msg := range req.Messages {
		role := msg.Role
		switch role {
		case "system":
			text := extractTextContent(msg.Content)
			gr.SystemInstruction = &geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: text}},
			}
		case "assistant":
			parts := messageToGeminiParts(msg)
			gr.Contents = append(gr.Contents, geminiContent{
				Role:  "model",
				Parts: parts,
			})
		case "tool":
			// Tool responses become function responses
			var result map[string]interface{}
			text := extractTextContent(msg.Content)
			_ = json.Unmarshal([]byte(text), &result)
			if result == nil {
				result = map[string]interface{}{"result": text}
			}
			gr.Contents = append(gr.Contents, geminiContent{
				Role: "function",
				Parts: []geminiPart{{
					FunctionResp: &functionResp{
						Name:     msg.Name,
						Response: result,
					},
				}},
			})
		default: // "user"
			parts := messageToGeminiParts(msg)
			gr.Contents = append(gr.Contents, geminiContent{
				Role:  "user",
				Parts: parts,
			})
		}
	}

	// Generation config
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

	// Tools
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
	// Handle tool calls in assistant messages
	if len(msg.ToolCalls) > 0 {
		var parts []geminiPart
		for _, tc := range msg.ToolCalls {
			var args map[string]interface{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			parts = append(parts, geminiPart{
				FunctionCall: &functionCall{
					Name: tc.Function.Name,
					Args: args,
				},
			})
		}
		return parts
	}

	// Handle content (string or multimodal)
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
						// For data URIs, extract inline data
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

// geminiToOpenAI converts a Gemini response to OpenAI format.
func geminiToOpenAI(resp *geminiResponse, model string, headers map[string][]string) *provider.CompletionResponse {
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

		// Check for function calls
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
			combined := ""
			for _, t := range textParts {
				combined += t
			}
			msg.Content = combined
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

func extractTextContent(content interface{}) string {
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

func parseDataURI(uri string) (mime, data string, ok bool) {
	// Format: data:image/png;base64,iVBOR...
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
	mime = rest[:semicol]
	// Skip ";base64,"
	dataStart := semicol + 8 // len(";base64,")
	if dataStart >= len(rest) {
		return "", "", false
	}
	return mime, rest[dataStart:], true
}
