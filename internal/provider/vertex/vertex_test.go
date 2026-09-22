package vertex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestConvertToGemini_BasicMessage(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "gemini-2.5-flash",
		Messages: []provider.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	result := convertToGemini(req)

	require.Len(t, result.Contents, 1)
	assert.Equal(t, "user", result.Contents[0].Role)
	assert.Equal(t, "Hello", result.Contents[0].Parts[0].Text)
	assert.Nil(t, result.SystemInstruction)
}

func TestConvertToGemini_SystemMessage(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "gemini-2.5-flash",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful"},
			{Role: "user", Content: "Hello"},
		},
	}

	result := convertToGemini(req)

	require.NotNil(t, result.SystemInstruction)
	assert.Equal(t, "You are helpful", result.SystemInstruction.Parts[0].Text)
	require.Len(t, result.Contents, 1)
	assert.Equal(t, "user", result.Contents[0].Role)
}

func TestConvertToGemini_AssistantToModel(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "gemini-2.5-flash",
		Messages: []provider.Message{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "Hello!"},
			{Role: "user", Content: "How are you?"},
		},
	}

	result := convertToGemini(req)

	require.Len(t, result.Contents, 3)
	assert.Equal(t, "user", result.Contents[0].Role)
	assert.Equal(t, "model", result.Contents[1].Role)
	assert.Equal(t, "user", result.Contents[2].Role)
}

func TestConvertToGemini_GenerationConfig(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTokens := 1000

	req := &provider.CompletionRequest{
		Model:       "gemini-2.5-flash",
		Messages:    []provider.Message{{Role: "user", Content: "Hi"}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTokens,
		Stop:        "END",
	}

	result := convertToGemini(req)

	require.NotNil(t, result.GenerationConfig)
	assert.Equal(t, &temp, result.GenerationConfig.Temperature)
	assert.Equal(t, &topP, result.GenerationConfig.TopP)
	assert.Equal(t, &maxTokens, result.GenerationConfig.MaxOutputTokens)
	assert.Equal(t, []string{"END"}, result.GenerationConfig.StopSequences)
}

func TestConvertToGemini_Tools(t *testing.T) {
	req := &provider.CompletionRequest{
		Model:    "gemini-2.5-flash",
		Messages: []provider.Message{{Role: "user", Content: "What's the weather?"}},
		Tools: []provider.Tool{
			{
				Type: "function",
				Function: provider.Function{
					Name:        "get_weather",
					Description: "Get current weather",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
		},
	}

	result := convertToGemini(req)

	require.Len(t, result.Tools, 1)
	require.Len(t, result.Tools[0].FunctionDeclarations, 1)
	assert.Equal(t, "get_weather", result.Tools[0].FunctionDeclarations[0].Name)
}

func TestConvertToGemini_ToolCallsInAssistant(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "gemini-2.5-flash",
		Messages: []provider.Message{
			{Role: "user", Content: "Hi"},
			{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: provider.FunctionCall{
							Name:      "get_weather",
							Arguments: `{"location":"Rome"}`,
						},
					},
				},
			},
			{Role: "tool", Content: `{"temp": 25}`, Name: "get_weather", ToolCallID: "call_1"},
		},
	}

	result := convertToGemini(req)

	require.Len(t, result.Contents, 3)
	// Assistant with tool call → model with functionCall part
	assert.Equal(t, "model", result.Contents[1].Role)
	require.NotNil(t, result.Contents[1].Parts[0].FunctionCall)
	assert.Equal(t, "get_weather", result.Contents[1].Parts[0].FunctionCall.Name)

	// Tool response → function role
	assert.Equal(t, "function", result.Contents[2].Role)
	require.NotNil(t, result.Contents[2].Parts[0].FunctionResp)
}

func TestGeminiToOpenAI_BasicResponse(t *testing.T) {
	stop := "STOP"
	resp := &geminiResponse{
		Candidates: []geminiCandidate{
			{
				Content: geminiContent{
					Role: "model",
					Parts: []geminiPart{
						{Text: "Hello! How can I help?"},
					},
				},
				FinishReason: stop,
			},
		},
		UsageMetadata: &geminiUsage{
			PromptTokenCount:     10,
			CandidatesTokenCount: 5,
			TotalTokenCount:      15,
		},
	}

	result := geminiToOpenAI(resp, "gemini-2.5-flash", nil)

	assert.Equal(t, "chat.completion", result.Object)
	assert.Equal(t, "gemini-2.5-flash", result.Model)
	require.Len(t, result.Choices, 1)
	assert.Equal(t, "Hello! How can I help?", result.Choices[0].Message.Content)
	assert.Equal(t, "stop", *result.Choices[0].FinishReason)
	require.NotNil(t, result.Usage)
	assert.Equal(t, 10, result.Usage.PromptTokens)
	assert.Equal(t, 5, result.Usage.CompletionTokens)
}

