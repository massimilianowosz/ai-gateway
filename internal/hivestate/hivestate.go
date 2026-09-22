package hivestate

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// HiveState is the intent-aware conversational state engine.
type HiveState struct {
	cfg       config.HiveStateConfig
	extractor *StateExtractor
	counter   TokenCounter
	logger    *slog.Logger
	cache     *stateCache
	logs      *memoryLogStore
	ccr       *CCRStore
}

// CompressionOverrides lets a per-key context_compression policy replace one
// or more of the engine's configured behaviors for that key alone. A nil
// field means "use the engine's own config value". live_compression is a
// separate, global-only subsystem (internal/livezone) with no per-key
// override path at all and is not covered here.
type CompressionOverrides struct {
	Threshold       *int
	TokenBudget     *int
	StepWindow      *int
	AppendOnlyState *bool
}

// WithOverrides returns a HiveState that behaves like h except for the
// fields overrides sets, sharing every cache, counter and extractor h
// already has — only the plain-value cfg differs on the returned copy. Used
// once per request, before any cfg field is read, so every downstream
// decision (the prefix-cache guard included) sees the same effective config.
func (h *HiveState) WithOverrides(overrides CompressionOverrides) *HiveState {
	if overrides == (CompressionOverrides{}) {
		return h
	}
	clone := *h
	if overrides.Threshold != nil {
		clone.cfg.Threshold = *overrides.Threshold
	}
	if overrides.TokenBudget != nil {
		clone.cfg.TokenBudget = *overrides.TokenBudget
	}
	if overrides.StepWindow != nil {
		clone.cfg.StepWindow = *overrides.StepWindow
	}
	if overrides.AppendOnlyState != nil {
		clone.cfg.AppendOnlyState = *overrides.AppendOnlyState
	}
	return &clone
}

// New creates a HiveState engine. If model is configured and available in the
// registry, state extraction is enabled. Otherwise HiveState only passes through.
func New(cfg config.HiveStateConfig, registry *provider.Registry, logger *slog.Logger) (*HiveState, error) {
	hs := &HiveState{
		cfg:     cfg,
		counter: NewTokenCounter(),
		logger:  logger,
		cache:   newStateCache(128, 10*time.Minute),
		logs:    newMemoryLogStore(128, 30*time.Minute),
		ccr:     newCCRStore(512, 32, 30*time.Minute),
	}

	if cfg.Model != "" {
		ext, err := NewStateExtractor(registry, cfg.Model, cfg.MaxLatency(), cfg.HiveRoute.Levels)
		if err != nil {
			logger.Warn("hivestate: state extraction disabled", "error", err)
		} else {
			hs.extractor = ext
		}
	}

	return hs, nil
}

