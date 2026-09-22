package anthropic

import (
	"encoding/json"
	"testing"
)

// Anthropic reports cache tokens as siblings of input_tokens, not inside it.
// Billing input_tokens alone undercounts every cached request.
func TestAnthropicUsage_CacheTokensAreAddedToPromptTotal(t *testing.T) {
	var u anthropicUsage
	err := json.Unmarshal([]byte(`{
		"input_tokens": 1200,
		"output_tokens": 800,
		"cache_read_input_tokens": 18000,
		"cache_creation_input_tokens": 400
	}`), &u)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := u.toProviderUsage()

	// 1200 fresh + 18000 read + 400 written = 19600 billable input tokens.
	// The old mapping reported 1200 and silently dropped the other 18400.
	if got.PromptTokens != 19600 {
		t.Fatalf("prompt tokens = %d, want 19600", got.PromptTokens)
	}
	if got.CachedTokens() != 18000 {
		t.Fatalf("cached = %d, want 18000", got.CachedTokens())
	}
	if got.CacheCreation() != 400 {
		t.Fatalf("created = %d, want 400", got.CacheCreation())
	}
	if got.UncachedPromptTokens() != 1200 {
		t.Fatalf("uncached = %d, want 1200", got.UncachedPromptTokens())
	}
	if got.TotalTokens != 20400 {
		t.Fatalf("total = %d, want 20400", got.TotalTokens)
	}
	if !got.CacheReported() {
		t.Fatal("Anthropic always reports the split; it must be marked measured")
	}
}

// Thinking is a subset of output_tokens, not a sibling like the cache
// counters, so it is reported alongside rather than added. Without it there is
// no way to tell a turn that thought for a minute from one that answered
// immediately — and on a reasoning model that is most of the output bill.
func TestAnthropicUsage_ThinkingTokensAreCarriedNotAdded(t *testing.T) {
	var u anthropicUsage
	if err := json.Unmarshal([]byte(`{
		"input_tokens": 100,
		"output_tokens": 5000,
		"output_tokens_details": {"thinking_tokens": 4800}
	}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := u.toProviderUsage()

	if got.CompletionTokens != 5000 {
		t.Fatalf("completion = %d, want 5000 (thinking must not be added on top)", got.CompletionTokens)
	}
	if got.CompletionTokensDetails == nil {
		t.Fatal("thinking tokens were dropped")
	}
	if got.CompletionTokensDetails.ReasoningTokens != 4800 {
		t.Fatalf("thinking = %d, want 4800", got.CompletionTokensDetails.ReasoningTokens)
	}
}

// Anthropic sends the thinking breakdown only on the final message_delta, so
// it has to survive the merge that assembles usage across stream events.
func TestAnthropicUsage_ThinkingSurvivesStreamMerge(t *testing.T) {
	var acc anthropicUsage
	acc.mergeUsage(&anthropicUsage{InputTokens: 900}) // message_start
	final := &anthropicUsage{OutputTokens: 5000}
	final.OutputTokensDetails.ThinkingTokens = 4800
	acc.mergeUsage(final) // message_delta

	got := acc.toProviderUsage()
	if got.PromptTokens != 900 {
		t.Fatalf("prompt = %d, want 900", got.PromptTokens)
	}
	if got.CompletionTokensDetails == nil || got.CompletionTokensDetails.ReasoningTokens != 4800 {
		t.Fatalf("thinking lost across the merge: %+v", got.CompletionTokensDetails)
	}
}

// A request that used no caching must behave exactly as before.
func TestAnthropicUsage_NoCacheIsUnchanged(t *testing.T) {
	var u anthropicUsage
	if err := json.Unmarshal([]byte(`{"input_tokens": 500, "output_tokens": 100}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := u.toProviderUsage()
	if got.PromptTokens != 500 || got.CompletionTokens != 100 || got.TotalTokens != 600 {
		t.Fatalf("uncached request changed shape: %+v", got)
	}
	if got.CachedTokens() != 0 || got.CacheCreation() != 0 {
		t.Fatalf("expected no cache tokens, got %+v", got)
	}
}
