package hivestate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func configWithThreshold(threshold int) config.HiveStateConfig {
	return config.HiveStateConfig{
		Enabled:      true,
		Threshold:    threshold,
		MaxLatencyMs: 3000,
		StepWindow:   1, // small window so tests have extractable history
	}
}

func TestSplitMessages_BasicConversation(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "I want to book a restaurant"},
		{Role: "assistant", Content: "Sure! What cuisine?"},
		{Role: "user", Content: "Italian, north area"},
		{Role: "assistant", Content: "How many people?"},
		{Role: "user", Content: "6 people, Friday 8pm"},
	}

	// With stepWindow=1: 2 assistant steps in body, keep last 1 → first step goes to History
	zones := SplitMessages(msgs, 1)

	if len(zones.System) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(zones.System))
	}
	if zones.System[0].Content != "You are a helpful assistant." {
		t.Errorf("system content mismatch")
	}
	if zones.Last.Content != "6 people, Friday 8pm" {
		t.Errorf("last user = %q, want %q", zones.Last.Content, "6 people, Friday 8pm")
	}
	// Step split: body=[user1, asst1, user2, asst2], last step is asst2 at idx 3
	// History = body[:3] = [user1, asst1, user2], Recent = body[3:] = [asst2]
	if len(zones.History) != 3 {
		t.Fatalf("expected 3 history messages, got %d", len(zones.History))
	}
	if len(zones.Recent) != 1 {
		t.Fatalf("expected 1 recent message, got %d", len(zones.Recent))
	}
}

func TestSplitMessages_NoHistory(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
	}

	zones := SplitMessages(msgs, 20)

	if len(zones.System) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(zones.System))
	}
	if zones.Last.Content != "Hello" {
		t.Errorf("last user = %q", zones.Last.Content)
	}
	if len(zones.History) != 0 {
		t.Errorf("expected 0 history, got %d", len(zones.History))
	}
}

func TestSplitMessages_MultipleSystemMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "System instruction 1"},
		{Role: "system", Content: "System instruction 2"},
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "Done"},
		{Role: "user", Content: "Do another thing"},
		{Role: "assistant", Content: "Also done"},
		{Role: "user", Content: "Do more"},
	}

	// With stepWindow=1: 2 assistant steps in body, keep last 1
	zones := SplitMessages(msgs, 1)

	if len(zones.System) != 2 {
		t.Fatalf("expected 2 system messages, got %d", len(zones.System))
	}
	if zones.Last.Content != "Do more" {
		t.Errorf("last user = %q", zones.Last.Content)
	}
	// body=[user1, asst1, user2, asst2], last step is asst2 at idx 3
	// History = body[:3] = [user1, asst1, user2], Recent = body[3:] = [asst2]
	if len(zones.History) != 3 {
		t.Fatalf("expected 3 history messages, got %d", len(zones.History))
	}
	if len(zones.Recent) != 1 {
		t.Fatalf("expected 1 recent message, got %d", len(zones.Recent))
	}
}

func TestSplitMessages_OnlySystem(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
	}

	zones := SplitMessages(msgs, 20)

	if len(zones.System) != 1 {
		t.Fatalf("expected 1 system, got %d", len(zones.System))
	}
	if zones.Last.Content != "" {
		t.Errorf("expected empty last user, got %q", zones.Last.Content)
	}
}

func TestSplitMessages_Empty(t *testing.T) {
	zones := SplitMessages(nil, 20)
	if len(zones.System) != 0 || len(zones.History) != 0 {
		t.Error("expected empty zones for nil input")
	}
}

func TestTokenCounter(t *testing.T) {
	counter := NewTokenCounter()

	// "hello world" = 11 chars → 11/4 = 2
	if got := counter.Count("hello world"); got != 2 {
		t.Errorf("Count(\"hello world\") = %d, want 2", got)
	}

	// Single char → at least 1
	if got := counter.Count("x"); got != 1 {
		t.Errorf("Count(\"x\") = %d, want 1", got)
	}

	// Empty → 0
	if got := counter.Count(""); got != 0 {
		t.Errorf("Count(\"\") = %d, want 0", got)
	}

	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Book a restaurant"},
	}
	total := counter.CountMessages(msgs)
	if total <= 0 {
		t.Errorf("CountMessages returned %d, want > 0", total)
	}
}

