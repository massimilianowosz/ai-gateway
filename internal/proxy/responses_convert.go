package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- Responses API request types ---

type responsesRequest struct {
	Model              string               `json:"model"`
	Input              json.RawMessage      `json:"input,omitempty"`
	Instructions       string               `json:"instructions,omitempty"`
	Stream             bool                 `json:"stream,omitempty"`
	Temperature        *float64             `json:"temperature,omitempty"`
	TopP               *float64             `json:"top_p,omitempty"`
	MaxOutputTokens    *int                 `json:"max_output_tokens,omitempty"`
	MaxToolCalls       *int                 `json:"max_tool_calls,omitempty"`
	Tools              []responsesTool      `json:"tools,omitempty"`
	ToolChoice         interface{}          `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	Conversation       json.RawMessage      `json:"conversation,omitempty"`
	Store              *bool                `json:"store,omitempty"`
	Background         *bool                `json:"background,omitempty"`
	Include            []string             `json:"include,omitempty"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
	Reasoning          *responsesReasoning  `json:"reasoning,omitempty"`
	Text               *responsesTextConfig `json:"text,omitempty"`
	Truncation         string               `json:"truncation,omitempty"`
	ServiceTier        string               `json:"service_tier,omitempty"`
	PromptCacheKey     string               `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier   string               `json:"safety_identifier,omitempty"`
	User               string               `json:"user,omitempty"`
	TopLogprobs        *int                 `json:"top_logprobs,omitempty"`
	StreamOptions      json.RawMessage      `json:"stream_options,omitempty"`
	Prompt             json.RawMessage      `json:"prompt,omitempty"`

	// resolvedConversation and pendingConversationItems are filled in when the
	// request names a gateway conversation. The conversation's history is
	// merged into Input so the provider sees the whole thread, while the items
	// this turn contributes are kept apart — appending the merged Input back
	// would store the history a second time on every turn.
	resolvedConversation     string
	pendingConversationItems []json.RawMessage
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesTextConfig struct {
	Format    *responsesTextFormat `json:"format,omitempty"`
	Verbosity string               `json:"verbosity,omitempty"`
}

// responsesTextFormat is the Responses shape of a structured-output config.
// Note it is *flat*: name/schema/strict sit next to type, whereas Chat
// Completions nests them under a "json_schema" object.
type responsesTextFormat struct {
	Type        string      `json:"type"`
	Name        string      `json:"name,omitempty"`
	Description string      `json:"description,omitempty"`
	Schema      interface{} `json:"schema,omitempty"`
	Strict      *bool       `json:"strict,omitempty"`
}

type responsesTool struct {
	Type        string      `json:"type"`
	Name        string      `json:"name,omitempty"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Strict      *bool       `json:"strict,omitempty"`
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Summary   json.RawMessage `json:"summary"`
	Refusal   string          `json:"refusal"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
	Detail   string `json:"detail"`
}

// --- Responses API response types ---

type responsesOutputTextContent struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []interface{}     `json:"annotations"`
	Logprobs    []json.RawMessage `json:"logprobs,omitempty"`
}

type responsesRefusalContent struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

type responsesMessageItem struct {
	Type    string        `json:"type"`
	ID      string        `json:"id"`
	Status  string        `json:"status"`
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

type responsesFunctionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// responsesReasoningItem carries a provider's chain-of-thought summary. The
// gateway only ever produces summary text, never encrypted reasoning content.
type responsesReasoningItem struct {
	Type    string                          `json:"type"`
	ID      string                          `json:"id"`
	Status  string                          `json:"status"`
	Summary []responsesReasoningSummaryPart `json:"summary"`
}

type responsesReasoningSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesTokenDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type responsesUsage struct {
	InputTokens        int                    `json:"input_tokens"`
	InputTokenDetails  *responsesTokenDetails `json:"input_tokens_details,omitempty"`
	OutputTokens       int                    `json:"output_tokens"`
	OutputTokenDetails *responsesTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens        int                    `json:"total_tokens"`
}

type responsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesResponse struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	CreatedAt          int64                       `json:"created_at"`
	Status             string                      `json:"status"`
	Model              string                      `json:"model"`
	Output             []interface{}               `json:"output"`
	OutputText         *string                     `json:"output_text,omitempty"`
	Usage              *responsesUsage             `json:"usage,omitempty"`
	ParallelToolCalls  bool                        `json:"parallel_tool_calls"`
	Error              *responsesError             `json:"error,omitempty"`
	IncompleteDetails  *responsesIncompleteDetails `json:"incomplete_details,omitempty"`
	Instructions       interface{}                 `json:"instructions"`
	MaxOutputTokens    *int                        `json:"max_output_tokens,omitempty"`
	MaxToolCalls       *int                        `json:"max_tool_calls,omitempty"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	PreviousResponseID string                      `json:"previous_response_id,omitempty"`
	Reasoning          *responsesReasoning         `json:"reasoning,omitempty"`
	Store              bool                        `json:"store"`
	Background         bool                        `json:"background,omitempty"`
	ServiceTier        string                      `json:"service_tier,omitempty"`
	Temperature        *float64                    `json:"temperature,omitempty"`
	TopP               *float64                    `json:"top_p,omitempty"`
	TopLogprobs        *int                        `json:"top_logprobs,omitempty"`
	Text               *responsesTextConfig        `json:"text,omitempty"`
	ToolChoice         interface{}                 `json:"tool_choice,omitempty"`
	Tools              []responsesTool             `json:"tools"`
	Truncation         string                      `json:"truncation,omitempty"`
	User               string                      `json:"user,omitempty"`
}

