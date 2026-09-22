package cache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Message represents a chat message for cache purposes.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ExtractQuery returns the last user message — this is the "query" for embedding.
func ExtractQuery(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	// Fallback: concatenate all messages
	var parts []string
	for _, m := range messages {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, " ")
}

// ExtractContext returns the conversation context (everything except the last user message).
// This represents the "pinned memory + relevant window" for dual embedding.
func ExtractContext(messages []Message) string {
	if len(messages) <= 1 {
		return ""
	}

	// Find last user message index
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = i
			break
		}
	}

	// Context is everything before the last user message
	var parts []string
	for i := 0; i < lastUser && i < len(messages); i++ {
		if messages[i].Content != "" {
			parts = append(parts, messages[i].Role+": "+messages[i].Content)
		}
	}

	// Limit context to last ~2000 chars (relevant window, avoid embedding noise)
	joined := strings.Join(parts, "\n")
	if len(joined) > 2000 {
		joined = joined[len(joined)-2000:]
	}
	return joined
}

func marshalMessages(messages []Message) string {
	b, _ := json.Marshal(messages)
	return string(b)
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
