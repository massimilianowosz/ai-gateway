package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// A key with no budget may still call a model the gateway does not pay for:
// such a call cannot consume a budget it was never given. Anything the gateway
// pays for needs one.
//
// The model is what decides it, not the key's whitelist — that governs which
// restricted models a tenant may see, which is a different question.
func TestCheckBudget_UnfundedKeyOnlyReachesFlatModels(t *testing.T) {
	unfunded := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "k", Active: true, Budget: 0})

	assert.Equal(t, BudgetOK, CheckBudget(unfunded, false),
		"an OAuth pass-through model costs the gateway nothing")
	assert.Equal(t, BudgetUnfunded, CheckBudget(unfunded, true),
		"a model the gateway pays for needs a budget")
}

// A funded key is unaffected either way; an exhausted one was already stopped
// in authentication, which does not need the model to know that.
func TestCheckBudget_FundedKeyIsUnaffected(t *testing.T) {
	funded := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "k", Active: true, Budget: 10})

	assert.Equal(t, BudgetOK, CheckBudget(funded, true))
	assert.Equal(t, BudgetOK, CheckBudget(funded, false))
}

// No key in context — the master key, or pass-through — is not gated.
func TestCheckBudget_NoKeyIsNotGated(t *testing.T) {
	assert.Equal(t, BudgetOK, CheckBudget(context.Background(), true))
}

// The master key is the operator and a pass-through caller pays its own
// upstream: neither carries a budget, and gating them locked the operator out
// of the gateway it administers.
func TestCheckBudget_PrivilegedPrincipalsAreNotGated(t *testing.T) {
	master := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "master", Name: "master", Active: true})
	passthrough := context.WithValue(context.Background(), keyInfoKey,
		&store.APIKey{KeyHash: "passthrough", Name: "passthrough", Active: true})

	assert.Equal(t, BudgetOK, CheckBudget(master, true))
	assert.Equal(t, BudgetOK, CheckBudget(passthrough, true))
}
