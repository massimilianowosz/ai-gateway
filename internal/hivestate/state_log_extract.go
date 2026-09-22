package hivestate

import (
	"context"
	"fmt"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// deltaBlockMaxTokens bounds a single appended block. Extraction latency is
// almost entirely generation time, so this is the knob that decides how long
// the caller waits.
const deltaBlockMaxTokens = 350

// extractIntoLog advances the conversation's state log by one block and returns
// the whole log as the state to put in the prompt.
//
// The saving does not come from extracting less — it comes from what the
// result does to the provider prefix cache. A re-extracted snapshot changes the
// head of the prompt, so every token after it is re-read at full price. A log
// only appends, so the earlier turns stay cached and only the new block is paid
// for.
//
// The returned ExtractionResult carries the rendered log as JSON so the rest of
// Process needs no special case, and the newest block's parsed state so
// HiveRoute still routes on where the task stands now.
func (h *HiveState) extractIntoLog(ctx context.Context, scope Scope, zones MessageZones, maxOutputTokens int, levels []config.HiveRouteLevel) (*ExtractionResult, error) {
	key := conversationKey(scope, zones.History)
	if key == "" {
		return nil, fmt.Errorf("state log: no history to key the conversation on")
	}

	log, found := h.logs.Get(key, zones.History)
	if !found {
		log = &StateLog{}
	} else if kept := log.MatchingBlocks(zones.History); kept < len(log.Blocks) {
		// Keep the blocks that still describe this history rather than
		// restarting: a restart changes every state item at once and throws
		// away the provider cache the log exists to earn.
		h.logger.Info("hivestate: state log truncated to matching prefix",
			"blocks_before", len(log.Blocks), "blocks_kept", kept,
			"history", len(zones.History))
		log.TruncateTo(kept)
	}

	covered := log.Covered()
	var promptTok, completionTok int
	if covered < len(zones.History) {
		newMessages := TruncateToolResponses(zones.History[covered:], 300)

		var res *ExtractionResult
		var err error
		if covered == 0 {
			res, err = h.extractor.ExtractWithBudget(ctx, newMessages, zones.Last, maxOutputTokens, levels)
		} else {
			// A delta describes a handful of new messages, not the whole
			// history, so it gets a budget sized for that. The caller's budget
			// is derived from the full history: applied here it let blocks run
			// to 900+ tokens, and since extraction latency is almost purely
			// generation time (~17ms/token) that put 16 seconds on the request
			// path for text the design wants to be small anyway.
			budget := h.counter.CountMessages(newMessages) / 3
			if budget > deltaBlockMaxTokens {
				budget = deltaBlockMaxTokens
			}
			if maxOutputTokens > 0 && budget > maxOutputTokens {
				budget = maxOutputTokens
			}
			res, err = h.extractor.ExtractDelta(ctx, log.Render(), newMessages, zones.Last, budget, levels)
		}
		if err != nil {
			return nil, err
		}

		if !log.Append(len(zones.History), res, historyHash(zones.History)) {
			return nil, fmt.Errorf("state log: block covering %d messages refused after %d", len(zones.History), covered)
		}
		h.logs.Put(key, log)
		promptTok, completionTok = res.PromptTokens, res.CompletionTokens

		// block is what this turn added; log_chars is what the whole state
		// costs. The two together say whether the delta contract is holding.
		h.logger.Info("hivestate: state log advanced",
			"blocks", len(log.Blocks),
			"new_messages", len(zones.History)-covered,
			"covered_messages", len(zones.History),
			"block_chars", len(res.JSON),
			"log_chars", len(log.Render()),
			"conv_key", key[:12],
			"block", res.JSON,
		)
	}

	current := log.Current()
	if current == nil {
		return nil, fmt.Errorf("state log: no usable state after %d blocks", len(log.Blocks))
	}

	return &ExtractionResult{
		State:            current,
		JSON:             log.Render(),
		Parts:            log.RenderParts(),
		PromptTokens:     promptTok,
		CompletionTokens: completionTok,
	}, nil
}
