package guardrail

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// fakeModerationProvider lets tests script the Complete() response returned
// to the moderation scanner without making real network calls.
type fakeModerationProvider struct {
	resp *provider.CompletionResponse
	err  error
}

func (f *fakeModerationProvider) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return f.resp, f.err
}
func (f *fakeModerationProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (f *fakeModerationProvider) Name() string { return "fake" }

type fakeModerationFactory struct {
	p provider.Provider
}

func (f *fakeModerationFactory) Create(_ config.ModelConfig) (provider.Provider, error) {
	return f.p, nil
}

func newTestModerationScanner(t *testing.T, p provider.Provider) *ModerationScanner {
	t.Helper()
	reg, err := provider.NewRegistry([]config.ModelConfig{
		{Name: "moderation-model", Provider: "fake", ProviderModel: "gpt-5-nano"},
	}, &fakeModerationFactory{p: p})
	require.NoError(t, err)
	return NewModerationScanner(reg, "moderation-model", slog.Default())
}

func completionWithContent(content string) *provider.CompletionResponse {
	return &provider.CompletionResponse{
		Choices: []provider.Choice{
			{Message: &provider.Message{Role: "assistant", Content: content}},
		},
		Usage: &provider.Usage{PromptTokens: 12, CompletionTokens: 8},
	}
}

func TestModerationScanner_Name(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{})
	require.Equal(t, "moderation", s.Name())
	require.Equal(t, "moderation-model", s.Model())
}

func TestModerationScanner_BlocksToxicContent(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent(`{"toxic":true,"injection":false,"sexual":false,"category":"harassment","reason":"insulting language"}`),
	})

	result, err := s.Scan(context.Background(), "you are worthless", "you are worthless")
	require.NoError(t, err)
	require.True(t, result.Blocked)
	require.Equal(t, "harassment", result.Category)
	require.Equal(t, "insulting language", result.Reason)
	require.Equal(t, 12, result.PromptTokens)
	require.Equal(t, 8, result.CompletionTokens)
}

func TestModerationScanner_AllowsCleanContent(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent(`{"toxic":false,"injection":false,"sexual":false,"category":"none","reason":""}`),
	})

	result, err := s.Scan(context.Background(), "what's the weather like?", "")
	require.NoError(t, err)
	require.False(t, result.Blocked)
}

func TestModerationScanner_HandlesMarkdownFencedJSON(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent("```json\n{\"toxic\":true,\"injection\":false,\"sexual\":false,\"category\":\"hate\",\"reason\":\"slur\"}\n```"),
	})

	result, err := s.Scan(context.Background(), "text", "text")
	require.NoError(t, err)
	require.True(t, result.Blocked)
	require.Equal(t, "hate", result.Category)
}

func TestModerationScanner_MalformedJSON_FailsOpenWithoutError(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent("not valid json at all"),
	})

	result, err := s.Scan(context.Background(), "text", "text")
	require.NoError(t, err, "parse errors should fail open, not be surfaced as scan errors")
	require.False(t, result.Blocked)
}

func TestModerationScanner_NoChoicesReturnsError(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: &provider.CompletionResponse{Choices: nil},
	})

	_, err := s.Scan(context.Background(), "text", "text")
	require.Error(t, err)
}

func TestModerationScanner_ProviderErrorPropagates(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		err: context.DeadlineExceeded,
	})

	_, err := s.Scan(context.Background(), "text", "text")
	require.Error(t, err)
}

func TestModerationScanner_DeploymentNotFound(t *testing.T) {
	reg, err := provider.NewRegistry(nil, &fakeModerationFactory{})
	require.NoError(t, err)
	s := NewModerationScanner(reg, "missing-model", slog.Default())

	_, err = s.Scan(context.Background(), "text", "text")
	require.Error(t, err)
}

// TestModerationScanner_SkipsWhenEUOnlyKeyMeetsANonEUModel pins GW-02/CTX-03:
// an EU-only key's prompt must never reach this auxiliary classifier when
// the classifier's own model has no all-EU deployment. Skipping must be
// silent (no block, no error) — the request continues without moderation,
// not blocked and not sent out of the EU.
func TestModerationScanner_SkipsWhenEUOnlyKeyMeetsANonEUModel(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent(`{"toxic":true,"injection":false,"sexual":false,"category":"harassment","reason":"insulting language"}`),
	})
	ctx := auth.ContextWithKeyInfo(context.Background(), &store.APIKey{KeyHash: "eu-only", RequireEUResidency: true})

	result, err := s.Scan(ctx, "you are worthless", "you are worthless")

	require.NoError(t, err)
	require.False(t, result.Blocked, "a residency skip must not block the request")
	require.Zero(t, result.PromptTokens, "the auxiliary model must never have actually been called")
}

func TestModerationScanner_RunsNormallyWithoutResidencyRestriction(t *testing.T) {
	s := newTestModerationScanner(t, &fakeModerationProvider{
		resp: completionWithContent(`{"toxic":true,"injection":false,"sexual":false,"category":"harassment","reason":"insulting language"}`),
	})
	ctx := auth.ContextWithKeyInfo(context.Background(), &store.APIKey{KeyHash: "no-residency"})

	result, err := s.Scan(ctx, "you are worthless", "you are worthless")

	require.NoError(t, err)
	require.True(t, result.Blocked, "a key with no residency requirement must still get a real moderation check")
}
