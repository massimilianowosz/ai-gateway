package hivestate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// buildAnthropicBody builds a /v1/messages body with `turns` user/assistant
// pairs. markLast places a cache_control breakpoint on the final user message,
// which is how incremental Anthropic caching walks the prefix forward.
func buildAnthropicBody(t *testing.T, turns int, markLast bool) []byte {
	t.Helper()
	var msgs []map[string]any
	filler := strings.Repeat("context ", 200)
	for i := 0; i < turns; i++ {
		user := map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("turn %d %s", i, filler)},
			},
		}
		if markLast && i == turns-1 {
			blocks := user["content"].([]map[string]any)
			blocks[0]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		msgs = append(msgs, user)
		msgs = append(msgs, map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": "reply " + filler}},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4",
		"system":   "You are helpful.",
		"messages": msgs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func buildOpenAIBody(t *testing.T, turns int) []byte {
	t.Helper()
	filler := strings.Repeat("context ", 200)
	msgs := []map[string]any{{"role": "system", "content": "You are helpful."}}
	for i := 0; i < turns; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("turn %d %s", i, filler)})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "reply " + filler})
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-4o", "messages": msgs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func TestDetectAnthropicPrefix_NoMarkers(t *testing.T) {
	body := buildAnthropicBody(t, 10, false)
	ps := DetectPrefixState(body, APIAnthropic, 50000, DefaultMinCacheableTokens, true, 0)
	if ps.FrozenTokens != 0 {
		t.Fatalf("expected no frozen prefix without markers, got %d", ps.FrozenTokens)
	}
	if ps.Explicit {
		t.Fatal("expected Explicit=false without markers")
	}
}

func TestDetectAnthropicPrefix_BreakpointOnLastTurn(t *testing.T) {
	body := buildAnthropicBody(t, 10, true)
	ps := DetectPrefixState(body, APIAnthropic, 50000, DefaultMinCacheableTokens, true, 0)
	if !ps.Explicit {
		t.Fatal("expected Explicit=true with cache_control present")
	}
	// The breakpoint sits on the last user message, so nearly the whole
	// conversation is inside the cached prefix.
	if ps.FrozenTokens < 40000 {
		t.Fatalf("expected most of the prompt frozen, got %d of 50000", ps.FrozenTokens)
	}
}

func TestDetectAnthropicPrefix_PreambleOnlyLeavesMessagesFree(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4",
		"system": []map[string]any{
			{"type": "text", "text": strings.Repeat("rules ", 500),
				"cache_control": map[string]string{"type": "ephemeral"}},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ps := DetectPrefixState(body, APIAnthropic, 5000, DefaultMinCacheableTokens, true, 0)
	if !ps.Explicit {
		t.Fatal("expected Explicit=true")
	}
	// HiveState never rewrites system/tools, so a preamble-only breakpoint
	// must not block it.
	if ps.FrozenTokens != 0 {
		t.Fatalf("preamble-only breakpoint must not freeze messages, got %d", ps.FrozenTokens)
	}
	if ps.Reason != "anthropic_preamble_only" {
		t.Fatalf("unexpected reason %q", ps.Reason)
	}
}

func TestDetectOpenAIPrefix_BelowMinCacheable(t *testing.T) {
	body := buildOpenAIBody(t, 2)
	ps := DetectPrefixState(body, APIOpenAI, 500, DefaultMinCacheableTokens, true, 0)
	if ps.FrozenTokens != 0 {
		t.Fatalf("short prompt must not assume a cache, got %d", ps.FrozenTokens)
	}
	if ps.Reason != "below_min_cacheable" {
		t.Fatalf("unexpected reason %q", ps.Reason)
	}
}

func TestDetectOpenAIPrefix_FirstTurnHasNothingCached(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": strings.Repeat("hello ", 3000)},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ps := DetectPrefixState(body, APIOpenAI, 5000, DefaultMinCacheableTokens, true, 0)
	if ps.FrozenTokens != 0 {
		t.Fatalf("first turn cannot be cached by a previous request, got %d", ps.FrozenTokens)
	}
}

func TestDetectOpenAIPrefix_ContinuingConversation(t *testing.T) {
	body := buildOpenAIBody(t, 8)
	ps := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, true, 0)
	if ps.Explicit {
		t.Fatal("OpenAI implicit cache must not be reported as explicit")
	}
	if ps.FrozenTokens < 15000 {
		t.Fatalf("expected most of a continuing conversation cached, got %d", ps.FrozenTokens)
	}
}