// Process runs the HiveState engine on a set of messages.
// Returns a Result with either rewritten messages (STATE mode) or original messages (NO_OP).
//
// scope isolates any CCR content this request stores or retrieves. An
// under-specified scope simply disables CCR for the request; compression
// itself is unaffected.
func (h *HiveState) Process(ctx context.Context, messages []Message, scope Scope, levelOverrides ...[]config.HiveRouteLevel) *Result {
	var levels []config.HiveRouteLevel
	if len(levelOverrides) > 0 {
		levels = levelOverrides[0]
	}

	// Dynamic profiling: detect conversation type and adapt parameters
	profile := DetectProfile(messages)
	params := DefaultProfileParams(profile)

	// Use config step_window as baseline, but profile can override
	stepWindow := h.cfg.StepWindow
	if params.StepWindow > 0 && params.StepWindow < stepWindow {
		stepWindow = params.StepWindow
	}

	profileNames := map[ConversationProfile]string{
		ProfileChat: "chat", ProfileToolAgent: "tool-agent", ProfileCodeAgent: "code-agent",
	}
	h.logger.Debug("hivestate: profile detected",
		"profile", profileNames[profile],
		"step_window", stepWindow,
		"preprocess_max", params.PreprocessMax,
	)

	zones := SplitMessages(messages, stepWindow)

	// Token budget cap: if configured, dynamically reduce step_window until Recent fits budget
	if h.cfg.TokenBudget > 0 && len(zones.History) > 0 {
		recentTokens := h.counter.CountMessages(zones.Recent)
		for recentTokens > h.cfg.TokenBudget && stepWindow > 2 {
			stepWindow--
			zones = SplitMessages(messages, stepWindow)
			recentTokens = h.counter.CountMessages(zones.Recent)
		}
		if stepWindow != h.cfg.StepWindow {
			h.logger.Info("hivestate: token budget cap applied",
				"original_step_window", h.cfg.StepWindow,
				"reduced_step_window", stepWindow,
				"recent_tokens", recentTokens,
				"budget", h.cfg.TokenBudget,
			)
		}
	}

	// Importance scoring: move critical messages to END of History so the extractor
	// sees them last and captures them more reliably in the state JSON.
	// Unlike before, promoted messages do NOT go into the output — only into extraction input.
	//
	// The append-only path cannot use this. The reordering depends on scores
	// computed over the whole history, so it reshuffles as the conversation
	// grows: a block frozen against one ordering would never match the next
	// turn's, and every log would be discarded one turn after it was built.
	// It is also pointless there, since a delta extraction only ever sees the
	// new messages.
	historyTokens := h.counter.CountMessages(zones.History)
	if !h.cfg.AppendOnlyState && len(zones.History) > 4 && historyTokens > 600 {
		scores := ScoreMessages(zones.History, h.counter)
		promotedBudget := params.PromotedBudget
		if h.cfg.TokenBudget > 0 {
			promotedBudget = h.cfg.TokenBudget / 5
		}
		maxPromoted := params.MaxPromoted
		if historyTokens < 1500 {
			maxPromoted = 2
			promotedBudget = 500
		}
		promoted := PickImportantMessages(zones.History, scores, maxPromoted, h.counter, promotedBudget)
		if len(promoted) > 0 {
			// Reorder History: put important messages at the end (extractor sees them last = recency bias)
			promSet := make(map[int]bool, len(promoted))
			for _, idx := range promoted {
				promSet[idx] = true
			}
			var regular, important []Message
			for i, m := range zones.History {
				if promSet[i] {
					important = append(important, m)
				} else {
					regular = append(regular, m)
				}
			}
			zones.History = append(regular, important...)
			h.logger.Debug("hivestate: reordered history for extraction emphasis",
				"important_count", len(important),
			)
		}
	}

	// No extractable history or no last user message → pass through
	if len(zones.History) == 0 || zones.Last.Content == "" {
		return &Result{
			Mode:           ModeNoOp,
			Messages:       messages,
			OriginalTokens: h.counter.CountMessages(messages),
			ResultTokens:   h.counter.CountMessages(messages),
			FallbackReason: "no_history",
		}
	}

	originalTokens := h.counter.CountMessages(messages)

	// Below threshold → pass through
	if originalTokens < h.cfg.Threshold {
		return &Result{
			Mode:           ModeNoOp,
			Messages:       messages,
			OriginalTokens: originalTokens,
			ResultTokens:   originalTokens,
			FallbackReason: fmt.Sprintf("below_threshold(%d/%d)", originalTokens, h.cfg.Threshold),
		}
	}

	// No extractor configured → pass through
	if h.extractor == nil {
		return &Result{
			Mode:           ModeNoOp,
			Messages:       messages,
			OriginalTokens: originalTokens,
			ResultTokens:   originalTokens,
			FallbackReason: "no_model",
		}
	}

	// Pre-check: if Recent + Last + minimum state overhead already exceeds 85% of original,
	// compression won't yield meaningful savings — skip to avoid wasting extraction call
	recentAndLastTokens := h.counter.CountMessages(zones.Recent) + h.counter.Count(zones.Last.Content)
	minStateOverhead := 100 // minimum tokens for state JSON + framing
	if recentAndLastTokens+minStateOverhead > (originalTokens * 85 / 100) {
		return &Result{
			Mode:           ModeNoOp,
			Messages:       messages,
			OriginalTokens: originalTokens,
			ResultTokens:   originalTokens,
			FallbackReason: "insufficient_headroom",
		}
	}

	// Check state cache — if History zone is identical to a previous call, reuse the state.
	//
	// The append-only path owns its own reuse: its log is keyed by scope, so it
	// keeps two sessions apart, while this cache is keyed by history content
	// alone and would hand one session's state to another.
	hKey := historyHash(zones.History)
	if cachedJSON, cachedState, ok := h.cache.get(hKey); ok && !h.cfg.AppendOnlyState {
		h.logger.Info("hivestate: cache hit, reusing state",
			"cache_size", h.cache.stats(),
			"original_tokens", originalTokens,
		)

		// System messages are forwarded verbatim. They sit at the very front
		// of the prompt, which is the region every provider prompt cache
		// covers, so editing them invalidates the cached prefix on every
		// request that touches them — the exact cost this package now exists
		// to avoid.
		rewritten := make([]Message, 0, len(zones.System)+3+len(zones.Recent))
		rewritten = append(rewritten, zones.System...)
		rewritten = append(rewritten, Message{
			Role:    "user",
			Content: "Conversation state (summary of older context):\n" + cachedJSON,
		})

		// Code Registry: deterministic extraction of code identifiers from History
		registry := h.buildRegistryMessage(zones.History)
		if registry.Content != "" {
			rewritten = append(rewritten, registry)
		}

		// CCR must run on this branch exactly as it does on the extraction
		// branch below. A session whose state keeps hitting the cache takes
		// only this path, so skipping the Store here meant the working set
		// never accumulated and retrieval always came back empty — the file
		// was compressed away and never recovered.
		h.ccr.Store(scope, zones.History, h.counter)

		var injectedCCR []Message
		var ccrBudgetSkipped bool
		ccrMsgs := h.ccr.GetWorkingSet(scope, ccrQueryTexts(zones), ccrRetrievalBudget, h.counter)
		if len(ccrMsgs) > 0 {
			ccrTokens := h.counter.CountMessages(ccrMsgs) + 15
			currentTok := h.counter.CountMessages(rewritten) + h.counter.CountMessages(zones.Recent) + h.counter.Count(zones.Last.Content)
			if currentTok+ccrTokens < ccrCeiling(originalTokens) {
				injectedCCR = append([]Message{{
					Role:    "user",
					Content: ccrFramingText,
				}}, ccrMsgs...)
				rewritten = append(rewritten, injectedCCR...)
				h.logger.Info("hivestate: CCR injected (cache-hit path)",
					"injected_messages", len(ccrMsgs),
					"injected_tokens", ccrTokens,
				)
			} else {
				ccrBudgetSkipped = true
				h.logger.Info("hivestate: CCR skipped (budget, cache-hit path)",
					"ccr_tokens", ccrTokens,
					"current_tokens", currentTok,
					"budget_limit", ccrCeiling(originalTokens),
				)
			}
		}

		rewritten = append(rewritten, zones.Recent...)
		rewritten = append(rewritten, zones.Last)

		resultTokens := h.counter.CountMessages(rewritten)
		if resultTokens >= originalTokens {
			return &Result{
				Mode:             ModeNoOp,
				Messages:         messages,
				OriginalTokens:   originalTokens,
				ResultTokens:     originalTokens,
				FallbackReason:   "no_savings",
				CCRBudgetSkipped: ccrBudgetSkipped,
			}
		}

		return &Result{
			Mode:             ModeState,
			Messages:         rewritten,
			CCRMessages:      injectedCCR,
			CCRBudgetSkipped: ccrBudgetSkipped,
			RegistryMessage:  registry,
			OriginalTokens:   originalTokens,
			ResultTokens:     resultTokens,
			State:            cachedState,
			StateJSON:        cachedJSON,
			CacheHit:         true,
		}
	}

	// Calculate extraction budget: state JSON should be at most 1/3 of history
	// to guarantee net savings after adding system + recent + last
	maxOutputTokens := historyTokens / 3
	if maxOutputTokens > 2048 {
		maxOutputTokens = 2048
	}
	if maxOutputTokens < 150 {
		maxOutputTokens = 150 // minimum viable state
	}

	// STATE mode: extract structured state from conversation history
	var state *ExtractionResult
	var err error

	if h.cfg.AppendOnlyState {
		state, err = h.extractIntoLog(ctx, scope, zones, maxOutputTokens, levels)
		if err != nil {
			// Deliberately not falling back to a full snapshot. A snapshot is
			// rewritten from scratch every turn, so it breaks the frozen prefix
			// this mode exists to keep and busts the very cache it is trying to
			// protect — while still paying for an extraction. Forwarding the
			// conversation untouched is the cheaper and safer failure.
			h.logger.Warn("hivestate: append-only extraction failed, passing through", "error", err)
			return &Result{
				Mode:           ModeNoOp,
				Messages:       messages,
				OriginalTokens: originalTokens,
				ResultTokens:   originalTokens,
				FallbackReason: "append_only_extraction_failed",
			}
		}
	}

	// Try incremental extraction first: if we have a cached prefix, only extract new messages
	if state == nil && !h.cfg.AppendOnlyState {
		if prevJSON, _, prevLen, found := h.cache.findPrefix(zones.History); found && prevLen > 0 {
			// Incremental: only extract messages beyond the cached prefix
			newMessages := zones.History[prevLen:]
			if len(newMessages) > 0 {
				// Prepend previous state as context for the extractor
				contextMsg := Message{
					Role:    "user",
					Content: "Previous conversation state:\n" + prevJSON + "\n\nNew messages to integrate:",
				}
				incrementalHistory := append([]Message{contextMsg}, newMessages...)
				state, err = h.extractor.ExtractWithBudget(ctx, incrementalHistory, zones.Last, maxOutputTokens, levels)
				if err == nil {
					h.logger.Info("hivestate: incremental extraction",
						"prev_messages", prevLen,
						"new_messages", len(newMessages),
						"total_history", len(zones.History),
					)
				}
			}
		}
	}

	// Fallback: full extraction if incremental failed or wasn't possible
	if state == nil {
		// Pre-process History to reduce token cost for the extractor
		var processedHistory []Message
		if params.PreprocessMax > 0 {
			processedHistory = PreProcessHistory(zones.History, params.PreprocessMax)
		} else {
			// Even without full preprocessing, truncate long tool responses
			// to prevent nested JSON escaping explosion in extraction output
			processedHistory = TruncateToolResponses(zones.History, 300)
		}
		state, err = h.extractor.ExtractWithBudget(ctx, processedHistory, zones.Last, maxOutputTokens, levels)
	}

	if err != nil {
		h.logger.Warn("hivestate: extraction failed, passing through", "error", err)
		return &Result{
			Mode:           ModeNoOp,
			Messages:       messages,
			OriginalTokens: originalTokens,
			ResultTokens:   originalTokens,
			FallbackReason: "extraction_failed",
		}
	}

	h.logger.Info("hivestate: state extracted",
		"intent", state.State.Intent,
		"domain", state.State.Domain,
		"status", state.State.ConversationStatus,
		"original_tokens", originalTokens,
		"state_json", state.JSON,
	)

	// Cache the extracted state for future reuse
	h.cache.put(hKey, state.JSON, state.State, len(zones.History))

	// CCR: store original History for proactive retrieval
	ccrID := h.ccr.Store(scope, zones.History, h.counter)
	h.logger.Info("hivestate: CCR stored",
		"ccr_id", ccrID,
		"stored_messages", len(zones.History),
		"stored_tokens", h.counter.CountMessages(zones.History),
	)

	// Build rewritten messages: [system verbatim] + [state as user context] + [CCR injection if relevant] + [recent messages] + [last user]
	// System messages are forwarded verbatim; see the cache-hit branch above.
	rewritten := make([]Message, 0, len(zones.System)+3+len(zones.Recent))
	rewritten = append(rewritten, zones.System...)
	rewritten = append(rewritten, Message{
		Role:    "user",
		Content: "Conversation state (summary of older context):\n" + state.JSON,
	})

	// Code Registry: deterministic extraction of code identifiers from History
	registry := h.buildRegistryMessage(zones.History)
	if registry.Content != "" {
		rewritten = append(rewritten, registry)
	}

	// CCR working set injection: the file contents the agent is actively
	// working with, recovered from the compressed history.
	//
	// The messages are also carried on the Result so the HTTP rewriters can
	// splice them into the real request body at the tail. What is appended to
	// `rewritten` here only feeds the token accounting below.
	ccrMsgs := h.ccr.GetWorkingSet(scope, ccrQueryTexts(zones), ccrRetrievalBudget, h.counter)
	currentTokens := h.counter.CountMessages(rewritten) + h.counter.CountMessages(zones.Recent) + h.counter.Count(zones.Last.Content)
	var injectedCCR []Message
	var ccrBudgetSkipped bool
	if len(ccrMsgs) > 0 {
		ccrTokens := h.counter.CountMessages(ccrMsgs)
		if currentTokens+ccrTokens < ccrCeiling(originalTokens) {
			// The "check context first" instruction rides with the framing
			// message rather than being appended to the system prompt.
			// Editing the system prompt is a head modification: it sits inside
			// the region a provider prompt cache covers, so a note that
			// changes with the working set would bust the cached prefix on
			// every turn. The tail costs nothing.
			injectedCCR = append([]Message{{
				Role:    "user",
				Content: ccrFramingText,
			}}, ccrMsgs...)
			rewritten = append(rewritten, injectedCCR...)
			h.logger.Info("hivestate: CCR injected",
				"injected_messages", len(ccrMsgs),
				"injected_tokens", ccrTokens,
				"current_tokens", currentTokens,
				"original_tokens", originalTokens,
			)
		} else {
			ccrBudgetSkipped = true
			h.logger.Info("hivestate: CCR skipped (budget)",
				"ccr_tokens", ccrTokens,
				"current_tokens", currentTokens,
				"budget_limit", ccrCeiling(originalTokens),
			)
		}
	}

	rewritten = append(rewritten, zones.Recent...)
	rewritten = append(rewritten, zones.Last)

	resultTokens := h.counter.CountMessages(rewritten)

	// Guard: if compression result is larger than original, pass through
	if resultTokens >= originalTokens {
		h.logger.Warn("hivestate: no_savings",
			"original_tokens", originalTokens,
			"result_tokens", resultTokens,
			"state_json_chars", len(state.JSON),
			"state_json_tokens", h.counter.Count(state.JSON),
			"recent_tokens", h.counter.CountMessages(zones.Recent),
			"last_tokens", h.counter.Count(zones.Last.Content),
			"history_tokens", historyTokens,
			"max_output_budget", maxOutputTokens,
			"num_rewritten_msgs", len(rewritten),
		)
		return &Result{
			Mode:             ModeNoOp,
			Messages:         messages,
			OriginalTokens:   originalTokens,
			ResultTokens:     originalTokens,
			FallbackReason:   "no_savings",
			PromptTokens:     state.PromptTokens,
			CompletionTokens: state.CompletionTokens,
			CCRBudgetSkipped: ccrBudgetSkipped,
		}
	}

	return &Result{
		Mode:             ModeState,
		Messages:         rewritten,
		CCRMessages:      injectedCCR,
		CCRBudgetSkipped: ccrBudgetSkipped,
		RegistryMessage:  registry,
		OriginalTokens:   originalTokens,
		ResultTokens:     resultTokens,
		State:            state.State,
		StateJSON:        state.JSON,
		StateParts:       state.Parts,
		PromptTokens:     state.PromptTokens,
		CompletionTokens: state.CompletionTokens,
	}
}

