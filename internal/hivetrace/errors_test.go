package hivetrace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUpstreamErrorMessage_ReadsEveryShapeTheGatewayWrites(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"anthropic json": {
			`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`,
			"This request would exceed your account's rate limit.",
		},
		"openai json":  {`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`, "quota exceeded"},
		"plain string": {`{"error":"bad gateway"}`, "bad gateway"},
		"anthropic stream": {
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"overloaded\"}}\n\n",
			"overloaded",
		},
		"responses stream": {
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit\",\"message\":\"usage limit reached\"}}}\n\n",
			"usage limit reached",
		},
	}
	for name, c := range cases {
		assert.Equal(t, c.want, upstreamErrorMessage([]byte(c.body)), name)
	}
}

// A successful Responses stream carries "error":null on every response object;
// reading that as a failure would mark every Codex turn as an error.
func TestUpstreamErrorMessage_SuccessfulStreamIsClean(t *testing.T) {
	body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"error\":null}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":null}}\n\n"
	assert.Empty(t, upstreamErrorMessage([]byte(body)))
	assert.Empty(t, upstreamErrorMessage([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}]}`)))
}

func TestUpstreamErrorMessage_IsBounded(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := upstreamErrorMessage([]byte(`{"error":{"message":"` + long + `"}}`))
	assert.LessOrEqual(t, len(got), maxErrorMessage+len("\u2026"))
}