func TestExtractContent_PlainJSON(t *testing.T) {
	input := `{"intent": "book_restaurant"}`
	got := extractContent(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestExtractContent_MarkdownFenced(t *testing.T) {
	input := "```json\n{\"intent\": \"test\"}\n```"
	want := `{"intent": "test"}`
	got := extractContent(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractContent_GenericFence(t *testing.T) {
	input := "```\n{\"intent\": \"test\"}\n```"
	want := `{"intent": "test"}`
	got := extractContent(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProcess_BelowThreshold(t *testing.T) {
	hs := &HiveState{
		cfg:     configWithThreshold(99999),
		counter: NewTokenCounter(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello there, how can I help?"},
		{Role: "user", Content: "I need to find a hotel"},
		{Role: "assistant", Content: "Sure, what area?"},
		{Role: "user", Content: "book restaurant"},
	}

	result := hs.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if result.Mode != ModeNoOp {
		t.Errorf("mode = %s, want %s", result.Mode, ModeNoOp)
	}
	if !strings.HasPrefix(result.FallbackReason, "below_threshold") {
		t.Errorf("fallback = %q, want prefix below_threshold", result.FallbackReason)
	}
}

func TestWithOverrides_NoOverridesReturnsSameInstance(t *testing.T) {
	hs := &HiveState{cfg: configWithThreshold(100), counter: NewTokenCounter(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got := hs.WithOverrides(CompressionOverrides{})
	if got != hs {
		t.Error("WithOverrides with a zero-value override must return the same instance, not a copy")
	}
}

func TestWithOverrides_ThresholdOverrideChangesBelowThresholdDecision(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello there, how can I help?"},
		{Role: "user", Content: "I need to find a hotel"},
		{Role: "assistant", Content: "Sure, what area?"},
		{Role: "user", Content: "book restaurant"},
	}
	hs := &HiveState{cfg: configWithThreshold(1), counter: NewTokenCounter(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// The base engine's low threshold means this conversation is never
	// below it — confirm that first, so the override below is the only
	// thing that can produce a below_threshold result.
	base := hs.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if strings.HasPrefix(base.FallbackReason, "below_threshold") {
		t.Fatalf("fixture assumption broken: base engine already treats this conversation as below threshold (%s)", base.FallbackReason)
	}

	narrowedThreshold := 999999
	overridden := hs.WithOverrides(CompressionOverrides{Threshold: &narrowedThreshold})
	result := overridden.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if result.Mode != ModeNoOp || !strings.HasPrefix(result.FallbackReason, "below_threshold") {
		t.Errorf("mode = %s, fallback = %q; a per-key threshold override must actually change Process's decision", result.Mode, result.FallbackReason)
	}

	// The base engine, untouched, must still behave as before: WithOverrides
	// must not have mutated the shared instance.
	again := hs.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if strings.HasPrefix(again.FallbackReason, "below_threshold") {
		t.Error("WithOverrides must return a copy, not mutate the shared engine")
	}
}

func TestProcess_NoExtractor(t *testing.T) {
	hs := &HiveState{
		cfg:     configWithThreshold(1), // very low threshold
		counter: NewTokenCounter(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "I want italian food in the north area"},
		{Role: "assistant", Content: "How many people?"},
		{Role: "user", Content: "6 people please"},
		{Role: "assistant", Content: "What day?"},
		{Role: "user", Content: "6 people on friday at 8pm"},
	}

	result := hs.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if result.Mode != ModeNoOp {
		t.Errorf("mode = %s, want %s", result.Mode, ModeNoOp)
	}
	if result.FallbackReason != "no_model" {
		t.Errorf("fallback = %q, want no_model", result.FallbackReason)
	}
}

func TestProcess_NoHistory(t *testing.T) {
	hs := &HiveState{
		cfg:     configWithThreshold(1),
		counter: NewTokenCounter(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
	}

	result := hs.Process(context.TODO(), msgs, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})
	if result.Mode != ModeNoOp {
		t.Errorf("mode = %s, want %s", result.Mode, ModeNoOp)
	}
	if result.FallbackReason != "no_history" {
		t.Errorf("fallback = %q, want no_history", result.FallbackReason)
	}
}