// buildRegistryMessage creates the Code Registry message from History zone.
func (h *HiveState) buildRegistryMessage(history []Message) Message {
	registry := ExtractCodeRegistry(history)
	registry.TruncateToTokenBudget(h.counter)
	msg := registry.FormatRegistryMessage()
	if msg == "" {
		return Message{}
	}
	h.logger.Info("hivestate: code registry injected",
		"content", msg,
	)
	return Message{Role: "user", Content: msg}
}

// ccrRetrievalBudget caps how many tokens of recovered files may be
// re-injected in one request.
const ccrRetrievalBudget = 8000

// ccrCeilingPercent is the share of the original prompt the rewritten one must
// stay under for recovered context to be injected at all. It also bounds what
// the prefix-cache guard has to assume a rewrite will cost, so both sides read
// it from here rather than repeating the literal.
const ccrCeilingPercent = 80

// ccrCeiling is the token count a rewrite must stay below to earn a CCR
// injection.
func ccrCeiling(originalTokens int) int { return originalTokens * ccrCeilingPercent / 100 }

// ccrQueryTexts builds the text CCR matches file mentions against: the live
// turn plus the preserved recent window. Both branches use it, so a cache hit
// and a fresh extraction retrieve the same working set.
func ccrQueryTexts(zones MessageZones) []string {
	texts := make([]string, 0, 1+len(zones.Recent))
	texts = append(texts, zones.Last.Content)
	for _, m := range zones.Recent {
		texts = append(texts, m.Content)
	}
	return texts
}

// ccrFramingText labels re-injected content so the model can tell recovered
// history from the live turn, and tells it not to re-read those files.
const ccrFramingText = "[Relevant context retrieved from compressed history]\n" +
	"Before reading a file, check whether it already appears below as a " +
	"[Working file: ...] message. If so, use it directly without re-reading."