// responsesError mirrors the OpenAI Responses API's response.error shape
// (response.failed / response.incomplete events), e.g.
// {"code": "usage_limit_reached", "message": "The usage limit has been reached"}.
// Codex CLI's SSE parser reads response.error.code/message to surface the
// real upstream failure reason to the user.
type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Conversion: Responses -> OpenAI ---

// unsupportedResponsesToolError explains why a built-in tool cannot work on a
// provider that only speaks Chat Completions. Silently dropping the tool would
// leave the caller wondering why the model never used it.
type unsupportedResponsesToolError struct {
	toolType string
	model    string
}

func (e *unsupportedResponsesToolError) Error() string {
	return fmt.Sprintf(
		"tool type %q is not supported for model %q: built-in tools require a provider with a native Responses API",
		e.toolType, e.model)
}

// unsupportedResponsesParamError covers Responses parameters whose meaning
// lives entirely upstream, so translating the request without them would send
// something the caller never asked for.
type unsupportedResponsesParamError struct {
	param  string
	model  string
	reason string
}

func (e *unsupportedResponsesParamError) Error() string {
	return fmt.Sprintf("%q is not supported for model %q: %s", e.param, e.model, e.reason)
}

func (h *ResponsesHandler) responsesToOpenAIRequest(req *responsesRequest) (*provider.CompletionRequest, error) {
	// A prompt template holds the instructions, tools and variables of the
	// turn, and only the provider that stores it can expand it. Dropping it
	// would send the model an empty brief instead of the caller's prompt, so
	// this is rejected rather than silently ignored.
	if len(req.Prompt) > 0 {
		return nil, &unsupportedResponsesParamError{
			param: "prompt", model: req.Model,
			reason: "prompt templates are stored by the provider and require a native Responses API",
		}
	}

	openaiReq := &provider.CompletionRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		User:        responsesUserIdentifier(req),
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = req.MaxOutputTokens
	}
	if req.ParallelToolCalls != nil {
		openaiReq.Extra = setExtra(openaiReq.Extra, "parallel_tool_calls", *req.ParallelToolCalls)
	}

	if req.Instructions != "" {
		openaiReq.Messages = append(openaiReq.Messages, provider.Message{
			Role:    "system",
			Content: req.Instructions,
		})
	}

	msgs, err := parseResponsesInput(req.Input)
	if err != nil {
		return nil, err
	}
	openaiReq.Messages = append(openaiReq.Messages, msgs...)

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, &unsupportedResponsesToolError{toolType: t.Type, model: req.Model}
		}
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object"}
		}
		openaiReq.Tools = append(openaiReq.Tools, provider.Tool{
			Type: "function",
			Function: provider.Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	if req.ToolChoice != nil {
		openaiReq.ToolChoice = convertResponsesToolChoice(req.ToolChoice)
	}
	if format := responsesTextToResponseFormat(req.Text); format != nil {
		openaiReq.ResponseFormat = format
	}

	applyResponsesChatExtras(openaiReq, req)
	return openaiReq, nil
}

// applyResponsesChatExtras forwards the Responses parameters that have a real
// Chat Completions counterpart. Responses-only parameters (truncation,
// include, max_tool_calls, ...) are deliberately not forwarded: a chat endpoint
// would reject them as unknown.
func applyResponsesChatExtras(openaiReq *provider.CompletionRequest, req *responsesRequest) {
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		openaiReq.Extra = setExtra(openaiReq.Extra, "reasoning_effort", req.Reasoning.Effort)
	}
	if req.ServiceTier != "" {
		openaiReq.Extra = setExtra(openaiReq.Extra, "service_tier", req.ServiceTier)
	}
	if req.PromptCacheKey != "" {
		openaiReq.Extra = setExtra(openaiReq.Extra, "prompt_cache_key", req.PromptCacheKey)
	}
	if req.SafetyIdentifier != "" {
		openaiReq.Extra = setExtra(openaiReq.Extra, "safety_identifier", req.SafetyIdentifier)
	}
	if len(req.Metadata) > 0 {
		openaiReq.Extra = setExtra(openaiReq.Extra, "metadata", req.Metadata)
	}
	if req.TopLogprobs != nil {
		// Chat Completions requires logprobs:true before top_logprobs is honoured.
		openaiReq.Extra = setExtra(openaiReq.Extra, "logprobs", true)
		openaiReq.Extra = setExtra(openaiReq.Extra, "top_logprobs", *req.TopLogprobs)
	}
	if len(req.StreamOptions) > 0 {
		var opts provider.StreamOptions
		if json.Unmarshal(req.StreamOptions, &opts) == nil {
			openaiReq.StreamOptions = &opts
		}
	}
}

