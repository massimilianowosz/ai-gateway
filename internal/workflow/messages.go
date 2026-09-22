package workflow

import (
	"encoding/json"
	"strings"
)

// wfMessage is a simplified message sent to the backend as execution context.
type wfMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// promptIndexWholeInput marks a Responses request whose "input" is a bare
// string rather than a list of items: there is no item index to rewrite.
const promptIndexWholeInput = -2

// extractPrompt returns a simplified message list, the text of the last user
// turn (the "prompt" a workflow transforms), and the index that text came from.
//
// The index matters. A turn whose content carries no text — an image-only
// message, or an Anthropic tool_result — yields "" and is skipped, so the last
// user turn the extractor chose is not always the last user turn in the array.
// Rewriting by role alone therefore wrote the workflow's answer into a
// different message than the one it was given. An index of -1 means there is
// nothing to rewrite.
func extractPrompt(body []byte, isAnthropic, isResponses bool) ([]wfMessage, string, int) {
	switch {
	case isAnthropic:
		return extractAnthropicPrompt(body)
	case isResponses:
		return extractResponsesPrompt(body)
	default:
		return extractOpenAIPrompt(body)
	}
}

// contentText extracts plain text from a content field that may be a plain
// string or an array of content blocks (e.g. [{"type":"text","text":"..."}]).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if (b.Type == "text" || b.Type == "input_text" || b.Type == "output_text") && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func extractOpenAIPrompt(body []byte) ([]wfMessage, string, int) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", -1
	}
	var out []wfMessage
	lastUser, lastUserIdx := "", -1
	for i, m := range req.Messages {
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		out = append(out, wfMessage{Role: m.Role, Content: text})
		if m.Role == "user" {
			lastUser, lastUserIdx = text, i
		}
	}
	return out, lastUser, lastUserIdx
}

func extractAnthropicPrompt(body []byte) ([]wfMessage, string, int) {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", -1
	}
	var out []wfMessage
	if sysText := contentText(req.System); sysText != "" {
		out = append(out, wfMessage{Role: "system", Content: sysText})
	}
	lastUser, lastUserIdx := "", -1
	for i, m := range req.Messages {
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		out = append(out, wfMessage{Role: m.Role, Content: text})
		if m.Role == "user" {
			lastUser, lastUserIdx = text, i
		}
	}
	return out, lastUser, lastUserIdx
}

func extractResponsesPrompt(body []byte) ([]wfMessage, string, int) {
	var req struct {
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", -1
	}
	var out []wfMessage
	if strings.TrimSpace(req.Instructions) != "" {
		out = append(out, wfMessage{Role: "system", Content: req.Instructions})
	}

	// input may be a plain string...
	var plain string
	if json.Unmarshal(req.Input, &plain) == nil {
		if plain == "" {
			return out, "", -1
		}
		out = append(out, wfMessage{Role: "user", Content: plain})
		return out, plain, promptIndexWholeInput
	}

	// ...or an array of input items.
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return out, "", -1
	}
	lastUser, lastUserIdx := "", -1
	for i, item := range items {
		if item.Type != "message" && item.Type != "" {
			continue
		}
		text := contentText(item.Content)
		if text == "" {
			continue
		}
		out = append(out, wfMessage{Role: item.Role, Content: text})
		if item.Role == "user" {
			lastUser, lastUserIdx = text, i
		}
	}
	return out, lastUser, lastUserIdx
}