// A provider whose real cache already collapsed to a small floor must not be
// overridden by the positional guess, which has no way to see that eviction
// and would otherwise assume the same optimistic best case forever.
func TestDetectOpenAIPrefix_ObservedCacheCapsThePositionalGuess(t *testing.T) {
	body := buildOpenAIBody(t, 8)
	uncapped := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, true, 0)
	capped := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, true, 1000)

	if capped.FrozenTokens != 1000 {
		t.Fatalf("expected the guess capped to the real observation (1000), got %d", capped.FrozenTokens)
	}
	if capped.FrozenTokens >= uncapped.FrozenTokens {
		t.Fatalf("cap must be strictly lower than the uncapped guess (%d) to prove it applied, got %d",
			uncapped.FrozenTokens, capped.FrozenTokens)
	}
	if !strings.Contains(capped.Reason, "capped_by_observed_cache") {
		t.Fatalf("expected the reason to record that the cap applied, got %q", capped.Reason)
	}
}

// An observation cannot raise the guess above what the request itself
// justifies — it is a ceiling on the optimistic case, not a new floor.
func TestDetectOpenAIPrefix_ObservedCacheNeverRaisesTheGuess(t *testing.T) {
	body := buildOpenAIBody(t, 8)
	uncapped := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, true, 0)
	ps := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, true, 999999)

	if ps.FrozenTokens != uncapped.FrozenTokens {
		t.Fatalf("a generous observation must not change the guess, got %d want %d",
			ps.FrozenTokens, uncapped.FrozenTokens)
	}
}

// Anthropic's cache_control markers are the provider's own contract, not a
// positional guess, so a stale observation must never override them.
func TestDetectAnthropicPrefix_NeverCappedByObservedCache(t *testing.T) {
	body := buildAnthropicBody(t, 10, true)
	ps := DetectPrefixState(body, APIAnthropic, 50000, DefaultMinCacheableTokens, true, 1)
	if ps.FrozenTokens < 40000 {
		t.Fatalf("explicit markers must be honoured regardless of an unrelated observed cap, got %d", ps.FrozenTokens)
	}
}

// The economics are the point of the whole change: on Anthropic a rewrite that
// cuts tokens by 79% still loses to passthrough, because the survivors move
// from 0.1x to 1x.
func TestEvaluateGuard_AnthropicRewriteLosesDespiteBigTokenCut(t *testing.T) {
	ps := PrefixState{FrozenTokens: 200000, Explicit: true, Reason: "anthropic_cache_control"}
	d := EvaluatePrefixCacheGuard(ps, 200000, 42000, DefaultAnthropicCacheReadRatio, DefaultGuardMargin)

	if !d.Skip {
		t.Fatalf("expected skip: passthrough=%.0f rewrite=%.0f", d.PassthroughCost, d.RewriteCost)
	}
	if d.Reason != "prefix_cache_cheaper" {
		t.Fatalf("unexpected reason %q", d.Reason)
	}
	if d.PassthroughCost != 20000 {
		t.Fatalf("passthrough should be 0.1*200000 = 20000, got %.0f", d.PassthroughCost)
	}
}

func TestEvaluateGuard_AnthropicRewriteWinsBelowBreakEven(t *testing.T) {
	// Compressing 200k to 15k clears the ~10% bar even after the margin.
	ps := PrefixState{FrozenTokens: 200000, Explicit: true}
	d := EvaluatePrefixCacheGuard(ps, 200000, 15000, DefaultAnthropicCacheReadRatio, DefaultGuardMargin)
	if d.Skip {
		t.Fatalf("expected rewrite to win: passthrough=%.0f rewrite=%.0f", d.PassthroughCost, d.RewriteCost)
	}
	if d.Reason != "rewrite_cheaper" {
		t.Fatalf("unexpected reason %q", d.Reason)
	}
}

// The same token cut that loses under a deep cache discount wins under a
// shallow one. The ratios are written out rather than taken from the defaults:
// what matters is that the economics follow the tariff, and the defaults now
// agree because the gpt-5 family reads cache at the same 0.1x as Anthropic.
func TestEvaluateGuard_ShallowDiscountAllowsWhatDeepDiscountRejects(t *testing.T) {
	ps := PrefixState{FrozenTokens: 100000}
	const original, predicted = 100000, 40000
	const shallow, deep = 0.5, 0.1

	lenient := EvaluatePrefixCacheGuard(ps, original, predicted, shallow, DefaultGuardMargin)
	strict := EvaluatePrefixCacheGuard(ps, original, predicted, deep, DefaultGuardMargin)

	if lenient.Skip {
		t.Fatalf("a 0.5x discount should allow the rewrite: passthrough=%.0f rewrite=%.0f",
			lenient.PassthroughCost, lenient.RewriteCost)
	}
	if !strict.Skip {
		t.Fatalf("a 0.1x discount should reject the same rewrite: passthrough=%.0f rewrite=%.0f",
			strict.PassthroughCost, strict.RewriteCost)
	}
}

