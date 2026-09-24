package hivetrace

import (
	"encoding/json"
	"strings"
)

// Client-declared conversation identity.
//
// Deriving a session from the prompt was always a fallback: it hashes the
// system messages plus the first user turn, and an agent that rewrites either
// of those mid-run — as Claude Code does, injecting reminders and todo state —
// appears as a new session every time the anchor shifts. One run then shows up
// as several rows, which is wrong in the session list and wrong in the spend
// attribution built on top of it.
//
// Clients that already know their own conversation id say so. Anthropic's API
// carries it in metadata.user_id, and Claude Code packs a JSON object in there
// whose session_id is exactly the identifier it uses for the conversation.
// Using it makes the session a fact rather than an inference, the same move as
// the agent fingerprint.

// clientSessionID reads a conversation id the caller declared, or returns
// empty so the caller falls back to deriving one.
func clientSessionID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	raw := strings.TrimSpace(req.Metadata.UserID)
	if raw == "" {
		return ""
	}

	// Claude Code nests a JSON document inside the string. Other clients put a
	// plain identifier there, which is just as usable.
	var nested struct {
		SessionID string `json:"session_id"`
	}
	if strings.HasPrefix(raw, "{") {
		if err := json.Unmarshal([]byte(raw), &nested); err == nil {
			// Only the session id is taken. The same object also carries a
			// device_id, which identifies the machine across every
			// conversation and is no business of a traffic log.
			return strings.TrimSpace(nested.SessionID)
		}
		return ""
	}
	return raw
}