func setExtra(extra map[string]interface{}, key string, value interface{}) map[string]interface{} {
	if extra == nil {
		extra = make(map[string]interface{}, 4)
	}
	extra[key] = value
	return extra
}

// responsesUserIdentifier prefers safety_identifier, the replacement OpenAI
// introduced for the deprecated user field.
func responsesUserIdentifier(req *responsesRequest) string {
	if req.SafetyIdentifier != "" {
		return req.SafetyIdentifier
	}
	return req.User
}

// responsesTextToResponseFormat converts the Responses text.format config into
// a Chat Completions response_format. The two differ in shape: Responses keeps
// name/schema/strict flat inside format, Chat Completions nests them under
// json_schema.
func responsesTextToResponseFormat(text *responsesTextConfig) interface{} {
	if text == nil || text.Format == nil {
		return nil
	}
	format := text.Format
	if format.Type != "json_schema" {
		if format.Type == "" {
			return nil
		}
		return map[string]interface{}{"type": format.Type}
	}

	schema := map[string]interface{}{"name": format.Name}
	if format.Description != "" {
		schema["description"] = format.Description
	}
	if format.Schema != nil {
		schema["schema"] = format.Schema
	}
	if format.Strict != nil {
		schema["strict"] = *format.Strict
	}
	return map[string]interface{}{"type": "json_schema", "json_schema": schema}
}

func convertResponsesToolChoice(tc interface{}) interface{} {
	if s, ok := tc.(string); ok {
		return s
	}
	if m, ok := tc.(map[string]interface{}); ok {
		if t, _ := m["type"].(string); t == "function" {
			name, _ := m["name"].(string)
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
	}
	return tc
}

// parseResponsesInput converts the Responses API "input" field (a plain
// string or an array of typed items) into internal chat messages.
func parseResponsesInput(raw json.RawMessage) ([]provider.Message, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []provider.Message{{Role: "user", Content: asString}}, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}

	var messages []provider.Message
	for _, itemRaw := range items {
		var item responsesInputItem
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			continue
		}

		itemType := item.Type
		if itemType == "" && item.Role != "" {
			itemType = "message"
		}

		switch itemType {
		case "message":
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, provider.Message{
				Role:    role,
				Content: responsesContentToMessageContent(item.Content),
			})

		case "function_call":
			messages = appendResponsesToolCall(messages, item)

		case "function_call_output":
			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    extractResponsesOutputText(item.Output),
			})

		case "reasoning":
			// Reasoning summaries are echoed back by clients replaying a prior
			// turn. Providers reject them as chat messages, so the summary is
			// carried on the assistant turn it belongs to instead.
			if summary := responsesReasoningSummaryText(item.Summary); summary != "" {
				messages = appendResponsesReasoning(messages, summary)
			}

		default:
			// item_reference and built-in tool call items have no Chat
			// Completions equivalent; they are resolved (or rejected) before
			// this point.
		}
	}

	return messages, nil
}

