package proxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func withKey(ctx context.Context, contextInstructions string) context.Context {
	return auth.ContextWithKeyInfo(ctx, &store.APIKey{ContextInstructions: contextInstructions})
}

func TestInjectContextPackInstructions_NoKey_NoOp(t *testing.T) {
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	injectContextPackInstructions(context.Background(), req)
	if len(req.Messages) != 1 {
		t.Fatalf("expected no message added, got %d messages", len(req.Messages))
	}
}

func TestInjectContextPackInstructions_EmptyBundle_NoOp(t *testing.T) {
	ctx := withKey(context.Background(), "")
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	injectContextPackInstructions(ctx, req)
	if len(req.Messages) != 1 {
		t.Fatalf("expected no message added, got %d messages", len(req.Messages))
	}
}

func TestInjectContextPackInstructions_NoExistingSystemMessage_Prepends(t *testing.T) {
	ctx := withKey(context.Background(), "Always answer in French.")
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	injectContextPackInstructions(ctx, req)

	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "Always answer in French." {
		t.Fatalf("expected a leading system message with the bundle, got %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" {
		t.Fatalf("expected the caller's own message to survive unchanged, got %+v", req.Messages[1])
	}
}

func TestInjectContextPackInstructions_ExistingSystemMessage_MergesIntoOne(t *testing.T) {
	ctx := withKey(context.Background(), "Always answer in French.")
	req := &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi"},
		},
	}
	injectContextPackInstructions(ctx, req)

	// Exactly one system message: a second one would be silently dropped by
	// at least one downstream provider encoder (Anthropic's "last system
	// wins" conversion), so merging is the only correct behavior here.
	systemCount := 0
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("expected exactly one system message, found %d", systemCount)
	}
	want := "Always answer in French.\n\nYou are a helpful assistant."
	if req.Messages[0].Content != want {
		t.Fatalf("expected merged system content %q, got %q", want, req.Messages[0].Content)
	}
}

func TestInjectContextPackInstructions_NonStringSystemContent_PrependsInstead(t *testing.T) {
	ctx := withKey(context.Background(), "Always answer in French.")
	req := &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: "system", Content: []map[string]string{{"type": "text", "text": "structured"}}},
			{Role: "user", Content: "hi"},
		},
	}
	injectContextPackInstructions(ctx, req)

	if len(req.Messages) != 3 {
		t.Fatalf("expected the bundle prepended as its own message, got %d messages", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "Always answer in French." {
		t.Fatalf("expected a leading system message with the bundle, got %+v", req.Messages[0])
	}
}

func TestMergeInstructionsField(t *testing.T) {
	cases := []struct {
		name     string
		existing string // raw JSON, "" means absent
		bundle   string
		want     string // decoded string, "" means "left alone" (checked separately)
	}{
		{name: "absent becomes the bundle", existing: "", bundle: "bundle text", want: "bundle text"},
		{name: "empty string becomes the bundle", existing: `""`, bundle: "bundle text", want: "bundle text"},
		{name: "existing text is appended after the bundle", existing: `"be concise"`, bundle: "bundle text", want: "bundle text\n\nbe concise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var existing []byte
			if tc.existing != "" {
				existing = []byte(tc.existing)
			}
			got, err := mergeInstructionsField(existing, tc.bundle)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var gotText string
			if err := json.Unmarshal(got, &gotText); err != nil {
				t.Fatalf("result was not a JSON string: %v (%s)", err, got)
			}
			if gotText != tc.want {
				t.Fatalf("want %q, got %q", tc.want, gotText)
			}
		})
	}
}

func TestMergeInstructionsField_NonStringExisting_LeftAlone(t *testing.T) {
	existing := []byte(`{"foo":"bar"}`)
	got, err := mergeInstructionsField(existing, "bundle text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(existing) {
		t.Fatalf("expected non-string existing value to be left untouched, got %s", got)
	}
}
