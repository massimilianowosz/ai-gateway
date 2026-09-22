package bedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestConvertToConverse_BasicMessage(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []provider.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	result := convertToConverse(req)

	require.Len(t, result.Messages, 1)
	assert.Equal(t, "user", result.Messages[0].Role)
	require.Len(t, result.Messages[0].Content, 1)
	assert.Equal(t, "Hello", result.Messages[0].Content[0].Text)
}

func TestConvertToConverse_SystemMessage(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []provider.Message{
			{Role: "system", Content: "You are helpful"},
			{Role: "user", Content: "Hi"},
		},
	}

	result := convertToConverse(req)

	require.Len(t, result.System, 1)
	assert.Equal(t, "You are helpful", result.System[0].Text)
	require.Len(t, result.Messages, 1)
}

func TestConvertToConverse_InferenceConfig(t *testing.T) {
	temp := 0.5
	maxTokens := 512

	req := &provider.CompletionRequest{
		Model:       "nova-2-lite",
		Messages:    []provider.Message{{Role: "user", Content: "Hi"}},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		Stop:        []interface{}{"STOP", "END"},
	}

	result := convertToConverse(req)

	require.NotNil(t, result.InferenceConfig)
	assert.Equal(t, &temp, result.InferenceConfig.Temperature)
	assert.Equal(t, &maxTokens, result.InferenceConfig.MaxTokens)
	assert.Equal(t, []string{"STOP", "END"}, result.InferenceConfig.StopSequences)
}

func TestConvertToConverse_ToolCalls(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []provider.Message{
			{Role: "user", Content: "Check weather"},
			{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{
					{
						ID:   "tc_1",
						Type: "function",
						Function: provider.FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Rome"}`,
						},
					},
				},
			},
			{Role: "tool", Content: `{"temp": 25}`, ToolCallID: "tc_1"},
		},
		Tools: []provider.Tool{
			{
				Type: "function",
				Function: provider.Function{
					Name:        "get_weather",
					Description: "Get weather",
					Parameters:  map[string]interface{}{"type": "object"},
				},
			},
		},
	}

	result := convertToConverse(req)

	// User message
	assert.Equal(t, "user", result.Messages[0].Role)
	assert.Equal(t, "Check weather", result.Messages[0].Content[0].Text)

	// Assistant with tool use
	assert.Equal(t, "assistant", result.Messages[1].Role)
	require.NotNil(t, result.Messages[1].Content[0].ToolUse)
	assert.Equal(t, "get_weather", result.Messages[1].Content[0].ToolUse.Name)
	assert.Equal(t, "tc_1", result.Messages[1].Content[0].ToolUse.ToolUseID)

	// Tool result
	assert.Equal(t, "user", result.Messages[2].Role)
	require.NotNil(t, result.Messages[2].Content[0].ToolResult)
	assert.Equal(t, "tc_1", result.Messages[2].Content[0].ToolResult.ToolUseID)

	// Tool config
	require.NotNil(t, result.ToolConfig)
	require.Len(t, result.ToolConfig.Tools, 1)
	assert.Equal(t, "get_weather", result.ToolConfig.Tools[0].ToolSpec.Name)
}

func TestConverseToOpenAI_BasicResponse(t *testing.T) {
	resp := &converseResponse{
		Output: converseOutput{
			Message: &converseMessage{
				Role: "assistant",
				Content: []contentBlock{
					{Text: "Hello! How can I help?"},
				},
			},
		},
		StopReason: "end_turn",
		Usage: &converseUsage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	}

	result := converseToOpenAI(resp, "bedrock-claude-sonnet-4", nil)

	assert.Equal(t, "chat.completion", result.Object)
	assert.Equal(t, "bedrock-claude-sonnet-4", result.Model)
	require.Len(t, result.Choices, 1)
	assert.Equal(t, "Hello! How can I help?", result.Choices[0].Message.Content)
	assert.Equal(t, "stop", *result.Choices[0].FinishReason)
	assert.Equal(t, 10, result.Usage.PromptTokens)
	assert.Equal(t, 5, result.Usage.CompletionTokens)
}

