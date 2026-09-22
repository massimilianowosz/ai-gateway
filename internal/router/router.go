package router

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// Routing strategy constants.
const (
	StrategyShuffle    = "shuffle"
	StrategyRoundRobin = "round-robin"
	StrategyLatency    = "latency"
)

// latencyTracker maintains an exponential moving average of response latency
// per deployment. Used by the latency routing strategy.
type latencyTracker struct {
	mu   sync.RWMutex
	avgs map[string]float64 // deployment ID → EMA latency (seconds)
}

func newLatencyTracker() *latencyTracker {
	return &latencyTracker{avgs: make(map[string]float64)}
}

// record updates the EMA latency for a deployment.
func (t *latencyTracker) record(id string, d time.Duration) {
	const alpha = 0.2
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.avgs[id]; ok {
		t.avgs[id] = alpha*d.Seconds() + (1-alpha)*cur
	} else {
		t.avgs[id] = d.Seconds()
	}
}

// get returns the EMA latency for a deployment (0 if unknown → explored first).
func (t *latencyTracker) get(id string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.avgs[id]
}

// Router selects deployments and handles retries with circuit breaking.
type Router struct {
	registry       *provider.Registry
	strategy       string
	maxRetries     int
	retryDelay     time.Duration
	circuitBreaker *CircuitBreaker
	logger         *slog.Logger

	rrCounter atomic.Uint64   // used by round-robin strategy
	latency   *latencyTracker // used by latency strategy
}

// New creates a new Router with the given configuration.
func New(registry *provider.Registry, cfg config.RouterConfig, logger *slog.Logger) *Router {
	return &Router{
		registry:   registry,
		strategy:   cfg.Strategy,
		maxRetries: cfg.Retries,
		retryDelay: cfg.RetryDelay,
		circuitBreaker: NewCircuitBreaker(
			cfg.CircuitBreaker.Threshold,
			cfg.CircuitBreaker.Recovery,
		),
		logger:  logger,
		latency: newLatencyTracker(),
	}
}

// RouteResult contains the result of routing a request.
type RouteResult struct {
	Deployment        *provider.Deployment
	Attempts          int
	FailedDeployments []string // provider names that failed before the successful one
}

// Route selects a healthy deployment for the given model, retrying on failures.
// The caller provides a tryFunc that attempts the request on a given deployment.
// Returns the successful deployment and attempt count, or the last error.
func (r *Router) Route(ctx context.Context, model string, tryFunc func(dep *provider.Deployment) error) (*RouteResult, error) {
	return r.RouteEligible(ctx, model, nil, tryFunc)
}

