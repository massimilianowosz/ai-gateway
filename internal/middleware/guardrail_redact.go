package middleware

import (
	"bytes"
	"encoding/json"
)

// redactBody rewrites the text a request puts in front of the model — chat
// message content, and the Responses API instructions/input — replacing PII
// with [ENTITY] placeholders. It is the counterpart of extractMessages: what
// the scanner reads is exactly what redaction can rewrite.
//
// The body is returned untouched, with no entities, when nothing matched or
// when it is not the JSON shape we understand.
func redactBody(body []byte, redact func(string) (string, []string)) ([]byte, []string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep 1.0 from becoming 1 on the way out
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return body, nil
	}

	var found []string
	apply := func(s string) string {
		out, entities := redact(s)
		found = appendUniqueAll(found, entities)
		return out
	}

	changed := false
	if messages, ok := root["messages"].([]any); ok {
		for _, m := range messages {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := redactContent(msg["content"], apply); ok {
				msg["content"] = content
				changed = true
			}
		}
	}
	// Anthropic carries the system prompt outside the messages array.
	if system, ok := redactContent(root["system"], apply); ok {
		root["system"] = system
		changed = true
	}
	if instructions, ok := root["instructions"].(string); ok {
		if r := apply(instructions); r != instructions {
			root["instructions"] = r
			changed = true
		}
	}
	switch input := root["input"].(type) {
	case string:
		if r := apply(input); r != input {
			root["input"] = r
			changed = true
		}
	case []any:
		for _, i := range input {
			item, ok := i.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := redactContent(item["content"], apply); ok {
				item["content"] = content
				changed = true
				continue
			}
			if redactBlock(item, apply, 0) {
				changed = true
			}
		}
	}
	switch prompt := root["prompt"].(type) {
	case string:
		if r := apply(prompt); r != prompt {
			root["prompt"] = r
			changed = true
		}
	case []any:
		for index, item := range prompt {
			text, ok := item.(string)
			if !ok {
				continue
			}
			if r := apply(text); r != text {
				prompt[index] = r
				changed = true
			}
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	return out, found
}

// redactContent handles both plain string content and the content-block arrays
// used by Anthropic and the Responses API. It reports whether it rewrote
// anything, so an untouched body is never re-serialised.
func redactContent(raw any, apply func(string) string) (any, bool) {
	return redactContentDepth(raw, apply, 0)
}

func redactContentDepth(raw any, apply func(string) string, depth int) (any, bool) {
	if depth > maxContentBlockDepth {
		return nil, false
	}
	switch content := raw.(type) {
	case string:
		if r := apply(content); r != content {
			return r, true
		}
	case []any:
		changed := false
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if redactBlock(block, apply, depth) {
				changed = true
			}
		}
		if changed {
			return content, true
		}
	}
	return nil, false
}

// redactBlock rewrites one content block. Tool results and function call
// outputs nest their payload one level deeper, and that payload is exactly
// where a file an agent just read ends up.
func redactBlock(block map[string]any, apply func(string) string, depth int) bool {
	switch block["type"] {
	case "text", "input_text", "output_text":
		text, ok := block["text"].(string)
		if !ok {
			return false
		}
		if redacted := apply(text); redacted != text {
			block["text"] = redacted
			return true
		}
	case "tool_result":
		if content, ok := redactContentDepth(block["content"], apply, depth+1); ok {
			block["content"] = content
			return true
		}
	case "function_call_output":
		if output, ok := redactContentDepth(block["output"], apply, depth+1); ok {
			block["output"] = output
			return true
		}
	}
	return false
}

func appendUniqueAll(dst []string, values []string) []string {
	for _, v := range values {
		seen := false
		for _, d := range dst {
			if d == v {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, v)
		}
	}
	return dst
}
