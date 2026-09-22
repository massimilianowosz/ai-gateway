package hivestate

import (
	"bytes"
	"encoding/json"
)

// Provider prompt caches are *positional*: they match a byte prefix of the
// serialized prompt, not a set of messages. Any rewrite at position N
// invalidates the cache from N onward, so preserving later messages verbatim
// does NOT preserve their cache validity — they simply fall out of the cached
// prefix and get re-billed at full input price.
//
// HiveState rewrites the head of the conversation: it replaces older history
// with a state summary inserted right after the system messages. When the
// client is using provider prompt caching, that rewrite converts every
// surviving token from a discounted cache read into full-price input. Cutting
// token count can therefore *raise* the bill.
//
// The break-even is:
//
//	cost_passthrough = ratio*frozenTokens + (originalTokens - frozenTokens)
//	cost_rewritten   = predictedTokens          // nothing is cached any more
//
// where ratio is the provider's cache-read price multiplier. On Anthropic
// (ratio 0.1) HiveState must shrink the prompt below ~10% of the original just
// to break even; a preserved recent window alone usually exceeds that budget.
// On OpenAI (ratio 0.5) the bar is ~50% and HiveState often still wins.
//
// This file computes that comparison before HiveState runs, so the engine is
// gated on the actual cache economics instead of a raw token threshold.

// APIFlavor identifies the inbound API surface a request arrived on.
type APIFlavor int

const (
	// APIOpenAI is the /v1/chat/completions surface.
	APIOpenAI APIFlavor = iota
	// APIAnthropic is the /v1/messages surface.
	APIAnthropic
	// APIResponses is the /v1/responses surface. It is OpenAI-hosted, so it
	// inherits OpenAI's automatic prefix caching and cache-read discount.
	APIResponses
)

// Default cache-read price multipliers, relative to normal input tokens.
//
// Only used when the model is absent from the price catalogue, which is the
// preferred source. Both default to 0.1: Anthropic bills cached reads at 0.1x,
// and so does OpenAI's gpt-5 family — 0.5x is the older 4o/4.1 tariff.
// Overstating the ratio makes passthrough look dearer than it is and lets the
// guard wave through rewrites that do not pay, so the low value is the safe
// assumption rather than the generous one.
const (
	DefaultAnthropicCacheReadRatio = 0.1
	DefaultOpenAICacheReadRatio    = 0.1

	// DefaultMinCacheableTokens is OpenAI's documented minimum prompt length
	// for automatic prefix caching. Below it there is no implicit cache to
	// preserve.
	DefaultMinCacheableTokens = 1024

	// DefaultStateSummaryChars is the assumed serialized size of the state
	// summary HiveState would inject. Used to probe the rewrite before paying
	// for a real extraction.
	DefaultStateSummaryChars = 1200

	// DefaultGuardMargin is the fractional advantage a rewrite must show over
	// passthrough before it is allowed to bust the cache. A marginal win is
	// not worth the variance.
	DefaultGuardMargin = 0.10

	// maxGuardMargin is the ceiling EvaluatePrefixCacheGuard clamps a
	// configured margin to. At 1.0 the comparison threshold reaches zero and
	// the guard would skip every request no matter what a rewrite would save.
	maxGuardMargin = 0.95
)

// cacheControlMarker is the Anthropic breakpoint key. Detection is a byte
// search rather than a full content-block parse: Anthropic content blocks vary
// widely in shape (string, array, nested tool_result), and a false positive
// (the literal text appearing inside user content) makes the guard skip
// HiveState — the fail-safe direction. A false negative would silently bust a
// real cache, so the conservative bias is deliberate.
var cacheControlMarker = []byte(`"cache_control"`)

// PrefixState describes what the gateway can infer about the provider-side
// prompt cache for one inbound request.
type PrefixState struct {
	// FrozenTokens estimates how many prompt tokens the provider would serve
	// from its cache if the request were forwarded unmodified.
	FrozenTokens int
	// Explicit is true when the client sent real cache markers, false when the
	// gateway inferred an implicit (automatic) cache.
	Explicit bool
	// Reason records how FrozenTokens was derived, for observability.
	Reason string
}

