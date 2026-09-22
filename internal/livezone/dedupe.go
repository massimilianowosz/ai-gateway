package livezone

import (
	"encoding/json"
	"strings"
)

// dedupeRepeatsAnthropic replaces a tool result whose text already appears earlier in
// the same request with a one-line reference to it.
//
// A coding agent reads the same file several times in a session — after an
// edit, after a failing test, to check itself — and every reading enters the
// prompt in full. The content is not lost by stating it once: the first copy
// is left exactly as it was, line numbers and all, so anything the agent
// quotes back still resolves.
//
// Only later copies are replaced, which is what keeps this safe with a
// provider prefix cache: the earliest bytes never move, and because the rule
// is a pure function of the request body, the same conversation yields the
// same output on every turn.
//
// It ignores the protected-tools list on purpose. A repeated Read is precisely
// the payload worth removing, and removing a *copy* does not carry the risk
// that compressing the original would: no offset shifts, because the original
// is still there.
func dedupeRepeatsAnthropic(msgs []json.RawMessage, start int, pol Policy, names map[string]string, stats *Stats) bool {
	idx := newDedupeIndex(pol)
	changed := false

	for i, raw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		blocks, ok := contentBlocks(msg)
		if !ok {
			continue
		}

		msgChanged := false
		for j, b := range blocks {
			var block map[string]json.RawMessage
			if err := json.Unmarshal(b, &block); err != nil {
				continue
			}
			var typ string
			if err := json.Unmarshal(block["type"], &typ); err != nil || typ != "tool_result" {
				continue
			}
			text, ok := resultText(block["content"])
			if !ok || len(text) < pol.Options.MinBytes {
				continue
			}

			ref, exact, repeat := idx.reference(text)
			if !repeat {
				idx.observe(text, describeResult(block, names))
				continue
			}
			// Everything before the live zone is left exactly as it arrived.
			if i < start {
				continue
			}

			marker, err := json.Marshal(ref)
			if err != nil {
				continue
			}
			block["content"] = marker
			nb, err := json.Marshal(block)
			if err != nil {
				continue
			}
			blocks[j] = nb
			msgChanged = true
			stats.record(Result{
				Applied: true, Kind: KindUnknown,
				BytesBefore: len(text), BytesAfter: len(marker),
			}, dedupeTransformer(exact))
		}

		if !msgChanged {
			continue
		}
		nc, err := json.Marshal(blocks)
		if err != nil {
			continue
		}
		msg["content"] = nc
		out, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		msgs[i] = out
		changed = true
	}

	return changed
}

// contentBlocks returns a message's content as blocks, or false when it is a
// bare string and so carries no tool_result.
func contentBlocks(msg map[string]json.RawMessage) ([]json.RawMessage, bool) {
	raw, ok := msg["content"]
	if !ok {
		return nil, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// dedupeRepeatsOpenAI is dedupeRepeatsAnthropic for /v1/chat/completions:
// a role="tool" message carries exactly one result, as a bare string rather
// than the array of blocks Anthropic uses, so there is no per-block loop.
func dedupeRepeatsOpenAI(msgs []json.RawMessage, start int, pol Policy, names map[string]string, stats *Stats) bool {
	idx := newDedupeIndex(pol)
	changed := false

	for i, raw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		var role string
		if err := json.Unmarshal(msg["role"], &role); err != nil || role != "tool" {
			continue
		}
		var text string
		if err := json.Unmarshal(msg["content"], &text); err != nil || len(text) < pol.Options.MinBytes {
			continue // structured content, or too small to bother deduping
		}

		ref, exact, repeat := idx.reference(text)
		if !repeat {
			tool := resultToolName(openAIToolName(msg), rawString(msg, "tool_call_id"), names)
			idx.observe(text, describeToolResult(tool))
			continue
		}
		// Everything before the live zone is left exactly as it arrived.
		if i < start {
			continue
		}

		marker, err := json.Marshal(ref)
		if err != nil {
			continue
		}
		msg["content"] = marker
		out, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		msgs[i] = out
		changed = true
		stats.record(Result{
			Applied: true, Kind: KindUnknown,
			BytesBefore: len(text), BytesAfter: len(marker),
		}, dedupeTransformer(exact))
	}

	return changed
}

// dedupeRepeatsResponses is dedupeRepeatsAnthropic for /v1/responses:
// function_call_output (and its computer/custom-tool equivalents) carry
// "output" as a bare string or an array of content parts, the same two
// shapes Anthropic's tool_result content takes.
func dedupeRepeatsResponses(items []json.RawMessage, start int, pol Policy, names map[string]string, stats *Stats) bool {
	idx := newDedupeIndex(pol)
	changed := false

	for i, raw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		var typ string
		if err := json.Unmarshal(item["type"], &typ); err != nil {
			continue
		}
		switch typ {
		// A LocalShellCall's result is not a distinct wire type — it comes
		// back as an ordinary function_call_output, so that's covered too.
		case "function_call_output", "custom_tool_call_output", "computer_call_output":
		default:
			continue
		}
		text, ok := resultText(item["output"])
		if !ok || len(text) < pol.Options.MinBytes {
			continue
		}

		ref, exact, repeat := idx.reference(text)
		if !repeat {
			tool := resultToolName("", rawString(item, "call_id"), names)
			idx.observe(text, describeToolResult(tool))
			continue
		}
		if i < start {
			continue
		}

		marker, err := json.Marshal(ref)
		if err != nil {
			continue
		}
		item["output"] = marker
		out, err := json.Marshal(item)
		if err != nil {
			continue
		}
		items[i] = out
		changed = true
		stats.record(Result{
			Applied: true, Kind: KindUnknown,
			BytesBefore: len(text), BytesAfter: len(marker),
		}, dedupeTransformer(exact))
	}

	return changed
}

// dedupeTransformer names the two cases apart in the metrics: an exact repeat
// collapses to one line, a near repeat to a line plus the changed region, and
// averaging the two would hide which one is doing the work.
func dedupeTransformer(exact bool) string {
	if exact {
		return "dedupe_repeat"
	}
	return "dedupe_delta"
}

// describeToolResult names a result by tool name alone, for the two formats
// (OpenAI, Responses) where a result carries no block map to inspect beyond
// the id already resolved into a name.
func describeToolResult(tool string) string {
	if tool != "" {
		return tool + " result"
	}
	return "tool result"
}

// resultText flattens a tool result's content to the text it carries. A result
// holding an image, or anything else that is not text, reports false: two
// payloads that happen to share their text are not interchangeable.
//
// "text" is Anthropic's tool_result content-part type; "output_text" is a
// message item's content-part type in the Responses API. Neither is what a
// function_call_output/custom_tool_call_output actually carries: Codex CLI
// tags those "input_text" (see FunctionCallOutputContentItem in openai/codex's
// protocol crate), the same content-item type it uses for input_image and
// input_audio. Anthropic bodies never carry either Responses tag, so
// accepting all three here is safe for either caller.
func resultText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false
	}
	var sb strings.Builder
	for _, p := range parts {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p, &part); err != nil || (part.Type != "text" && part.Type != "output_text" && part.Type != "input_text") {
			return "", false
		}
		sb.WriteString(part.Text)
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}

// describeResult names the first copy well enough for the agent to find it.
func describeResult(block map[string]json.RawMessage, names map[string]string) string {
	tool := resultToolName(anthropicToolName(block), rawString(block, "tool_use_id"), names)
	return describeToolResult(tool)
}
