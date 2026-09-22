package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultPricingURL points to the LiteLLM public pricing file (updated frequently).
	DefaultPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	refreshInterval   = 24 * time.Hour
)

//go:embed model_prices.json
var embeddedPrices []byte

// ModelPrice holds per-token pricing for a model.
//
// The two cache tariffs are optional: providers without a prompt cache omit
// them, and so do catalog entries that predate cache pricing. Resolution falls
// back to InputCostPerToken, which is what the gateway charged before cache
// categories existed — never zero, so a missing tariff cannot silently make
// tokens free.
type ModelPrice struct {
	InputCostPerToken  float64 `json:"input_cost_per_token" yaml:"input_cost_per_token"`
	OutputCostPerToken float64 `json:"output_cost_per_token" yaml:"output_cost_per_token"`
	// CacheReadInputTokenCost is the per-token price of prompt tokens served
	// from the provider's cache (Anthropic ~0.1x input, OpenAI ~0.5x).
	CacheReadInputTokenCost float64 `json:"cache_read_input_token_cost,omitempty" yaml:"cache_read_input_token_cost"`
	// CacheCreationInputTokenCost is the per-token price of writing prompt
	// tokens into the provider's cache (Anthropic ~1.25x input; most others
	// charge nothing extra).
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost,omitempty" yaml:"cache_creation_input_token_cost"`
}

// cacheReadCost returns the effective per-token cache-read tariff.
func (p ModelPrice) cacheReadCost() float64 {
	if p.CacheReadInputTokenCost > 0 {
		return p.CacheReadInputTokenCost
	}
	return p.InputCostPerToken
}

// cacheCreationCost returns the effective per-token cache-write tariff.
func (p ModelPrice) cacheCreationCost() float64 {
	if p.CacheCreationInputTokenCost > 0 {
		return p.CacheCreationInputTokenCost
	}
	return p.InputCostPerToken
}

// UsageCost describes one request's token breakdown for pricing. It mirrors
// provider.Usage without importing it, keeping the pricing package free of a
// dependency on the provider layer.
type UsageCost struct {
	// PromptTokens is the inclusive input total: uncached + cached + created.
	PromptTokens int
	// CachedPromptTokens is the subset served from the provider cache.
	CachedPromptTokens int
	// CacheCreationTokens is the subset written into the provider cache.
	CacheCreationTokens int
	// CompletionTokens is the generated output.
	CompletionTokens int
}

// uncached returns prompt tokens billed at full input price, never negative.
func (u UsageCost) uncached() int {
	n := u.PromptTokens - u.CachedPromptTokens - u.CacheCreationTokens
	if n < 0 {
		return 0
	}
	return n
}

// Calculator provides model pricing lookups.
type Calculator struct {
	mu     sync.RWMutex
	prices map[string]ModelPrice
	url    string
	logger *slog.Logger
}

// NewCalculator creates a pricing calculator.
// It loads embedded prices first (compiled into the binary), then attempts
// a remote refresh from our own repo if pricingURL is non-empty.
// Config overrides take highest priority.
// Pass pricingURL="" to disable remote refresh (offline/CI-friendly).
func NewCalculator(logger *slog.Logger, pricingURL string, configOverrides map[string]ModelPrice) *Calculator {
	c := &Calculator{
		prices: make(map[string]ModelPrice),
		url:    pricingURL,
		logger: logger,
	}

	// 1. Load embedded defaults (always available, even offline)
	if err := c.loadEmbedded(); err != nil {
		logger.Error("failed to parse embedded model prices", "error", err)
	}
	embeddedCount := len(c.prices)

	// 2. Try remote refresh only if URL is configured
	if c.url != "" {
		if err := c.refresh(); err != nil {
			logger.Warn("remote price refresh failed, using embedded prices", "error", err)
		}
	}

	// 3. Apply per-model overrides from gateway.yaml (highest priority)
	for model, price := range configOverrides {
		c.prices[model] = price
	}

	logger.Info("model pricing ready",
		"embedded", embeddedCount,
		"overrides", len(configOverrides),
		"total", len(c.prices),
		"remote_refresh", c.url != "",
	)

	// Background refresh every 24h (only if URL configured)
	if c.url != "" {
		go c.backgroundRefresh()
	}
	return c
}