func TestGeminiToOpenAI_FunctionCall(t *testing.T) {
	resp := &geminiResponse{
		Candidates: []geminiCandidate{
			{
				Content: geminiContent{
					Role: "model",
					Parts: []geminiPart{
						{
							FunctionCall: &functionCall{
								Name: "get_weather",
								Args: map[string]interface{}{"location": "Rome"},
							},
						},
					},
				},
				FinishReason: "STOP",
			},
		},
	}

	result := geminiToOpenAI(resp, "gemini-2.5-flash", nil)

	require.Len(t, result.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "get_weather", result.Choices[0].Message.ToolCalls[0].Function.Name)
	var args map[string]interface{}
	json.Unmarshal([]byte(result.Choices[0].Message.ToolCalls[0].Function.Arguments), &args)
	assert.Equal(t, "Rome", args["location"])
}

func TestConvertToAnthropic_BasicMessage(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful"},
			{Role: "user", Content: "Hello"},
		},
	}

	result := convertToAnthropic(req)

	assert.Equal(t, "You are helpful", result.System)
	assert.Equal(t, "vertex-2023-10-16", result.AnthropicVersion)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "user", result.Messages[0].Role)
	assert.Equal(t, "Hello", result.Messages[0].Content)
}

func TestConvertToAnthropic_WithMaxTokens(t *testing.T) {
	maxTokens := 2000
	req := &provider.CompletionRequest{
		Model:     "claude-sonnet-4",
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
		MaxTokens: &maxTokens,
	}

	result := convertToAnthropic(req)
	assert.Equal(t, 2000, result.MaxTokens)
}

func TestConvertToAnthropic_ToolCalls(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []provider.Message{
			{Role: "user", Content: "Hi"},
			{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{
					{
						ID:   "tc_1",
						Type: "function",
						Function: provider.FunctionCall{
							Name:      "search",
							Arguments: `{"q":"test"}`,
						},
					},
				},
			},
			{Role: "tool", Content: "found it", ToolCallID: "tc_1"},
		},
	}

	result := convertToAnthropic(req)

	require.Len(t, result.Messages, 3)
	// Assistant message should have tool_use blocks (may include text block if content was present)
	blocks := result.Messages[1].Content.([]anthropicContentBlock)
	// Find the tool_use block
	var toolBlock anthropicContentBlock
	for _, b := range blocks {
		if b.Type == "tool_use" {
			toolBlock = b
		}
	}
	assert.Equal(t, "tool_use", toolBlock.Type)
	assert.Equal(t, "tc_1", toolBlock.ID)

	// Tool result
	toolBlocks := result.Messages[2].Content.([]anthropicContentBlock)
	require.Len(t, toolBlocks, 1)
	assert.Equal(t, "tool_result", toolBlocks[0].Type)
	assert.Equal(t, "tc_1", toolBlocks[0].ToolUseID)
}

func TestAnthropicToOpenAI_BasicResponse(t *testing.T) {
	resp := &anthropicResponse{
		ID:   "msg_123",
		Type: "message",
		Role: "assistant",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Hello!"},
		},
		StopReason: "end_turn",
		Usage:      &anthropicUsage{InputTokens: 10, OutputTokens: 5},
	}

	result := anthropicToOpenAI(resp, "claude-sonnet-4", nil)

	assert.Equal(t, "msg_123", result.ID)
	assert.Equal(t, "chat.completion", result.Object)
	require.Len(t, result.Choices, 1)
	assert.Equal(t, "Hello!", result.Choices[0].Message.Content)
	assert.Equal(t, "stop", *result.Choices[0].FinishReason)
	assert.Equal(t, 15, result.Usage.TotalTokens)
}

func TestAnthropicToOpenAI_ToolUse(t *testing.T) {
	resp := &anthropicResponse{
		ID:   "msg_456",
		Role: "assistant",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Let me check"},
			{
				Type:  "tool_use",
				ID:    "tu_1",
				Name:  "search",
				Input: map[string]interface{}{"query": "test"},
			},
		},
		StopReason: "tool_use",
	}

	result := anthropicToOpenAI(resp, "claude-sonnet-4", nil)

	assert.Equal(t, "tool_calls", *result.Choices[0].FinishReason)
	assert.Equal(t, "Let me check", result.Choices[0].Message.Content)
	require.Len(t, result.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "search", result.Choices[0].Message.ToolCalls[0].Function.Name)
}

func TestIsAnthropicModel(t *testing.T) {
	assert.True(t, isAnthropicModel("claude-sonnet-4"))
	assert.True(t, isAnthropicModel("claude-3-opus"))
	assert.True(t, isAnthropicModel("claude-haiku-4-5"))
	assert.False(t, isAnthropicModel("gemini-2.5-flash"))
	assert.False(t, isAnthropicModel("llama-3"))
}

func TestMapGeminiFinishReason(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"UNKNOWN", "stop"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, *mapGeminiFinishReason(tt.input))
	}
}

func TestParseDataURI(t *testing.T) {
	mime, data, ok := parseDataURI("data:image/png;base64,iVBORw0KGgo=")
	assert.True(t, ok)
	assert.Equal(t, "image/png", mime)
	assert.Equal(t, "iVBORw0KGgo=", data)

	_, _, ok = parseDataURI("https://example.com/image.png")
	assert.False(t, ok)
}
