package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletionRequest_PreservesUnknownFields(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"hi"}],
		"seed":123,
		"logit_bias":{"42":-100},
		"metadata":{"tenant":"acme"}
	}`)

	var req CompletionRequest
	require.NoError(t, json.Unmarshal(body, &req))

	assert.Equal(t, "gpt-4o", req.Model)
	assert.Equal(t, float64(123), req.Extra["seed"])
	assert.Contains(t, req.Extra, "logit_bias")
	assert.Contains(t, req.Extra, "metadata")

	encoded, err := json.Marshal(&req)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(encoded, &out))
	assert.Equal(t, float64(123), out["seed"])
	assert.Contains(t, out, "logit_bias")
	assert.Contains(t, out, "metadata")
}