// RouteEligible removes unauthorized deployments before ordering, circuit
// breaking and retry. This guarantees that fallback cannot reintroduce a
// provider excluded by governance.
func (r *Router) RouteEligible(ctx context.Context, model string, eligible func(*provider.Deployment) bool, tryFunc func(dep *provider.Deployment) error) (*RouteResult, error) {
	deployments, err := r.registry.GetDeployments(model)
	if err != nil {
		return nil, err
	}

	if eligible != nil {
		filtered := make([]*provider.Deployment, 0, len(deployments))
		for _, dep := range deployments {
			if eligible(dep) {
				filtered = append(filtered, dep)
				continue
			}
			// GW-01: a candidate excluded here must be visible somewhere,
			// not just silently absent from the ordered list — the same
			// reasoning circuit-breaker exclusions already get a log line
			// for. Deployment ID and provider name only: nothing about the
			// caller's own denied_providers/model_allowlist configuration
			// (reserved to the tenant) reaches this log.
			r.logger.Debug("deployment excluded from routing by governance filter",
				"deployment", dep.ID,
				"provider", dep.ProviderName,
				"model", model,
			)
		}
		deployments = filtered
	}
	if len(deployments) == 0 {
		return nil, fmt.Errorf("no authorized deployments available for model %q", model)
	}

	ordered := r.orderDeployments(deployments)

	var lastErr error
	attempts := 0
	var failedProviders []string

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		// Pick a deployment, cycling through the ordered list
		dep := ordered[attempt%len(ordered)]

		// Check circuit breaker
		if r.circuitBreaker.IsOpen(dep.ID) {
			r.logger.Debug("circuit breaker open, skipping deployment",
				"deployment", dep.ID,
				"model", model,
			)
			// Try next deployment without counting as an attempt
			if attempt < r.maxRetries {
				continue
			}
			// All retries exhausted, try anyway (half-open)
		}

		attempts++
		start := time.Now()
		err := tryFunc(dep)
		elapsed := time.Since(start)

		if err == nil {
			r.circuitBreaker.RecordSuccess(dep.ID)
			if r.strategy == StrategyLatency {
				r.latency.record(dep.ID, elapsed)
			}
			return &RouteResult{
				Deployment:        dep,
				Attempts:          attempts,
				FailedDeployments: failedProviders,
			}, nil
		}

		lastErr = err
		// A 400 says the request is wrong, not that the deployment is
		// unhealthy. Counting it opened the circuit of every deployment for a
		// model as soon as a client repeatedly sent something invalid — a bad
		// tool schema, an over-long context — and pushed unrelated healthy
		// traffic away. It is still retried, because providers validate
		// differently, but it no longer damages a deployment's health.
		if indicatesDeploymentFailure(err) {
			r.circuitBreaker.RecordFailure(dep.ID)
		}
		failedProviders = append(failedProviders, dep.ProviderName)

		r.logger.Warn("request attempt failed",
			"deployment", dep.ID,
			"model", model,
			"attempt", attempt+1,
			"max_retries", r.maxRetries+1,
			"error", err,
		)

		// Don't wait after the last attempt
		if attempt < r.maxRetries {
			// Check if the error is retryable
			if !isRetryable(err) {
				return nil, err
			}

			// Wait before retry (with exponential backoff)
			delay := r.retryDelay * time.Duration(1<<uint(attempt))
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	return nil, fmt.Errorf("all %d attempts failed for model %q: %w", attempts, model, lastErr)
}

// orderDeployments returns deployments ordered according to the active strategy.
func (r *Router) orderDeployments(deployments []*provider.Deployment) []*provider.Deployment {
	ordered := make([]*provider.Deployment, len(deployments))
	copy(ordered, deployments)

	switch r.strategy {
	case StrategyRoundRobin:
		n := len(ordered)
		// Advance the global counter; start cycling from that position.
		start := int(r.rrCounter.Add(1)-1) % n
		result := make([]*provider.Deployment, n)
		for i := 0; i < n; i++ {
			result[i] = ordered[(start+i)%n]
		}
		return result

	case StrategyLatency:
		// Sort ascending by EMA latency; unknown (0) deployments come first
		// so they are explored early and can build their latency estimate.
		sort.SliceStable(ordered, func(i, j int) bool {
			li := r.latency.get(ordered[i].ID)
			lj := r.latency.get(ordered[j].ID)
			// Prefer unexplored (0) over any measured latency
			if li == 0 && lj != 0 {
				return true
			}
			if lj == 0 && li != 0 {
				return false
			}
			return li < lj
		})

	default: // StrategyShuffle (default)
		rand.Shuffle(len(ordered), func(i, j int) {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		})
	}

	return ordered
}

// indicatesDeploymentFailure reports whether an error says something about the
// deployment's health, as opposed to the validity of the request. Only the
// former belongs in the circuit breaker.
func indicatesDeploymentFailure(err error) bool {
	if ue, ok := err.(*provider.UpstreamError); ok && ue.StatusCode == 400 {
		return false
	}
	return true
}

// isRetryable determines if an error should be retried.
func isRetryable(err error) bool {
	if ue, ok := err.(*provider.UpstreamError); ok {
		switch ue.StatusCode {
		case 429: // Rate limited
			return true
		case 400: // Bad request — retry because providers have different validation
			return true
		case 500, 502, 503, 504: // Server errors
			return true
		case 408: // Request timeout
			return true
		default:
			// Client errors (4xx) are not retryable
			return false
		}
	}
	// Network errors are retryable
	return true
}

// --- Circuit Breaker ---

// CircuitBreaker tracks failures per deployment and opens the circuit
// when the threshold is exceeded.
type CircuitBreaker struct {
	mu        sync.RWMutex
	states    map[string]*circuitState
	threshold int
	recovery  time.Duration
}

type circuitState struct {
	failures  int
	lastFail  time.Time
	openUntil time.Time
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(threshold int, recovery time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		states:    make(map[string]*circuitState),
		threshold: threshold,
		recovery:  recovery,
	}
}

// IsOpen returns true if the circuit is open for the given deployment.
func (cb *CircuitBreaker) IsOpen(deploymentID string) bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	state, ok := cb.states[deploymentID]
	if !ok {
		return false
	}

	if time.Now().Before(state.openUntil) {
		return true
	}

	return false
}

// RecordSuccess records a successful request, resetting the failure count.
func (cb *CircuitBreaker) RecordSuccess(deploymentID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	delete(cb.states, deploymentID)
}

// RecordFailure records a failed request. Opens the circuit if threshold is exceeded.
func (cb *CircuitBreaker) RecordFailure(deploymentID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state, ok := cb.states[deploymentID]
	if !ok {
		state = &circuitState{}
		cb.states[deploymentID] = state
	}

	state.failures++
	state.lastFail = time.Now()

	if state.failures >= cb.threshold {
		state.openUntil = time.Now().Add(cb.recovery)
	}
}

// Reset clears all circuit breaker state (useful for testing).
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.states = make(map[string]*circuitState)
}
