package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// An unregistered model has nothing for this check to refuse — the model
// lookup itself refuses the request for its own reason downstream.
func TestIsResidencyAllowed_NoDeploymentsAlwaysPasses(t *testing.T) {
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, RequireEUResidency: true})
	assert.True(t, IsResidencyAllowed(key, false, false))
}

func TestIsResidencyAllowed_NoRestrictionAllowsNonEU(t *testing.T) {
	unrestricted := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true}) // RequireEUResidency false
	assert.True(t, IsResidencyAllowed(unrestricted, false, true))

	assert.True(t, IsResidencyAllowed(context.Background(), false, true),
		"a request with no key info at all is not this check's business")
}

func TestIsResidencyAllowed_RestrictedKeyRequiresAllDeploymentsEU(t *testing.T) {
	key := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "abc123", Active: true, RequireEUResidency: true})

	assert.True(t, IsResidencyAllowed(key, true, true))
	assert.False(t, IsResidencyAllowed(key, false, true),
		"even one non-EU deployment must fail a residency-required key")
}