func appendResponsesToolCall(messages []provider.Message, item responsesInputItem) []provider.Message {
	call := provider.ToolCall{
		ID:   item.CallID,
		Type: "function",
		Function: provider.FunctionCall{
			Name:      item.Name,
			Arguments: item.Arguments,
		},
	}
	if n := len(messages); n > 0 && messages[n-1].Role == "assistant" && messages[n-1].Content == nil {
		messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, call)
		return messages
	}
	return append(messages, provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{call}})
}

func appendResponsesReasoning(messages []provider.Message, summary string) []provider.Message {
	if n := len(messages); n > 0 && messages[n-1].Role == "assistant" {
		messages[n-1].ReasoningContent = summary
		return messages
	}
	return append(messages, provider.Message{Role: "assistant", ReasoningContent: summary})
}

func responsesReasoningSummaryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []responsesReasoningSummaryPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range parts {
		sb.WriteString(part.Text)
	}
	return sb.String()
}

// responsesContentToMessageContent converts a Responses API message "content"
// field (string or array of content parts) into internal message content
// (string, or []provider.ContentPart for multimodal input).
func responsesContentToMessageContent(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	onlyText := true
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			onlyText = false
			break
		}
	}
	if onlyText {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}

	var out []interface{}
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, map[string]interface{}{"type": "text", "text": p.Text})
		case "refusal":
			out = append(out, map[string]interface{}{"type": "text", "text": p.Refusal})
		case "input_image":
			image := map[string]interface{}{}
			if p.ImageURL != "" {
				image["url"] = p.ImageURL
			}
			if p.FileID != "" {
				image["file_id"] = p.FileID
			}
			if p.Detail != "" {
				image["detail"] = p.Detail
			}
			if len(image) > 0 {
				out = append(out, map[string]interface{}{"type": "image_url", "image_url": image})
			}
		case "input_file":
			file := map[string]interface{}{}
			if p.FileID != "" {
				file["file_id"] = p.FileID
			}
			if p.FileData != "" {
				file["file_data"] = p.FileData
			}
			if p.FileURL != "" {
				file["file_url"] = p.FileURL
			}
			if p.Filename != "" {
				file["filename"] = p.Filename
			}
			if len(file) > 0 {
				out = append(out, map[string]interface{}{"type": "file", "file": file})
			}
		case "input_audio":
			audio := map[string]interface{}{}
			if p.FileData != "" {
				audio["data"] = p.FileData
			}
			if p.Filename != "" {
				audio["format"] = strings.TrimPrefix(strings.ToLower(p.Filename[strings.LastIndex(p.Filename, ".")+1:]), ".")
			}
			if len(audio) > 0 {
				out = append(out, map[string]interface{}{"type": "input_audio", "input_audio": audio})
			}
		}
	}
	return out
}

func extractResponsesOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	switch v := responsesContentToMessageContent(raw).(type) {
	case string:
		return v
	case []interface{}:
		return concatenateResponseTextParts(v)
	}
	return ""
}

func concatenateResponseTextParts(parts []interface{}) string {
	var result strings.Builder
	for _, part := range parts {
		result.WriteString(responseTextPart(part))
	}
	return result.String()
}

func responseTextPart(value interface{}) string {
	part, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	partType, _ := part["type"].(string)
	if partType != "text" {
		return ""
	}
	text, _ := part["text"].(string)
	return text
}

// --- Conversion: OpenAI -> Responses ---

// newResponsesResponse seeds a response object with the request parameters the
// Responses API echoes back. Clients (and the official SDK's Pydantic models)
// expect these to round-trip.
func newResponsesResponse(id string, createdAt int64, req *responsesRequest) *responsesResponse {
	out := &responsesResponse{
		ID:                 id,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "completed",
		Model:              req.Model,
		Output:             []interface{}{},
		ParallelToolCalls:  req.ParallelToolCalls == nil || *req.ParallelToolCalls,
		Instructions:       nil,
		MaxOutputTokens:    req.MaxOutputTokens,
		MaxToolCalls:       req.MaxToolCalls,
		Metadata:           req.Metadata,
		PreviousResponseID: req.PreviousResponseID,
		Reasoning:          req.Reasoning,
		Store:              req.Store == nil || *req.Store,
		Background:         req.Background != nil && *req.Background,
		ServiceTier:        req.ServiceTier,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		TopLogprobs:        req.TopLogprobs,
		Text:               req.Text,
		ToolChoice:         req.ToolChoice,
		Tools:              req.Tools,
		Truncation:         req.Truncation,
		User:               req.User,
	}
	if req.Instructions != "" {
		out.Instructions = req.Instructions
	}
	if out.Tools == nil {
		out.Tools = []responsesTool{}
	}
	return out
}