// Cost calculates the cost for a request given model name and token counts.
// Returns 0 if model pricing is unknown.
//
// This treats every prompt token as full-price input. Callers that know the
// cache breakdown should use CostUsage instead, which is the only form that
// bills cached reads and cache writes at their real tariffs.
func (c *Calculator) Cost(model string, promptTokens, completionTokens int) float64 {
	return c.CostUsage(model, UsageCost{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
	})
}

// CostUsage calculates the cost for a request from its full token breakdown,
// pricing cached reads and cache writes at their own tariffs.
//
// Categories are disjoint by construction — uncached, cached and created sum
// back to PromptTokens — so no token is billed twice. Returns 0 when the model
// has no pricing entry.
func (c *Calculator) CostUsage(model string, u UsageCost) float64 {
	p, ok := c.GetPrice(model)
	if !ok {
		return 0
	}
	return float64(u.uncached())*p.InputCostPerToken +
		float64(u.CachedPromptTokens)*p.cacheReadCost() +
		float64(u.CacheCreationTokens)*p.cacheCreationCost() +
		float64(u.CompletionTokens)*p.OutputCostPerToken
}

// ModelCount returns how many models have pricing info.
func (c *Calculator) ModelCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.prices)
}

// SetPrice allows adding/updating a model price at runtime.
func (c *Calculator) SetPrice(model string, price ModelPrice) {
	c.mu.Lock()
	c.prices[model] = price
	c.mu.Unlock()
}

// GetPrice returns the per-token pricing for a model.
// Tries exact match, then strips provider prefix. Returns zero prices if unknown.
func (c *Calculator) GetPrice(model string) (ModelPrice, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.prices[model]; ok {
		return p, true
	}
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		if p, ok := c.prices[model[idx+1:]]; ok {
			return p, true
		}
	}
	return ModelPrice{}, false
}

func (c *Calculator) loadEmbedded() error {
	var raw map[string]ModelPrice
	if err := json.Unmarshal(embeddedPrices, &raw); err != nil {
		return fmt.Errorf("pricing: parse embedded: %w", err)
	}
	for name, p := range raw {
		if p.InputCostPerToken > 0 || p.OutputCostPerToken > 0 {
			c.prices[name] = p
		}
	}
	return nil
}

// embeddedOnce guards the lazily-parsed embedded price catalog used by LookupEmbedded.
var (
	embeddedOnce sync.Once
	embeddedMap  map[string]ModelPrice
)

// LookupEmbedded returns the embedded catalog price for a model.
// It tries an exact match first, then strips a provider prefix
// (e.g. "azure/gpt-4o" → "gpt-4o"). Returns false when not found.
func LookupEmbedded(model string) (ModelPrice, bool) {
	embeddedOnce.Do(func() {
		var raw map[string]ModelPrice
		_ = json.Unmarshal(embeddedPrices, &raw)
		embeddedMap = raw
	})
	if p, ok := embeddedMap[model]; ok {
		return p, true
	}
	// Try stripping a provider prefix
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		if p, ok := embeddedMap[model[idx+1:]]; ok {
			return p, true
		}
	}
	return ModelPrice{}, false
}

func (c *Calculator) refresh() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(c.url)
	if err != nil {
		return fmt.Errorf("pricing: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pricing: fetch returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("pricing: read body: %w", err)
	}

	var raw map[string]ModelPrice
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("pricing: parse JSON: %w", err)
	}

	c.mu.Lock()
	for name, p := range raw {
		if p.InputCostPerToken > 0 || p.OutputCostPerToken > 0 {
			c.prices[name] = p
		}
	}
	c.mu.Unlock()

	c.logger.Info("model prices refreshed from remote", "models_updated", len(raw))
	return nil
}

func (c *Calculator) backgroundRefresh() {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := c.refresh(); err != nil {
			c.logger.Warn("pricing refresh failed, will retry in 24h", "error", err)
		}
	}
}
