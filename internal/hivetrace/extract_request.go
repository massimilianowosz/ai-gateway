package hivetrace

import (
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
)

// apiFor maps a request path to the inbound surface, in both the form stored
// on the event and the form hivestate's parsers expect.
func apiFor(path string) (string, hivestate.APIFlavor) {
	switch {
	case strings.HasSuffix(path, "/messages"):
		return APIMessages, hivestate.APIAnthropic
	case strings.HasSuffix(path, "/responses"):
		return APIResponses, hivestate.APIResponses
	default:
		return APIChatCompletions, hivestate.APIOpenAI
	}
}

// newInputText returns the portion of a conversation that arrived this turn:
// everything after the last assistant message.
//
// Scanning the whole request instead would report the same pasted credential
// again on every following turn, because a chat request carries the entire
// history — a fifty-turn session would show fifty copies of one exposure. The
// tail is what the caller actually sent this time: the new user message, and
// the tool results the agent fed back, which is where a .env file read by a
// Read tool shows up.
func newInputText(messages []hivestate.Message) string {
	start := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			start = i + 1
			break
		}
	}
	var b strings.Builder
	for _, m := range messages[start:] {
		if m.Content == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Content)
	}
	return b.String()
}