// DetectPrefixState estimates the provider-cached prefix for a request body.
//
// It never mutates the body and never fails: an unparseable body yields a
// zero-value PrefixState, which lets HiveState run exactly as it does today.
//
// observedCachedTokens is the real cached_prompt_tokens the provider reported
// on the previous request in this same conversation, or 0 when none is known
// yet. It caps the implicit detectors' guess: they can only assume the best
// case (everything before the newest turn is warm), which stays wrong forever
// once the real cache stops growing, since nothing in the request itself ever
// contradicts the assumption. Explicit Anthropic cache_control markers are
// evidence, not a guess, and are never capped by it.
func DetectPrefixState(body []byte, api APIFlavor, originalTokens, minCacheable int, assumeImplicit bool, observedCachedTokens int) PrefixState {
	if api == APIAnthropic {
		// cache_control markers are evidence, not inference: always honoured.
		return detectAnthropicPrefix(body, originalTokens)
	}
	if !assumeImplicit {
		// OpenAI and Responses expose no markers, so a warm cache can only be
		// guessed at. Guessing wrong disables HiveState on traffic that had
		// nothing to protect, so the guess is opt-in.
		return PrefixState{Reason: "implicit_cache_detection_off"}
	}
	if api == APIResponses {
		return capToObserved(detectResponsesPrefix(body, originalTokens, minCacheable), observedCachedTokens)
	}
	return capToObserved(detectOpenAIPrefix(body, originalTokens, minCacheable), observedCachedTokens)
}

// capToObserved bounds an implicit-cache guess by what the provider actually
// reported last turn. It only ever lowers the guess: a stale or missing
// observation must not invent a ceiling the request itself gives no reason
// to believe.
func capToObserved(ps PrefixState, observedCachedTokens int) PrefixState {
	if observedCachedTokens > 0 && observedCachedTokens < ps.FrozenTokens {
		ps.FrozenTokens = observedCachedTokens
		ps.Reason += "_capped_by_observed_cache"
	}
	return ps
}

// detectResponsesPrefix infers the implicit cache for /v1/responses.
//
// Responses carries its history in `input` rather than `messages`, and a
// request that chains on previous_response_id sends almost no history at all —
// the provider holds it server-side. In that case there is no client-visible
// prefix for HiveState to rewrite, and nothing for the guard to protect.
func detectResponsesPrefix(body []byte, originalTokens, minCacheable int) PrefixState {
	var b struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(body, &b); err == nil && b.PreviousResponseID != "" {
		return PrefixState{Explicit: true, Reason: "responses_server_side_history"}
	}
	if minCacheable <= 0 {
		minCacheable = DefaultMinCacheableTokens
	}
	if originalTokens < minCacheable {
		return PrefixState{Reason: "below_min_cacheable"}
	}
	// Inlined history: the same automatic prefix cache as Chat Completions
	// applies, and the parsed message view is what the rewriter works on.
	msgs := parseResponsesMessages(body)
	if countUserTurns(msgs) < 2 {
		return PrefixState{Reason: "first_turn"}
	}
	var frozen, total int
	lastUser := lastUserIndex(msgs)
	for i, m := range msgs {
		n := len(m.Content)
		total += n
		if i < lastUser {
			frozen += n
		}
	}
	if total == 0 {
		return PrefixState{Reason: "empty_messages"}
	}
	return PrefixState{
		FrozenTokens: originalTokens * frozen / total,
		Reason:       "responses_implicit_prefix",
	}
}