func TestEvaluateGuard_NoCachedPrefixNeverSkips(t *testing.T) {
	d := EvaluatePrefixCacheGuard(PrefixState{}, 100000, 10000, DefaultAnthropicCacheReadRatio, DefaultGuardMargin)
	if d.Skip {
		t.Fatal("nothing cached means nothing to protect")
	}
	if d.Reason != "no_cached_prefix" {
		t.Fatalf("unexpected reason %q", d.Reason)
	}
}

func TestEvaluateGuard_NoDiscountNeverSkips(t *testing.T) {
	ps := PrefixState{FrozenTokens: 100000}
	d := EvaluatePrefixCacheGuard(ps, 100000, 90000, 1.0, DefaultGuardMargin)
	if d.Skip {
		t.Fatal("a provider without a cache discount has nothing to protect")
	}
	if d.Reason != "no_cache_discount" {
		t.Fatalf("unexpected reason %q", d.Reason)
	}
}

func TestPredictRewrittenTokens_ShrinksLongConversation(t *testing.T) {
	body := buildAnthropicBody(t, 20, false)
	predicted, would := PredictRewrittenTokens(body, APIAnthropic, 4, 50000, DefaultStateSummaryChars)
	if !would {
		t.Fatal("a 20-turn conversation should be rewritable with a 4-step window")
	}
	if predicted >= 50000 {
		t.Fatalf("predicted %d should be below the original 50000", predicted)
	}
}

func TestPredictRewrittenTokens_ShortConversationIsNoop(t *testing.T) {
	body := buildAnthropicBody(t, 2, false)
	predicted, would := PredictRewrittenTokens(body, APIAnthropic, 4, 5000, DefaultStateSummaryChars)
	if would {
		t.Fatal("a 2-turn conversation is inside the window; no rewrite should happen")
	}
	if predicted != 5000 {
		t.Fatalf("a no-op rewrite must report the original size, got %d", predicted)
	}
}

func TestPredictRewrittenTokens_MatchesRealRewrite(t *testing.T) {
	// The predictor must track the real rewriter, not an independent model of
	// it. Same body, same window: the probe's byte ratio and a real rewrite's
	// byte ratio must agree closely.
	body := buildOpenAIBody(t, 15)
	predicted, would := PredictRewrittenTokens(body, APIOpenAI, 4, 40000, DefaultStateSummaryChars)
	if !would {
		t.Fatal("expected a rewrite")
	}

	real, err := rewriteOpenAIBodyWithWindow(body, &Result{
		StateJSON: syntheticStateJSON(DefaultStateSummaryChars),
	}, 4)
	if err != nil {
		t.Fatalf("real rewrite failed: %v", err)
	}
	// The prediction is the rewriter's byte ratio plus the room the rewrite
	// keeps for recovered context, so compare against the same allowance.
	actual := withCCRAllowance(40000*len(real)/len(body), 40000)
	if predicted != actual {
		t.Fatalf("predictor drifted from rewriter: predicted %d, actual %d", predicted, actual)
	}
}

// The probe rewrites with an empty Result, so it never sees the CCR messages
// the real rewrite may append. Without an allowance the guard compares
// passthrough against a body smaller than the one actually sent, and can bust
// a live cache for a rewrite that barely shrank.
func TestWithCCRAllowance_BoundedByInjectionCeiling(t *testing.T) {
	const original = 100000
	ceiling := ccrCeiling(original)

	// Room for the full budget: it is added in full.
	if got, want := withCCRAllowance(10000, original), 10000+ccrRetrievalBudget; got != want {
		t.Errorf("with headroom: got %d, want %d", got, want)
	}
	// Near the ceiling: the allowance stops there rather than overshooting.
	if got := withCCRAllowance(ceiling-100, original); got != ceiling {
		t.Errorf("near ceiling: got %d, want %d", got, ceiling)
	}
	// At or past the ceiling the rewriter injects nothing, so nothing is added.
	if got := withCCRAllowance(ceiling+500, original); got != ceiling+500 {
		t.Errorf("past ceiling: got %d, want %d", got, ceiling+500)
	}
}

