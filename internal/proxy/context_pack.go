package proxy

import (
	"context"
	"encoding/json"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// injectContextPackInstructions merges the calling key's governed context
// pack bundle into the request's system message, ahead of anything the
// caller supplied.
//
// The bundle is never computed here: Ubiquum's portal resolves it (which
// packs are published and bound to this agent/environment, in priority
// order) and pushes the assembled text onto the key row whenever a pack is
// published or a binding changes (see gateway_client.update_key). The
// gateway's only job is to read what's already on the key it just used to
// authenticate — no request-time lookup, no knowledge of packs or bindings.
//
// A key with nothing bound carries an empty string, so this is a no-op for
// the common case.
//
// It merges into a single system message rather than adding a second one:
// at least one downstream provider encoder (Anthropic's) keeps only the last
// "system"-role message it sees when converting back to the upstream wire
// format, so two separate system messages would silently drop one instead
// of both applying.
func injectContextPackInstructions(ctx context.Context, req *provider.CompletionRequest) {
	key := auth.KeyInfoFromContext(ctx)
	if key == nil || key.ContextInstructions == "" {
		return
	}

	for i := range req.Messages {
		if req.Messages[i].Role != "system" {
			continue
		}
		if existing, ok := req.Messages[i].Content.(string); ok {
			if existing == "" {
				req.Messages[i].Content = key.ContextInstructions
			} else {
				req.Messages[i].Content = key.ContextInstructions + "\n\n" + existing
			}
			return
		}
		// Non-string system content (already-structured content parts): fall
		// through to prepending a new leading message. This keeps the common
		// case (a plain-text system message, which is nearly all traffic)
		// correct, at the cost of the same last-system-wins limitation the
		// Anthropic provider encoder already has for any caller that sends
		// more than one system message today.
		break
	}

	system := provider.Message{Role: "system", Content: key.ContextInstructions}
	req.Messages = append([]provider.Message{system}, req.Messages...)
}

// mergeInstructionsField prepends contextInstructions to a raw Responses API
// "instructions" field (a plain JSON string, or absent) for the native
// passthrough path, which forwards the client's own JSON rather than a
// re-encoded struct. Any non-string existing value is left alone rather than
// guessed at.
func mergeInstructionsField(existing json.RawMessage, contextInstructions string) (json.RawMessage, error) {
	if len(existing) == 0 {
		return json.Marshal(contextInstructions)
	}
	var existingText string
	if err := json.Unmarshal(existing, &existingText); err != nil {
		// Not a plain string (e.g. already null or some other shape this
		// gateway does not expect for Responses' instructions field): leave
		// it exactly as the caller sent it rather than risk corrupting it.
		return existing, nil
	}
	if existingText == "" {
		return json.Marshal(contextInstructions)
	}
	return json.Marshal(contextInstructions + "\n\n" + existingText)
}
