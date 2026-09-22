package provider

import (
	"encoding/json"
	"testing"
)

// OpenAI counts cached tokens *inside* prompt_tokens, so the total must not
// move — only the breakdown is recovered.
func TestUsage_OpenAICachedTokensAreASubsetOfPrompt(t *testing.T) {
	var u Usage
	err := json.Unmarshal([]byte(`{
		"prompt_tokens": 10000,
		"completion_tokens": 500,
		"total_tokens": 10500,
		"prompt_tokens_details": {"cached_tokens": 8000}
	}`), &u)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if u.PromptTokens != 10000 {
		t.Fatalf("prompt total must be unchanged, got %d", u.PromptTokens)
	}
	if u.CachedTokens() != 8000 {
		t.Fatalf("cached tokens = %d, want 8000", u.CachedTokens())
	}
	if u.UncachedPromptTokens() != 2000 {
		t.Fatalf("uncached = %d, want 2000", u.UncachedPromptTokens())
	}
	if !u.CacheReported() {
		t.Fatal("a reported breakdown must be marked measured")
	}
}

// Absence of the detail block is absence of data, not an observation of zero.
func TestUsage_MissingDetailsIsNotMeasured(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.CacheReported() {
		t.Fatal("no detail block means the cache split was never reported")
	}
	if u.UncachedPromptTokens() != 100 {
		t.Fatalf("uncached = %d, want 100", u.UncachedPromptTokens())
	}
}

// The internal cache fields must never reach the client: the response body a
// caller receives keeps the provider's own shape.
func TestUsage_CacheFieldsAreNotSerialized(t *testing.T) {
	u := Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	u.SetCacheUsage(80, 5)
	out, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"cached_prompt_tokens", "cache_creation_tokens", "cache_usage_measured", "cachedPromptTokens"} {
		if _, present := round[k]; present {
			t.Fatalf("internal field %q leaked into the client-visible payload: %s", k, out)
		}
	}
	if len(round) != 3 {
		t.Fatalf("payload shape changed, got %s", out)
	}
}

func TestUsage_UncachedNeverNegative(t *testing.T) {
	u := Usage{PromptTokens: 100}
	u.SetCacheUsage(500, 200)
	if got := u.UncachedPromptTokens(); got != 0 {
		t.Fatalf("over-reported cache must clamp to 0, got %d", got)
	}
}
