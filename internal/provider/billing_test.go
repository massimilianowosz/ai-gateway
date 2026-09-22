package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// Only a model whose every deployment is flat-billed is free to the gateway.
// A mixed model is billed, or a caller exempt from the budget gate could reach
// the metered half of it.
func TestRegistry_IsGatewayBilled(t *testing.T) {
	r := &Registry{deployments: map[string][]*Deployment{
		"oauth-only": {{BillingMode: config.BillingModeFlat}},
		"mixed":      {{BillingMode: config.BillingModeFlat}, {BillingMode: "metered"}},
		"metered":    {{BillingMode: "metered"}},
		"default":    {{}},
	}}

	assert.False(t, r.IsGatewayBilled("oauth-only"))
	assert.True(t, r.IsGatewayBilled("mixed"), "a mixed model must count as billed")
	assert.True(t, r.IsGatewayBilled("metered"))
	assert.True(t, r.IsGatewayBilled("default"), "an unset billing mode is metered")
	assert.True(t, r.IsGatewayBilled("unknown"), "an unknown model must count as billed")
}
