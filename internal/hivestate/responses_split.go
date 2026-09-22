package hivestate

import "encoding/json"

// Where to cut a Responses conversation so the older part can be replaced by
// state without corrupting the newer part.
//
// The rewriter used to split on user messages, keeping the last N of them. That
// conflated two different things. The window exists to protect in-flight tool
// activity — an output must never be separated from the call that produced it —
// and "N user messages" was only an indirect way of landing on a safe spot. On
// agentic traffic it also measures the wrong thing entirely: Codex runs dozens
// of tool calls between two human messages, so a window of 10 user turns spans
// nearly the whole session and nothing is ever compressed.
//
// The unit here is the agent step, matching what step_window means everywhere
// else, and the call/output pairing is enforced directly rather than hoped for.

// responsesItem is the part of an input item this file reasons about.
type responsesItem struct {
	Type   string `json:"type"`
	Role   string `json:"role"`
	CallID string `json:"call_id"`
}

func parseResponsesItem(raw json.RawMessage) responsesItem {
	var it responsesItem
	_ = json.Unmarshal(raw, &it)
	return it
}

// isResponsesStepStart reports whether an item begins an agent step: a tool
// invocation, or something the assistant said.
func isResponsesStepStart(raw json.RawMessage) bool {
	it := parseResponsesItem(raw)
	switch it.Type {
	case "function_call", "custom_tool_call", "local_shell_call", "computer_call":
		return true
	case "message":
		return it.Role == "assistant"
	}
	return false
}

// responsesCallID returns the call this item invokes, and whether it is a call.
func responsesCallID(raw json.RawMessage) (string, bool) {
	it := parseResponsesItem(raw)
	switch it.Type {
	case "function_call", "custom_tool_call", "local_shell_call", "computer_call":
		return it.CallID, true
	}
	return "", false
}

// responsesOutputCallID returns the call this item answers, and whether it is
// an output. local_shell_call results come back as ordinary
// function_call_output, so there is no separate type to match.
func responsesOutputCallID(raw json.RawMessage) (string, bool) {
	it := parseResponsesItem(raw)
	switch it.Type {
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		return it.CallID, true
	}
	return "", false
}

// responsesPreambleEnd returns how many leading items must be forwarded
// untouched.
//
// Codex leaves the instructions field nearly empty and puts its tool schema
// (additional_tools) and system prompt in the first input items instead —
// around 83KB of them. They are not conversation and must never be summarised:
// dropping them takes the agent's tools away, and rewriting the very first item
// also guarantees the provider re-reads the entire prompt at full price.
func responsesPreambleEnd(items []json.RawMessage) int {
	n := 0
	for _, raw := range items {
		it := parseResponsesItem(raw)
		if it.Type == "additional_tools" {
			n++
			continue
		}
		if it.Type == "message" && (it.Role == "developer" || it.Role == "system") {
			n++
			continue
		}
		break
	}
	return n
}

// responsesSplitPoint returns the index at which the verbatim tail must begin.
//
// It keeps the last `steps` agent steps, then moves the cut earlier until no
// kept output refers to a call that would be compressed away. A return of 0
// means there is nothing older than the window and the body should be left
// alone.
func responsesSplitPoint(items []json.RawMessage, steps int) int {
	if steps <= 0 {
		steps = 4
	}

	var stepStarts []int
	for i, raw := range items {
		if isResponsesStepStart(raw) {
			stepStarts = append(stepStarts, i)
		}
	}
	if len(stepStarts) <= steps {
		return 0
	}

	split := stepStarts[len(stepStarts)-steps]

	callAt := make(map[string]int, len(items))
	for i, raw := range items {
		if id, ok := responsesCallID(raw); ok && id != "" {
			if _, seen := callAt[id]; !seen {
				callAt[id] = i
			}
		}
	}

	// Each pass can only move the cut earlier, which can expose further
	// orphans, so repeat until it settles.
	for {
		earliest := split
		for i := split; i < len(items); i++ {
			id, ok := responsesOutputCallID(items[i])
			if !ok || id == "" {
				continue
			}
			if at, found := callAt[id]; found && at < earliest {
				earliest = at
			}
		}
		if earliest == split {
			return split
		}
		split = earliest
	}
}
