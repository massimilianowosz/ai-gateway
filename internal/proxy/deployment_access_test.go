package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestDeniedModelPhrase(t *testing.T) {
	// A pass-through deployment is renamed in the catalogue, so the caller is
	// refused a name it never sent; both have to appear or the message sends
	// the operator looking for the wrong allow-list entry.
	resolved := middleware.ContextWithRequestedModel(context.Background(), "claude-sonnet-5")
	assert.Equal(t,
		`model "claude-code-sonnet-5" (requested as "claude-sonnet-5")`,
		deniedModelPhrase(resolved, "claude-code-sonnet-5"))

	same := middleware.ContextWithRequestedModel(context.Background(), "gpt-5")
	assert.Equal(t, `model "gpt-5"`, deniedModelPhrase(same, "gpt-5"),
		"an unresolved name must not be repeated twice")

	assert.Equal(t, `model "gpt-5"`, deniedModelPhrase(context.Background(), "gpt-5"))
}

func TestUnavailableModelPhrase(t *testing.T) {
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          "claude-code-sonnet-5",
		Provider:      "mock",
		ProviderModel: "claude-sonnet-5",
		AuthMode:      config.AuthModeOAuthPassthrough,
	}}, &staticFactory{provider: &mockProvider{}})
	require.NoError(t, err)

	// The deployment exists; only the caller's credential is missing, and
	// "not available" would send an operator hunting for a missing model.
	assert.Contains(t,
		unavailableModelPhrase(registry, "claude-sonnet-5"),
		"only served by an OAuth pass-through deployment")

	assert.Equal(t, `model "gpt-9-turbo" is not available`,
		unavailableModelPhrase(registry, "gpt-9-turbo"))
	assert.Equal(t, `model "x" is not available`, unavailableModelPhrase(nil, "x"))
}
