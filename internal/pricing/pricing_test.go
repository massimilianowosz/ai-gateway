package pricing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"io"
	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCalculator_Cost_ExactMatch(t *testing.T) {
	c := &Calculator{prices: map[string]ModelPrice{
		"gpt-4o": {InputCostPerToken: 0.000005, OutputCostPerToken: 0.000015},
	}}

	cost := c.Cost("gpt-4o", 1000, 500)
	expected := 1000*0.000005 + 500*0.000015
	assert.InDelta(t, expected, cost, 1e-10)
}

func TestCalculator_Cost_ProviderPrefixStripped(t *testing.T) {
	c := &Calculator{prices: map[string]ModelPrice{
		"gpt-4o": {InputCostPerToken: 0.000005, OutputCostPerToken: 0.000015},
	}}

	// "azure/gpt-4o" → strip prefix → "gpt-4o"
	cost := c.Cost("azure/gpt-4o", 1000, 500)
	expected := 1000*0.000005 + 500*0.000015
	assert.InDelta(t, expected, cost, 1e-10)
}

func TestCalculator_Cost_UnknownModel_ReturnsZero(t *testing.T) {
	c := &Calculator{prices: map[string]ModelPrice{}}
	assert.Equal(t, 0.0, c.Cost("unknown-model-xyz", 1000, 500))
}

func TestCalculator_Cost_ZeroTokens(t *testing.T) {
	c := &Calculator{prices: map[string]ModelPrice{
		"claude-3-haiku": {InputCostPerToken: 0.00000025, OutputCostPerToken: 0.00000125},
	}}
	assert.Equal(t, 0.0, c.Cost("claude-3-haiku", 0, 0))
}

func TestCalculator_SetPrice(t *testing.T) {
	c := &Calculator{prices: make(map[string]ModelPrice)}
	c.SetPrice("my-model", ModelPrice{InputCostPerToken: 0.001, OutputCostPerToken: 0.002})

	cost := c.Cost("my-model", 10, 5)
	assert.InDelta(t, 0.001*10+0.002*5, cost, 1e-10)
}

func TestCalculator_ModelCount(t *testing.T) {
	c := &Calculator{prices: map[string]ModelPrice{
		"a": {}, "b": {}, "c": {},
	}}
	assert.Equal(t, 3, c.ModelCount())
}

func TestCalculator_ConfigOverrides_TakePriority(t *testing.T) {
	logger := discardLogger()
	overrides := map[string]ModelPrice{
		"gpt-4o": {InputCostPerToken: 0.999, OutputCostPerToken: 0.999},
	}
	c := NewCalculator(logger, "", overrides)

	// Override must beat any embedded price
	cost := c.Cost("gpt-4o", 1, 0)
	assert.InDelta(t, 0.999, cost, 1e-10)
}

func TestCalculator_EmbeddedPrices_LoadedOnInit(t *testing.T) {
	logger := discardLogger()
	c := NewCalculator(logger, "", nil)
	// The embedded model_prices.json has entries; total must be > 0
	assert.Greater(t, c.ModelCount(), 0)
}

func TestCalculator_RemoteRefresh_UpdatesPrices(t *testing.T) {
	prices := map[string]ModelPrice{
		"remote-model": {InputCostPerToken: 0.0001, OutputCostPerToken: 0.0002},
	}
	body, err := json.Marshal(prices)
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	logger := discardLogger()
	c := NewCalculator(logger, server.URL, nil)

	cost := c.Cost("remote-model", 1000, 500)
	expected := 1000*0.0001 + 500*0.0002
	assert.InDelta(t, expected, cost, 1e-10)
}

func TestCalculator_RemoteRefresh_FailsGracefully(t *testing.T) {
	// Server returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	logger := discardLogger()
	// Should not panic; falls back to embedded prices
	c := NewCalculator(logger, server.URL, nil)
	assert.Greater(t, c.ModelCount(), 0)
}

func TestCalculator_Cost_MultipleSlashes(t *testing.T) {
	// "bedrock/us-east-1/claude-3-haiku" → last segment → "claude-3-haiku"
	c := &Calculator{prices: map[string]ModelPrice{
		"claude-3-haiku": {InputCostPerToken: 0.00000025, OutputCostPerToken: 0.00000125},
	}}
	cost := c.Cost("bedrock/us-east-1/claude-3-haiku", 1000, 200)
	expected := 1000*0.00000025 + 200*0.00000125
	assert.InDelta(t, expected, cost, 1e-12)
}
