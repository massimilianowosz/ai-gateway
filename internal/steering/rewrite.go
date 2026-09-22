package steering

import (
	"encoding/json"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// The three rewrites below share one contract with the livezone package:
// everything they do not change stays json.RawMessage, so no value is ever
// re-encoded from a parsed form. Only the envelope key order is not preserved,
// which is not part of the prompt.
//
// The verbosity note goes at the *end* of the system prompt on purpose. A
// provider prompt cache matches the longest common prefix, so appending leaves
// the whole prompt ahead of the note byte-identical, and because the note is a
// constant the request that follows it is stable too — one cold turn when an
// operator first enables this, cached from then on. Prepending would move
// every token in the conversation on every request.

func steerAnthropic(body []byte, cfg config.SteeringConfig) ([]byte, Actions) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, Actions{}
	}
	note := strings.TrimSpace(cfg.VerbosityNote)
	if note == "" {
		return nil, Actions{}
	}
	sys, ok := appendAnthropicSystem(env["system"], note)
	if !ok {
		return nil, Actions{}
	}
	env["system"] = sys
	out, err := json.Marshal(env)
	if err != nil {
		return nil, Actions{}
	}
	return out, Actions{Verbosity: true}
}

func steerOpenAI(body []byte, cfg config.SteeringConfig) ([]byte, Actions) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, Actions{}
	}
	note := strings.TrimSpace(cfg.VerbosityNote)
	if note == "" {
		return nil, Actions{}
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(env["messages"], &msgs); err != nil {
		return nil, Actions{}
	}
	i, ok := openAISystemIndex(msgs)
	if !ok {
		return nil, Actions{}
	}
	msg, ok := appendOpenAISystem(msgs[i], note)
	if !ok {
		return nil, Actions{}
	}
	msgs[i] = msg
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return nil, Actions{}
	}
	env["messages"] = newMsgs
	out, err := json.Marshal(env)
	if err != nil {
		return nil, Actions{}
	}
	return out, Actions{Verbosity: true}
}

func steerResponses(body []byte, cfg config.SteeringConfig) ([]byte, Actions) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, Actions{}
	}
	note := strings.TrimSpace(cfg.VerbosityNote)
	if note == "" {
		return nil, Actions{}
	}
	// The Responses API carries the system prompt in "instructions", outside
	// the input array — which is why appending there cannot disturb the
	// conversation at all.
	instructions, ok := appendInstructions(env["instructions"], note)
	if !ok {
		return nil, Actions{}
	}
	env["instructions"] = instructions
	out, err := json.Marshal(env)
	if err != nil {
		return nil, Actions{}
	}
	return out, Actions{Verbosity: true}
}

// --- system prompt appends ---

// appendAnthropicSystem handles both shapes the system field takes: a bare
// string, or an array of content blocks. A note added as a new trailing block
// sits after any cache_control breakpoint the caller set, so the cached prefix
// is untouched.
func appendAnthropicSystem(raw json.RawMessage, note string) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		out, err := json.Marshal(note)
		return out, err == nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.Contains(text, note) {
			return nil, false
		}
		out, err := json.Marshal(text + "\n\n" + note)
		return out, err == nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	for _, b := range blocks {
		if strings.Contains(string(b), note) {
			return nil, false
		}
	}
	block, err := json.Marshal(map[string]string{"type": "text", "text": note})
	if err != nil {
		return nil, false
	}
	out, err := json.Marshal(append(blocks, block))
	return out, err == nil
}

// openAISystemIndex returns the index of the leading system/developer message.
// Without one there is nothing to append to: adding a system message of our
// own would change the first token of the prompt on every request.
func openAISystemIndex(msgs []json.RawMessage) (int, bool) {
	last := -1
	for i, raw := range msgs {
		var m struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			last = i
			continue
		}
		break // only the leading run counts
	}
	return last, last >= 0
}

func appendOpenAISystem(raw json.RawMessage, note string) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, false
	}
	if strings.Contains(string(msg["content"]), note) {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(msg["content"], &text); err == nil {
		nc, err := json.Marshal(text + "\n\n" + note)
		if err != nil {
			return nil, false
		}
		msg["content"] = nc
	} else {
		var parts []json.RawMessage
		if err := json.Unmarshal(msg["content"], &parts); err != nil {
			return nil, false
		}
		part, err := json.Marshal(map[string]string{"type": "text", "text": note})
		if err != nil {
			return nil, false
		}
		nc, err := json.Marshal(append(parts, part))
		if err != nil {
			return nil, false
		}
		msg["content"] = nc
	}
	out, err := json.Marshal(msg)
	return out, err == nil
}

func appendInstructions(raw json.RawMessage, note string) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		out, err := json.Marshal(note)
		return out, err == nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, false
	}
	if strings.Contains(text, note) {
		return nil, false
	}
	out, err := json.Marshal(text + "\n\n" + note)
	return out, err == nil
}