// replaceContentText rewrites the text carried by a content field, leaving its
// structure and every non-text part exactly where they were.
//
// A content field is either a plain string or an array of typed blocks. The
// extractor joins every text block into one string, so the replacement collapses
// back the same way: the first text block takes the new text and the remaining
// text blocks go, while images, files and tool results stay untouched.
//
// Replacing the whole field with a bare string — which is what this used to do —
// destroyed all of that. On Anthropic it turned a tool_result turn into a
// request the provider rejects outright with "tool_use ids were found without
// tool_result blocks", so every tool-using turn broke as soon as a workflow was
// configured.
//
// Reports false when the field carries no text to replace, in which case the
// caller must leave it alone rather than overwrite it.
func replaceContentText(raw json.RawMessage, newText string) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		encoded, err := json.Marshal(newText)
		if err != nil {
			return raw, false
		}
		return encoded, true
	}

	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return raw, false
	}
	out := make([]json.RawMessage, 0, len(blocks))
	replaced := false
	for _, block := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &probe) != nil || !isTextBlockType(probe.Type) {
			out = append(out, block)
			continue
		}
		if replaced {
			// The joined text already went into the first text block; keeping
			// the rest would repeat what the workflow replaced.
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(block, &fields) != nil {
			out = append(out, block)
			continue
		}
		encoded, err := json.Marshal(newText)
		if err != nil {
			out = append(out, block)
			continue
		}
		fields["text"] = encoded
		rebuilt, err := json.Marshal(fields)
		if err != nil {
			out = append(out, block)
			continue
		}
		out = append(out, rebuilt)
		replaced = true
	}
	if !replaced {
		return raw, false
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func isTextBlockType(t string) bool {
	return t == "text" || t == "input_text" || t == "output_text"
}

// rewriteLastUserText replaces the text of the turn the extractor read the
// prompt from, leaving system prompts, tool calls, and earlier history alone.
//
// idx is the index extractPrompt returned. Searching for the target again by
// role would find a different message whenever the extractor skipped one for
// carrying no text.
func rewriteLastUserText(body []byte, newText string, idx int, isAnthropic, isResponses bool) ([]byte, error) {
	if idx == -1 {
		return body, nil
	}
	if isResponses {
		return rewriteResponsesLastUser(body, newText, idx)
	}
	return rewriteMessagesLastUser(body, newText, idx)
}

// rewriteMessagesLastUser handles both OpenAI ("messages") and Anthropic
// ("messages", with "system" left alone) request shapes — structurally
// identical for this purpose: a top-level array of {role, content} objects.
func rewriteMessagesLastUser(body []byte, newText string, idx int) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	msgsRaw, ok := doc["messages"]
	if !ok {
		return body, nil
	}
	var rawMsgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &rawMsgs); err != nil {
		return nil, err
	}
	if idx < 0 || idx >= len(rawMsgs) {
		return body, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs[idx], &m); err != nil {
		return nil, err
	}
	content, ok := replaceContentText(m["content"], newText)
	if !ok {
		return body, nil
	}
	m["content"] = content
	newMsgJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	rawMsgs[idx] = newMsgJSON

	msgsJSON, err := json.Marshal(rawMsgs)
	if err != nil {
		return nil, err
	}
	doc["messages"] = msgsJSON
	return json.Marshal(doc)
}

// rewriteResponsesLastUser handles the Responses API "input" field, which is
// either a bare string or a list of items.
func rewriteResponsesLastUser(body []byte, newText string, idx int) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	inputRaw, ok := doc["input"]
	if !ok {
		return body, nil
	}

	if idx == promptIndexWholeInput {
		newInput, err := json.Marshal(newText)
		if err != nil {
			return nil, err
		}
		doc["input"] = newInput
		return json.Marshal(doc)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, err
	}
	if idx < 0 || idx >= len(items) {
		return body, nil
	}

	// The item is edited in place rather than rebuilt: a fresh
	// {type, role, content} discarded its id and status, and every
	// input_image or input_file part along with them.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(items[idx], &fields); err != nil {
		return nil, err
	}
	content, ok := replaceContentText(fields["content"], newText)
	if !ok {
		return body, nil
	}
	fields["content"] = content
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	items[idx] = rebuilt

	itemsJSON, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	doc["input"] = itemsJSON
	return json.Marshal(doc)
}

// overrideModel rewrites the top-level "model" field, which is shaped the
// same way across chat completions, Anthropic messages, and the Responses API.
func overrideModel(body []byte, model string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	doc["model"] = modelJSON
	return json.Marshal(doc)
}

// isStreamingRequest reports whether the request body sets "stream": true.
func isStreamingRequest(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Stream
}
