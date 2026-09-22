package middleware

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three surfaces shape both their bodies and their terminal events
// differently. Distinguishing only Anthropic from "everything else" wrote an
// OpenAI chat.completion.chunk into a Responses stream.
func TestFeedbackAppender_ResponsesJSONGetsTheQuestion(t *testing.T) {
	upstream := `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"La risposta vera."}]}]}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/responses", `{"model":"gpt-4","input":"hello"}`)

	var resp struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Output, 1)
	require.Len(t, resp.Output[0].Content, 2, "the question is a part of its own")
	assert.Equal(t, "La risposta vera.", resp.Output[0].Content[0].Text)
	assert.Contains(t, resp.Output[0].Content[1].Text, "📊")
	assert.Equal(t, "output_text", resp.Output[0].Content[1].Type)
}

// A Responses turn ending in a function call must reach the client untouched.
func TestFeedbackAppender_ResponsesFunctionCallIsLeftAlone(t *testing.T) {
	upstream := `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"}]}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/responses", `{"model":"gpt-4","input":"hello"}`)

	assert.JSONEq(t, upstream, rec.Body.String())
}

// Streaming Responses is left alone rather than corrupted: adding an output
// item needs an index and item id consistent with what the provider sent, and
// a malformed turn is worse than an unasked question.
func TestFeedbackAppender_ResponsesStreamIsNotCorrupted(t *testing.T) {
	upstream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ciao\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "text/event-stream"),
		"/v1/responses", `{"model":"gpt-4","input":"hello","stream":true}`)

	out := rec.Body.String()
	assert.Equal(t, upstream, out, "the Responses stream must pass through byte-for-byte")
	assert.NotContains(t, out, "chat.completion.chunk", "an OpenAI chunk must never enter a Responses stream")
}

func TestSurfaceForPath(t *testing.T) {
	assert.Equal(t, surfaceAnthropic, surfaceForPath("/v1/messages"))
	assert.Equal(t, surfaceResponses, surfaceForPath("/v1/responses"))
	assert.Equal(t, surfaceOpenAI, surfaceForPath("/v1/chat/completions"))
}

func TestFeedbackAppender_TerminalDetectionIsPerSurface(t *testing.T) {
	responses := &feedbackAppender{surface: surfaceResponses}
	assert.True(t, responses.isTerminalSSE([]byte("event: response.completed\n")))
	assert.False(t, responses.isTerminalSSE([]byte("event: message_stop\n")),
		"an Anthropic terminal must not be read as a Responses one")

	anthropic := &feedbackAppender{surface: surfaceAnthropic}
	assert.True(t, anthropic.isTerminalSSE([]byte("event: message_stop\n")))
	assert.False(t, anthropic.isTerminalSSE([]byte("data: [DONE]\n")))

	openai := &feedbackAppender{surface: surfaceOpenAI}
	assert.True(t, openai.isTerminalSSE([]byte("data: [DONE]\n")))
	assert.False(t, openai.isTerminalSSE([]byte("event: response.completed\n")))
}

// strings is used by the fixtures above.
var _ = strings.Contains
