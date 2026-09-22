package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// A model with no deployments at all (unregistered) has nothing for this
// check to refuse — the model lookup itself refuses the request for its own
// reason downstream.
func TestIsProviderAllowed_NoProvidersToCheckAlwaysPasses(t *testing.T) {
	restricted := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, AllowedProviders: store.StringList{"openai"}})
	assert.True(t, IsProviderAllowed(restricted, nil))
}

func TestIsProviderAllowed_NoAllowlistReachesAnyProvider(t *testing.T) {
	unrestricted := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true}) // AllowedProviders nil
	assert.True(t, IsProviderAllowed(unrestricted, []string{"anthropic"}))

	assert.True(t, IsProviderAllowed(context.Background(), []string{"anthropic"}),
		"a request with no key info at all is not this check's business")
}

func TestIsProviderAllowed_KeyAllowlistBlocksProvidersNotListed(t *testing.T) {
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, AllowedProviders: store.StringList{"openai"}})

	assert.True(t, IsProviderAllowed(key, []string{"openai"}))
	assert.False(t, IsProviderAllowed(key, []string{"anthropic"}))
}

func TestIsProviderAllowed_AnyMatchingProviderIsEnough(t *testing.T) {
	// A model with deployments on two providers, only one of which is allowed.
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, AllowedProviders: store.StringList{"azure"}})
	assert.True(t, IsProviderAllowed(key, []string{"openai", "azure"}))
}

// Revoking every provider produced an empty allow-list, which must read as
// "nothing", not "no restriction" — the same NULL-vs-[] distinction Models
// already draws.
func TestIsProviderAllowed_EmptyAllowlistDeniesEveryProvider(t *testing.T) {
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, AllowedProviders: store.StringList{}})
	assert.False(t, IsProviderAllowed(key, []string{"openai"}))
}

func TestIsProviderAllowed_CaseInsensitiveMatch(t *testing.T) {
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, AllowedProviders: store.StringList{"OpenAI"}})
	assert.True(t, IsProviderAllowed(key, []string{"openai"}))
}

func TestIsProviderAllowed_DenyTakesPrecedenceAndLeavesAlternatives(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{
		AllowedProviders: store.StringList{"openai", "azure"},
		DeniedProviders:  store.StringList{"OPENAI"},
	})
	assert.False(t, IsProviderNameAllowed(ctx, "openai"))
	assert.True(t, IsProviderNameAllowed(ctx, "azure"))
	assert.True(t, IsProviderAllowed(ctx, []string{"openai", "azure"}))
	assert.False(t, IsProviderAllowed(ctx, []string{"openai"}))
}
