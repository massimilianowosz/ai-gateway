package hivestate

// MessageZones splits a conversation into protected and extractable zones.
type MessageZones struct {
	System  []Message // system/developer instructions — forwarded unchanged
	History []Message // older turns — input for state extraction
	Recent  []Message // recent turns (within window) — forwarded unchanged
	Last    Message   // last user message — forwarded unchanged
}

// SplitMessages separates messages into protected zones and extractable history.
// System messages at the head are protected. The last user message is protected.
// Recent messages within the step window are preserved uncompressed.
// Only older history beyond the window is eligible for state extraction.
//
// Split by assistant "steps" — each assistant message = one agent step.
// Keeps the last stepWindow steps (+ their tool results) intact.
// Works for both agentic (many tools, few human msgs) and conversational (many exchanges).
func SplitMessages(messages []Message, stepWindow int) MessageZones {
	if len(messages) == 0 {
		return MessageZones{}
	}

	if stepWindow <= 0 {
		stepWindow = 4
	}

	// Find the boundary between system/developer messages and conversation history.
	systemEnd := 0
	for i, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			systemEnd = i + 1
		} else {
			break
		}
	}

	// Find the last user message (human, not tool response).
	lastUserIdx := -1
	for i := len(messages) - 1; i >= systemEnd; i-- {
		if messages[i].Role == "user" && messages[i].ToolCallID == "" {
			lastUserIdx = i
			break
		}
	}

	// If no user message found after system, treat entire conversation as pass-through.
	if lastUserIdx < 0 {
		return MessageZones{
			System: messages,
		}
	}

	zones := MessageZones{
		System: messages[:systemEnd],
		Last:   messages[lastUserIdx],
	}

	// The "body" is everything that isn't system or the Last message itself.
	// This includes messages BEFORE and AFTER the last human message
	// (tool chains from the current turn come AFTER the human message).
	var body []Message
	for i := systemEnd; i < len(messages); i++ {
		if i == lastUserIdx {
			continue // skip Last, it's preserved separately
		}
		body = append(body, messages[i])
	}

	if len(body) > 0 {
		// Find assistant message indices (each = one agent step)
		stepStarts := []int{}
		for i, m := range body {
			if m.Role == "assistant" {
				stepStarts = append(stepStarts, i)
			}
		}

		if len(stepStarts) > stepWindow {
			// Split at the step boundary: keep last stepWindow steps
			splitStepIdx := len(stepStarts) - stepWindow
			splitPoint := stepStarts[splitStepIdx]
			zones.History = body[:splitPoint]
			zones.Recent = body[splitPoint:]
		} else {
			// Not enough steps — nothing to compress
			zones.Recent = body
		}
	}

	return zones
}