// countUserTurns counts real user turns, ignoring tool results.
func countUserTurns(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// lastUserIndex returns the index of the newest user turn, or -1.
func lastUserIndex(msgs []Message) int {
	idx := -1
	for i, m := range msgs {
		if m.Role == "user" {
			idx = i
		}
	}
	return idx
}

// detectAnthropicPrefix walks explicit cache_control breakpoints.
//
// The cached prefix extends through the last breakpoint. A breakpoint in
// system or tools freezes only the preamble, which HiveState never rewrites —
// so that case reports the preamble share and lets HiveState proceed. A
// breakpoint inside messages freezes conversation history, which is exactly
// what HiveState would rewrite.
func detectAnthropicPrefix(body []byte, originalTokens int) PrefixState {
	var b struct {
		System   json.RawMessage   `json:"system"`
		Tools    json.RawMessage   `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return PrefixState{Reason: "unparseable_body"}
	}

	// Highest message index carrying a breakpoint; -1 when none do.
	lastMarked := -1
	for i, raw := range b.Messages {
		if bytes.Contains(raw, cacheControlMarker) {
			lastMarked = i
		}
	}

	preambleMarked := bytes.Contains(b.System, cacheControlMarker) ||
		bytes.Contains(b.Tools, cacheControlMarker)

	if lastMarked < 0 {
		if preambleMarked {
			// Only the preamble is cached. HiveState leaves system and tools
			// untouched, so the cached region survives the rewrite.
			return PrefixState{Explicit: true, Reason: "anthropic_preamble_only"}
		}
		return PrefixState{Reason: "anthropic_no_markers"}
	}

	// Estimate the frozen share by byte weight. Byte share tracks token share
	// closely enough for a break-even comparison and avoids a second tokenizer
	// pass over the whole body.
	//
	// The ratio has to span exactly what originalTokens counts, which is the
	// system prompt plus the messages — parseAnthropicMessages prepends system
	// as a message. So system bytes go in the denominator, and in the numerator
	// too: a breakpoint on any message caches everything ahead of it, preamble
	// included. Counting system in originalTokens but not in the ratio inflated
	// the passthrough cost of a marked head while understating the live cache,
	// which is the direction that approves a rewrite it should have refused.
	// Tools are in neither term, for the same reason: originalTokens omits them.
	systemBytes := len(b.System)
	frozenBytes, totalBytes := systemBytes, systemBytes
	for i, raw := range b.Messages {
		totalBytes += len(raw)
		if i <= lastMarked {
			frozenBytes += len(raw)
		}
	}
	if totalBytes == 0 {
		return PrefixState{Explicit: true, Reason: "anthropic_empty_messages"}
	}

	return PrefixState{
		FrozenTokens: originalTokens * frozenBytes / totalBytes,
		Explicit:     true,
		Reason:       "anthropic_cache_control",
	}
}

// detectOpenAIPrefix infers the implicit automatic cache.
//
// OpenAI exposes no per-request markers, so a continuing conversation is
// assumed to have cached everything up to the newest user turn — that is what
// the previous request in the same session established.
func detectOpenAIPrefix(body []byte, originalTokens int, minCacheable int) PrefixState {
	if minCacheable <= 0 {
		minCacheable = DefaultMinCacheableTokens
	}
	if originalTokens < minCacheable {
		return PrefixState{Reason: "below_min_cacheable"}
	}

	var b struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return PrefixState{Reason: "unparseable_body"}
	}

	// The newest user turn is the only part the previous request could not
	// have cached. Everything before it is assumed warm.
	lastTurn := -1
	userTurns := 0
	for i, raw := range b.Messages {
		if isOpenAIUserTurn(raw) {
			lastTurn = i
			userTurns++
		}
	}
	if userTurns < 2 {
		// Only one user turn means this is the opening request of the
		// conversation: no earlier request existed to establish a prefix, so
		// nothing — not even the system message — is warm yet.
		return PrefixState{Reason: "first_turn"}
	}

	var frozenBytes, totalBytes int
	for i, raw := range b.Messages {
		totalBytes += len(raw)
		if i < lastTurn {
			frozenBytes += len(raw)
		}
	}
	if totalBytes == 0 {
		return PrefixState{Reason: "empty_messages"}
	}

	return PrefixState{
		FrozenTokens: originalTokens * frozenBytes / totalBytes,
		Explicit:     false,
		Reason:       "openai_implicit_prefix",
	}
}

// GuardDecision is the outcome of the prefix-cache economics check.
type GuardDecision struct {
	// Skip is true when forwarding unmodified is cheaper than rewriting.
	Skip bool
	// Reason is a stable machine-readable cause, surfaced in headers and metrics.
	Reason string
	// FrozenTokens is what the guard believed was cached.
	FrozenTokens int
	// PassthroughCost and RewriteCost are in normalized input-token units:
	// one unit is one full-price input token.
	PassthroughCost float64
	RewriteCost     float64
	// BreakEvenTurns is how many turns the rewrite needs to pay for itself:
	// the turn count at which its one-time transition cost is recovered by
	// its lower per-turn cost thereafter. It is what Horizon is a guess at —
	// exposing it turns "amortize_over_turns: 10" from an unverifiable
	// assumption into a number a decision can be checked against. 0 when the
	// rewrite never pays off (its steady-state cost is not even lower), in
	// which case no horizon, however long, would change the answer.
	BreakEvenTurns float64
}

// GuardInput describes one rewrite decision.
type GuardInput struct {
	Prefix          PrefixState
	OriginalTokens  int
	PredictedTokens int
	// ReusableAfterRewrite is how much of the rewritten prompt the provider
	// will still have cached on the next turn. It is zero unless the rewrite
	// emits a cacheable, append-only state region.
	ReusableAfterRewrite int
	CacheReadRatio       float64
	// CacheWriteRatio prices storing a new prefix. Anthropic charges 1.25x;
	// providers with an implicit cache charge nothing extra.
	CacheWriteRatio float64
	Margin          float64
	// Horizon is how many turns the decision is amortised over. A rewrite
	// always loses on the turn it happens — it discards a warm cache to build
	// a new one — so a horizon of 1 can never choose it, no matter how much
	// smaller the rewritten prompt is.
	Horizon int
}

// EvaluatePrefixCacheGuard compares the cost of forwarding unmodified against
// the cost of letting HiveState rewrite the head.
//
// Both costs are expressed in full-price input-token units. Passthrough bills
// the frozen prefix at the provider's cache-read ratio; a rewrite destroys the
// prefix, so every surviving token is billed at full price.
func EvaluatePrefixCacheGuard(ps PrefixState, originalTokens, predictedTokens int, ratio, margin float64) GuardDecision {
	return EvaluateGuard(GuardInput{
		Prefix:          ps,
		OriginalTokens:  originalTokens,
		PredictedTokens: predictedTokens,
		CacheReadRatio:  ratio,
		Margin:          margin,
		Horizon:         1,
	})
}

// EvaluateGuard weighs a rewrite over a horizon of turns.
//
// The single-turn comparison is not the whole story once the rewritten prompt
// is itself cacheable: the rewrite pays once to install a smaller prefix and is
// then cheaper every turn after. Judged one turn at a time that trade can never
// be taken, which is why a rewrite that shrinks a prompt to a fifth was still
// being refused.
func EvaluateGuard(in GuardInput) GuardDecision {
	ps, originalTokens, ratio := in.Prefix, in.OriginalTokens, in.CacheReadRatio
	d := GuardDecision{FrozenTokens: ps.FrozenTokens}

	if ps.FrozenTokens <= 0 {
		d.Reason = "no_cached_prefix"
		return d
	}
	if originalTokens <= 0 {
		d.Reason = "unknown_token_count"
		return d
	}
	if ratio >= 1.0 {
		// Provider gives no cache discount: there is nothing to protect.
		d.Reason = "no_cache_discount"
		return d
	}

	frozen := float64(ps.FrozenTokens)
	if frozen > float64(originalTokens) {
		frozen = float64(originalTokens)
	}

	predicted := float64(in.PredictedTokens)
	reusable := float64(in.ReusableAfterRewrite)
	if reusable > predicted {
		reusable = predicted
	}
	if reusable < 0 {
		reusable = 0
	}
	writeRatio := in.CacheWriteRatio
	if writeRatio < 1.0 {
		writeRatio = 1.0
	}
	horizon := in.Horizon
	if horizon < 1 {
		horizon = 1
	}
	margin := in.Margin

	passthroughTurn := ratio*frozen + (float64(originalTokens) - frozen)
	// The turn the switch happens: the old prefix is gone and the new one has
	// to be written before it can ever be read back.
	transition := (predicted - reusable) + writeRatio*reusable
	steady := ratio*reusable + (predicted - reusable)

	d.PassthroughCost = passthroughTurn
	d.RewriteCost = steady
	if horizon == 1 {
		d.RewriteCost = transition
	}

	passthroughTotal := passthroughTurn * float64(horizon)
	rewriteTotal := transition + steady*float64(horizon-1)
	d.BreakEvenTurns = breakEvenTurns(passthroughTurn, transition, steady)

	// margin shrinks the passthrough side so a rewrite has to win clearly
	// before it is allowed to bust a cache. Clamped at both ends: at 1.0 or
	// above the threshold collapses to zero or goes negative, and every
	// request would skip unconditionally — a misconfiguration that looks like
	// the guard working rather than like a broken setting.
	if margin < 0 {
		margin = 0
	} else if margin > maxGuardMargin {
		margin = maxGuardMargin
	}
	if rewriteTotal >= passthroughTotal*(1.0-margin) {
		d.Skip = true
		d.Reason = "prefix_cache_cheaper"
		return d
	}

	d.Reason = "rewrite_cheaper"
	return d
}

// breakEvenTurns solves for the turn count R at which a rewrite's one-time
// transition cost is recovered by its lower steady-state cost:
//
//	transition + steady*(R-1) = passthrough*R
//	R = (transition - steady) / (passthrough - steady)
//
// 0 when passthrough <= steady: the rewrite is not even cheaper per turn once
// installed, so it never pays off regardless of how many turns are left.
func breakEvenTurns(passthroughTurn, transition, steady float64) float64 {
	if passthroughTurn <= steady {
		return 0
	}
	r := (transition - steady) / (passthroughTurn - steady)
	if r < 0 {
		return 0
	}
	return r
}

// CacheWriteRatioFor returns what storing a new prefix costs relative to a
// full-price input token. Anthropic bills cache writes at 1.25x; providers
// with an implicit cache fold the cost into the normal price.
func CacheWriteRatioFor(api APIFlavor) float64 {
	if api == APIAnthropic {
		return 1.25
	}
	return 1.0
}

// CacheReadRatioFor returns the cache-read price multiplier to assume for an
// API surface, honouring explicit configuration when present.
func CacheReadRatioFor(api APIFlavor, anthropicRatio, openaiRatio float64) float64 {
	// Responses is OpenAI-hosted and shares its cache-read discount.
	if api == APIAnthropic {
		if anthropicRatio > 0 {
			return anthropicRatio
		}
		return DefaultAnthropicCacheReadRatio
	}
	if openaiRatio > 0 {
		return openaiRatio
	}
	return DefaultOpenAICacheReadRatio
}

// PredictRewrittenTokens estimates the prompt size after a HiveState rewrite
// without paying for a state extraction.
//
// It runs the *real* rewriter against a synthetic state summary of the
// expected size, so the guard can never drift from what the rewrite actually
// does. The result is scaled from the byte delta rather than re-tokenized:
// the comparison only needs a ratio, and both sides are measured the same way.
//
// The second return value is false when the rewriter would leave the body
// unchanged — there is no rewrite to gate, and no cache to bust.
func PredictRewrittenTokens(body []byte, api APIFlavor, window, originalTokens, stateChars int) (int, bool) {
	predicted, _, ok := PredictRewrite(body, api, window, originalTokens, stateChars)
	return predicted, ok
}

// PredictRewrite is PredictRewrittenTokens plus how much of the result stays
// cached between turns.
func PredictRewrite(body []byte, api APIFlavor, window, originalTokens, stateChars int) (int, int, bool) {
	if originalTokens <= 0 || len(body) == 0 {
		return originalTokens, 0, false
	}
	if stateChars <= 0 {
		stateChars = DefaultStateSummaryChars
	}

	probe := &Result{StateJSON: syntheticStateJSON(stateChars)}

	var rewritten []byte
	var err error
	switch api {
	case APIAnthropic:
		rewritten, err = rewriteAnthropicBodyWithWindow(body, probe, window)
	case APIResponses:
		rewritten, err = rewriteResponsesBodyWithWindow(body, probe, window)
	default:
		rewritten, err = rewriteOpenAIBodyWithWindow(body, probe, window)
	}
	if err != nil || len(rewritten) == 0 {
		// Treat an unpredictable rewrite as "no shrink": the guard then
		// compares full size against passthrough and errs toward preserving
		// the cache.
		return originalTokens, 0, false
	}
	if len(rewritten) >= len(body) {
		return originalTokens, 0, false
	}

	predicted := withCCRAllowance(originalTokens*len(rewritten)/len(body), originalTokens)
	return predicted, PredictReusableTokens(rewritten, api, predicted), true
}

// PredictReusableTokens estimates how much of a rewritten prompt the provider
// will still have cached on the following turn.
//
// That is the region built to be stable: the untouched system/preamble plus
// the append-only state. The recent raw tail is excluded — it turns over every
// turn by design. Without this the guard prices a rewrite as permanently
// uncached, which is what kept refusing rewrites that shrink a prompt to a
// fifth of its size.
func PredictReusableTokens(rewritten []byte, api APIFlavor, predictedTokens int) int {
	if predictedTokens <= 0 || len(rewritten) == 0 {
		return 0
	}

	if api == APIAnthropic {
		var parsed struct {
			System   json.RawMessage   `json:"system"`
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(rewritten, &parsed); err != nil || len(parsed.Messages) == 0 {
			return 0
		}
		stable := len(parsed.System)
		// The state message leads the rewritten conversation.
		stable += len(parsed.Messages[0])
		return scaleTokens(stable, len(rewritten), predictedTokens)
	}

	if api != APIResponses {
		return 0
	}
	var parsed struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rewritten, &parsed); err != nil || len(parsed.Input) == 0 {
		return 0
	}
	// The preamble, then the run of message items that carries the state. The
	// first tool call marks where the volatile tail begins.
	stable := 0
	for _, raw := range parsed.Input {
		it := parseResponsesItem(raw)
		isPreamble := it.Type == "additional_tools" ||
			(it.Type == "message" && (it.Role == "developer" || it.Role == "system"))
		isState := it.Type == "message" && it.Role == "user"
		if !isPreamble && !isState {
			break
		}
		stable += len(raw)
	}
	return scaleTokens(stable, len(rewritten), predictedTokens)
}

func scaleTokens(part, whole, tokens int) int {
	if whole <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return tokens
	}
	return tokens * part / whole
}

// withCCRAllowance raises a rewrite prediction to cover recovered context.
//
// The probe runs the rewriter with an empty Result, so it never sees the CCR
// messages the real rewrite may append — up to ccrRetrievalBudget tokens of
// recovered files. Left uncorrected, the guard compared passthrough against a
// body smaller than the one actually sent, and could bust a live cache for a
// rewrite that barely shrank.
//
// The raw budget is not the bound, though: the rewriter only injects CCR while
// the result stays under ccrCeiling, so that ceiling is the worst case. A
// prediction already at or above it earns no injection and stands as it is.
func withCCRAllowance(predicted, originalTokens int) int {
	ceiling := ccrCeiling(originalTokens)
	if predicted >= ceiling {
		return predicted
	}
	if withCCR := predicted + ccrRetrievalBudget; withCCR < ceiling {
		return withCCR
	}
	return ceiling
}

// syntheticStateJSON builds a placeholder state summary of roughly n bytes.
// Only its length matters — it is never forwarded upstream.
func syntheticStateJSON(n int) string {
	const head = `{"intent":"`
	const tail = `"}`
	if n <= len(head)+len(tail) {
		return head + tail
	}
	filler := make([]byte, n-len(head)-len(tail))
	for i := range filler {
		filler[i] = 'x'
	}
	return head + string(filler) + tail
}