func TestConverseToOpenAI_ToolUse(t *testing.T) {
	resp := &converseResponse{
		Output: converseOutput{
			Message: &converseMessage{
				Role: "assistant",
				Content: []contentBlock{
					{Text: "Let me look that up"},
					{
						ToolUse: &toolUse{
							ToolUseID: "tu_123",
							Name:      "search",
							Input:     map[string]interface{}{"q": "weather"},
						},
					},
				},
			},
		},
		StopReason: "tool_use",
	}

	result := converseToOpenAI(resp, "bedrock-claude", nil)

	assert.Equal(t, "tool_calls", *result.Choices[0].FinishReason)
	assert.Equal(t, "Let me look that up", result.Choices[0].Message.Content)
	require.Len(t, result.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "search", result.Choices[0].Message.ToolCalls[0].Function.Name)
	assert.Equal(t, "tu_123", result.Choices[0].Message.ToolCalls[0].ID)

	var args map[string]interface{}
	json.Unmarshal([]byte(result.Choices[0].Message.ToolCalls[0].Function.Arguments), &args)
	assert.Equal(t, "weather", args["q"])
}

func TestMapBedrockStopReason(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"stop_sequence", "stop"},
		{"content_filtered", "content_filter"},
		{"unknown", "stop"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, mapBedrockStopReason(tt.input))
	}
}

func TestSigV4Signing(t *testing.T) {
	// Verify that signing produces the required AWS headers
	p := &Provider{
		region:          "us-east-1",
		modelID:         "test-model",
		accessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}

	body := []byte(`{"messages":[]}`)
	req, _ := NewTestRequest(p.region, p.modelID, body)
	err := p.signRequest(req, body)

	require.NoError(t, err)
	assert.NotEmpty(t, req.Header.Get("Authorization"))
	assert.NotEmpty(t, req.Header.Get("X-Amz-Date"))
	assert.NotEmpty(t, req.Header.Get("X-Amz-Content-Sha256"))
	assert.Contains(t, req.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
	assert.Contains(t, req.Header.Get("Authorization"), "AKIAIOSFODNN7EXAMPLE")
}

func TestSigV4Signing_WithSessionToken(t *testing.T) {
	p := &Provider{
		region:          "eu-west-1",
		modelID:         "test-model",
		accessKeyID:     "AKIAEXAMPLE",
		secretAccessKey: "SECRET",
		sessionToken:    "SESSION_TOKEN",
	}

	body := []byte(`{}`)
	req, _ := NewTestRequest(p.region, p.modelID, body)
	err := p.signRequest(req, body)

	require.NoError(t, err)
	assert.Equal(t, "SESSION_TOKEN", req.Header.Get("X-Amz-Security-Token"))
}

func NewTestRequest(region, modelID string, body []byte) (*http.Request, error) {
	url := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse", region, modelID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func TestBearerTokenAuth(t *testing.T) {
	p := &Provider{
		region:      "eu-west-1",
		modelID:     "test-model",
		bearerToken: "ABSK-test-token-123",
	}

	body := []byte(`{"messages":[]}`)
	req, _ := NewTestRequest(p.region, p.modelID, body)
	err := p.authRequest(req, body)

	require.NoError(t, err)
	assert.Equal(t, "Bearer ABSK-test-token-123", req.Header.Get("Authorization"))
	// Should NOT have SigV4 headers
	assert.Empty(t, req.Header.Get("X-Amz-Date"))
	assert.Empty(t, req.Header.Get("X-Amz-Content-Sha256"))
}

func TestNewProvider_BearerToken(t *testing.T) {
	p, err := New(Config{
		Region:      "eu-west-1",
		BearerToken: "ABSK-token",
	}, "test-model")

	require.NoError(t, err)
	assert.Equal(t, "ABSK-token", p.bearerToken)
	assert.Empty(t, p.accessKeyID)
}

func TestNewProvider_SigV4(t *testing.T) {
	p, err := New(Config{
		Region:          "us-east-1",
		AccessKeyID:     "AKIA-test",
		SecretAccessKey: "secret-test",
	}, "test-model")

	require.NoError(t, err)
	assert.Equal(t, "AKIA-test", p.accessKeyID)
	assert.Empty(t, p.bearerToken)
}

func TestNewProvider_MissingAuth(t *testing.T) {
	_, err := New(Config{
		Region: "us-east-1",
	}, "test-model")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "either bearer_token or access_key_id is required")
}