// A margin at or above 1.0 drives the comparison threshold to zero, which would
// make the guard skip every request regardless of what a rewrite would save.
func TestEvaluatePrefixCacheGuard_MarginIsClamped(t *testing.T) {
	ps := PrefixState{FrozenTokens: 1000, Explicit: true}
	// A rewrite that is dramatically cheaper than passthrough.
	d := EvaluatePrefixCacheGuard(ps, 10000, 100, 0.1, 5.0)
	if d.Skip {
		t.Fatalf("a margin of 5.0 disabled rewriting entirely: reason=%q rewrite=%v passthrough=%v",
			d.Reason, d.RewriteCost, d.PassthroughCost)
	}
	if d.Reason != "rewrite_cheaper" {
		t.Errorf("reason = %q, want rewrite_cheaper", d.Reason)
	}
}

func TestCacheReadRatioFor_DefaultsAndOverrides(t *testing.T) {
	if got := CacheReadRatioFor(APIAnthropic, 0, 0); got != DefaultAnthropicCacheReadRatio {
		t.Fatalf("anthropic default = %v", got)
	}
	if got := CacheReadRatioFor(APIOpenAI, 0, 0); got != DefaultOpenAICacheReadRatio {
		t.Fatalf("openai default = %v", got)
	}
	if got := CacheReadRatioFor(APIAnthropic, 0.25, 0.75); got != 0.25 {
		t.Fatalf("anthropic override = %v", got)
	}
	if got := CacheReadRatioFor(APIOpenAI, 0.25, 0.75); got != 0.75 {
		t.Fatalf("openai override = %v", got)
	}
}

// Implicit-cache detection is opt-in: without evidence the guard must stand
// aside rather than guess a warm cache and disable HiveState for nothing.
func TestDetectPrefixState_ImplicitDetectionOffByDefault(t *testing.T) {
	body := buildOpenAIBody(t, 8)
	ps := DetectPrefixState(body, APIOpenAI, 20000, DefaultMinCacheableTokens, false, 0)
	if ps.FrozenTokens != 0 {
		t.Fatalf("implicit detection is off; nothing should be reported frozen, got %d", ps.FrozenTokens)
	}
	if ps.Reason != "implicit_cache_detection_off" {
		t.Fatalf("unexpected reason %q", ps.Reason)
	}
}

// Anthropic's cache_control markers are evidence, not inference, so they are
// honoured whether or not implicit detection is enabled.
func TestDetectPrefixState_AnthropicMarkersHonouredRegardless(t *testing.T) {
	body := buildAnthropicBody(t, 10, true)
	for _, assume := range []bool{false, true} {
		ps := DetectPrefixState(body, APIAnthropic, 50000, DefaultMinCacheableTokens, assume, 0)
		if ps.FrozenTokens < 40000 {
			t.Fatalf("assumeImplicit=%v: explicit markers must always be honoured, got %d",
				assume, ps.FrozenTokens)
		}
	}
}

// originalTokens counts the system prompt — parseAnthropicMessages prepends it
// as a message — so the byte ratio has to span it too. Counting system in the
// multiplicand but not in the ratio understated the live cache and inflated the
// passthrough cost, which is the direction that approves a rewrite that should
// have been refused.
func TestDetectAnthropicPrefix_SystemCountsTowardTheFrozenShare(t *testing.T) {
	filler := strings.Repeat("context ", 200)
	msgs := []map[string]any{
		{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "first " + filler, "cache_control": map[string]string{"type": "ephemeral"}},
		}},
	}
	// Many unmarked turns after the breakpoint, so messages[0] is a small
	// share of the message bytes while the marked prefix is most of the prompt.
	for i := 0; i < 20; i++ {
		msgs = append(msgs, map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "text", "text": "reply " + filler}}})
		msgs = append(msgs, map[string]any{"role": "user",
			"content": []map[string]any{{"type": "text", "text": "next " + filler}}})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4",
		"system":   strings.Repeat("system instructions ", 2000),
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}

	const originalTokens = 20000
	ps := detectAnthropicPrefix(body, originalTokens)
	if !ps.Explicit || ps.Reason != "anthropic_cache_control" {
		t.Fatalf("expected an explicit marker detection, got %+v", ps)
	}

	// The system prompt alone is a large share of the body, and it sits ahead
	// of the breakpoint, so the frozen estimate cannot be a sliver.
	var b struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	total := len(b.System)
	for _, m := range b.Messages {
		total += len(m)
	}
	wantAtLeast := originalTokens * len(b.System) / total
	if ps.FrozenTokens < wantAtLeast {
		t.Fatalf("frozen tokens %d is below the system prompt's own share (%d): "+
			"the preamble was left out of the ratio", ps.FrozenTokens, wantAtLeast)
	}
}
