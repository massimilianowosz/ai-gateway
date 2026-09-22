package echo

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestNew(t *testing.T) {
	p := New("gpt-4o")
	assert.Equal(t, "echo", p.Name())
}

func TestComplete(t *testing.T) {
	p := New("gpt-4o")
	req := &provider.CompletionRequest{Model: "gpt-4o"}

	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Contains(t, resp.ID, "echo-")
	assert.Equal(t, "chat.completion", resp.Object)
	assert.Equal(t, "gpt-4o", resp.Model)
	assert.NotZero(t, resp.Created)

	require.Len(t, resp.Choices, 1)
	choice := resp.Choices[0]
	assert.Equal(t, 0, choice.Index)
	require.NotNil(t, choice.Message)
	assert.Equal(t, "assistant", choice.Message.Role)
	assert.NotEmpty(t, choice.Message.Content)
	require.NotNil(t, choice.FinishReason)
	assert.Equal(t, "stop", *choice.FinishReason)

	require.NotNil(t, resp.Usage)
	assert.Equal(t, 25, resp.Usage.PromptTokens)
	assert.Equal(t, 15, resp.Usage.CompletionTokens)
	assert.Equal(t, 40, resp.Usage.TotalTokens)
}

func TestComplete_EchoesRequestedModel(t *testing.T) {
	p := New("configured-model")
	req := &provider.CompletionRequest{Model: "caller-supplied-model"}

	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)

	// The response model reflects the request, not the deployment's configured model.
	assert.Equal(t, "caller-supplied-model", resp.Model)
}

func TestStream(t *testing.T) {
	p := New("gpt-4o")
	req := &provider.CompletionRequest{Model: "gpt-4o"}

	reader, err := p.Stream(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, reader)
	defer reader.Close()

	chunk, err := reader.Next()
	require.NoError(t, err)
	require.NotEmpty(t, chunk)

	var payload struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(chunk, &payload))

	assert.Contains(t, payload.ID, "echo-")
	assert.Equal(t, "chat.completion.chunk", payload.Object)
	assert.Equal(t, "gpt-4o", payload.Model)
	require.Len(t, payload.Choices, 1)
	assert.Equal(t, "assistant", payload.Choices[0].Delta.Role)
	assert.NotEmpty(t, payload.Choices[0].Delta.Content)
	assert.Equal(t, "stop", payload.Choices[0].FinishReason)
}

func TestStream_EOFAfterSingleChunk(t *testing.T) {
	p := New("gpt-4o")
	req := &provider.CompletionRequest{Model: "gpt-4o"}

	reader, err := p.Stream(context.Background(), req)
	require.NoError(t, err)
	defer reader.Close()

	_, err = reader.Next()
	require.NoError(t, err)

	_, err = reader.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStream_CloseAndHeaders(t *testing.T) {
	p := New("gpt-4o")
	req := &provider.CompletionRequest{Model: "gpt-4o"}

	reader, err := p.Stream(context.Background(), req)
	require.NoError(t, err)

	assert.NoError(t, reader.Close())
	assert.NotNil(t, reader.Headers())
	assert.Empty(t, reader.Headers())
}