// setResponsesOutput attaches the output items and derives the output_text
// convenience field the SDK exposes as response.output_text.
func setResponsesOutput(out *responsesResponse, items []interface{}) {
	if items == nil {
		items = []interface{}{}
	}
	out.Output = items

	var sb strings.Builder
	for _, item := range items {
		msg, ok := item.(responsesMessageItem)
		if !ok {
			continue
		}
		for _, part := range msg.Content {
			if text, ok := part.(responsesOutputTextContent); ok {
				sb.WriteString(text.Text)
			}
		}
	}
	text := sb.String()
	out.OutputText = &text
}

func (h *ResponsesHandler) openAIToResponses(resp *provider.CompletionResponse, req *responsesRequest) *responsesResponse {
	out := newResponsesResponse(newResponseID(), resp.Created, req)

	var items []interface{}
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message != nil {
			if choice.Message.ReasoningContent != "" {
				items = append(items, responsesReasoningItem{
					Type: "reasoning", ID: newResponsesItemID("rs"), Status: "completed",
					Summary: []responsesReasoningSummaryPart{{
						Type: "summary_text", Text: choice.Message.ReasoningContent,
					}},
				})
			}

			var content []interface{}
			if text, ok := choice.Message.Content.(string); ok && text != "" {
				content = append(content, responsesOutputTextContent{
					Type: "output_text", Text: text, Annotations: []interface{}{},
					Logprobs: choiceLogprobs(choice),
				})
			}
			if choice.Message.Refusal != "" {
				content = append(content, responsesRefusalContent{
					Type: "refusal", Refusal: choice.Message.Refusal,
				})
			}
			if len(content) > 0 {
				items = append(items, responsesMessageItem{
					Type: "message", ID: newResponsesItemID("msg"),
					Status: "completed", Role: "assistant", Content: content,
				})
			}

			for _, tc := range choice.Message.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				items = append(items, responsesFunctionCallItem{
					Type: "function_call", ID: newResponsesItemID("fc"),
					CallID: tc.ID, Name: tc.Function.Name, Arguments: args, Status: "completed",
				})
			}
		}
		if choice.FinishReason != nil {
			applyResponsesFinishReason(out, *choice.FinishReason)
		}
	}
	setResponsesOutput(out, items)

	if resp.Usage != nil {
		out.Usage = newResponsesUsage(resp.Usage)
	}

	return out
}

// choiceLogprobs lifts the token probabilities a caller asked for out of the
// chat choice. Responses carries them on the output_text part rather than
// beside the choice, but the entries themselves have the same shape.
func choiceLogprobs(choice provider.Choice) []json.RawMessage {
	if choice.Logprobs == nil || len(choice.Logprobs.Content) == 0 {
		return nil
	}
	return choice.Logprobs.Content
}

// applyResponsesFinishReason maps a Chat Completions finish_reason onto the
// Responses status / incomplete_details pair.
func applyResponsesFinishReason(out *responsesResponse, finishReason string) {
	switch finishReason {
	case "length":
		out.Status = "incomplete"
		out.IncompleteDetails = &responsesIncompleteDetails{Reason: "max_output_tokens"}
	case "content_filter":
		out.Status = "incomplete"
		out.IncompleteDetails = &responsesIncompleteDetails{Reason: "content_filter"}
	}
}

func newResponsesUsage(usage *provider.Usage) *responsesUsage {
	out := &responsesUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if d := usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
		out.InputTokenDetails = &responsesTokenDetails{CachedTokens: d.CachedTokens}
	}
	if d := usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > 0 {
		out.OutputTokenDetails = &responsesTokenDetails{ReasoningTokens: d.ReasoningTokens}
	}
	return out
}
