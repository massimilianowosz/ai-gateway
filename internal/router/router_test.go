package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// mockProvider for testing
type mockProvider struct {
	name string
}

func (m *mockProvider) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, nil
}
func (m *mockProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (m *mockProvider) Name() string { return m.name }

type mockFactory struct {
	p provider.Provider
}

func (f *mockFactory) Create(_ config.ModelConfig) (provider.Provider, error) {
	return f.p, nil
}

func testRouter(t *testing.T, models []config.ModelConfig) *Router {
	t.Helper()
	mock := &mockProvider{name: "mock"}
	registry, err := provider.NewRegistry(models, &mockFactory{p: mock})
	require.NoError(t, err)

	cfg := config.RouterConfig{
		Strategy:   "shuffle",
		Retries:    2,
		RetryDelay: 10 * time.Millisecond,
		CircuitBreaker: config.CircuitBreaker{
			Threshold: 3,
			Recovery:  100 * time.Millisecond,
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(registry, cfg, logger)
}

func testRouterWithConfig(t *testing.T, models []config.ModelConfig, strategy string, retries int, retryDelay time.Duration, threshold int, recovery time.Duration) *Router {
	t.Helper()
	mock := &mockProvider{name: "mock"}
	registry, err := provider.NewRegistry(models, &mockFactory{p: mock})
	require.NoError(t, err)

	cfg := config.RouterConfig{
		Strategy:   strategy,
		Retries:    retries,
		RetryDelay: retryDelay,
		CircuitBreaker: config.CircuitBreaker{
			Threshold: threshold,
			Recovery:  recovery,
		},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(registry, cfg, logger)
}

func TestRouter_SuccessOnFirstAttempt(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	attempts := 0
	result, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		attempts++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, attempts)
	assert.Equal(t, 1, result.Attempts)
	assert.Equal(t, "gpt-4", result.Deployment.ModelName)
}

func TestRouter_RouteEligibleFiltersBeforeRetryAndFallback(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "mixed", Provider: "denied", ProviderModel: "denied-model"},
		{Name: "mixed", Provider: "allowed", ProviderModel: "allowed-model"},
	})

	for i := 0; i < 20; i++ {
		result, err := rt.RouteEligible(context.Background(), "mixed", func(dep *provider.Deployment) bool {
			return dep.ProviderName == "allowed"
		}, func(dep *provider.Deployment) error {
			assert.Equal(t, "allowed", dep.ProviderName)
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, "allowed", result.Deployment.ProviderName)
	}

	_, err := rt.RouteEligible(context.Background(), "mixed", func(*provider.Deployment) bool {
		return false
	}, func(*provider.Deployment) error {
		t.Fatal("an ineligible deployment must never be attempted")
		return nil
	})
	assert.ErrorContains(t, err, "no authorized deployments")
}

func TestRouter_RetryOnFailure(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	var attempts int32
	result, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return &provider.UpstreamError{StatusCode: 500, Message: "server error"}
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	assert.Equal(t, 3, result.Attempts)
}

func TestRouter_AllRetriesExhausted(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	_, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		return &provider.UpstreamError{StatusCode: 500, Message: "always fails"}
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all")
	assert.Contains(t, err.Error(), "attempts failed")
}

func TestRouter_NonRetryableError(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	attempts := 0
	_, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		attempts++
		return &provider.UpstreamError{StatusCode: 403, Message: "forbidden"}
	})

	require.Error(t, err)
	assert.Equal(t, 1, attempts) // Should not retry 403 errors
}

func TestRouter_ModelNotFound(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	_, err := rt.Route(context.Background(), "nonexistent", func(dep *provider.Deployment) error {
		return nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRouter_ContextCancelled(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := rt.Route(ctx, "gpt-4", func(dep *provider.Deployment) error {
		attempts++
		if attempts == 1 {
			cancel() // Cancel after first failure
			return &provider.UpstreamError{StatusCode: 500, Message: "fail"}
		}
		return nil
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestRouter_MultipleDeployments(t *testing.T) {
	rt := testRouter(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v2"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v3"},
	})

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		result, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
			return nil
		})
		require.NoError(t, err)
		seen[result.Deployment.ProviderModel] = true
	}

	// With 50 attempts across 3 deployments, should hit at least 2
	assert.True(t, len(seen) >= 2, "expected requests to be distributed across deployments, got: %v", seen)
}

// --- Circuit Breaker Tests ---

func TestCircuitBreaker_ClosedByDefault(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)
	assert.False(t, cb.IsOpen("dep-1"))
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)

	cb.RecordFailure("dep-1")
	assert.False(t, cb.IsOpen("dep-1"))
	cb.RecordFailure("dep-1")
	assert.False(t, cb.IsOpen("dep-1"))
	cb.RecordFailure("dep-1")
	assert.True(t, cb.IsOpen("dep-1"))
}

func TestCircuitBreaker_ResetsOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Minute)

	cb.RecordFailure("dep-1")
	cb.RecordFailure("dep-1")
	cb.RecordSuccess("dep-1")

	// After success, failure count resets
	cb.RecordFailure("dep-1")
	cb.RecordFailure("dep-1")
	assert.False(t, cb.IsOpen("dep-1"))
}

func TestCircuitBreaker_RecoversAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker(2, 50*time.Millisecond)

	cb.RecordFailure("dep-1")
	cb.RecordFailure("dep-1")
	assert.True(t, cb.IsOpen("dep-1"))

	time.Sleep(60 * time.Millisecond)
	assert.False(t, cb.IsOpen("dep-1"))
}

func TestCircuitBreaker_IndependentPerDeployment(t *testing.T) {
	cb := NewCircuitBreaker(2, time.Minute)

	cb.RecordFailure("dep-1")
	cb.RecordFailure("dep-1")
	assert.True(t, cb.IsOpen("dep-1"))
	assert.False(t, cb.IsOpen("dep-2"))
}

// --- isRetryable Tests ---

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		err       error
		retryable bool
	}{
		{&provider.UpstreamError{StatusCode: 429}, true},
		{&provider.UpstreamError{StatusCode: 500}, true},
		{&provider.UpstreamError{StatusCode: 502}, true},
		{&provider.UpstreamError{StatusCode: 503}, true},
		{&provider.UpstreamError{StatusCode: 504}, true},
		{&provider.UpstreamError{StatusCode: 408}, true},
		{&provider.UpstreamError{StatusCode: 400}, true},
		{&provider.UpstreamError{StatusCode: 401}, false},
		{&provider.UpstreamError{StatusCode: 403}, false},
		{&provider.UpstreamError{StatusCode: 404}, false},
		{&provider.UpstreamError{StatusCode: 422}, false},
		{fmt.Errorf("network error"), true}, // Non-upstream errors are retryable
	}

	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			assert.Equal(t, tt.retryable, isRetryable(tt.err))
		})
	}
}

// --- All-deployments-down / circuit-open Tests ---

func TestRouter_SingleDeployment_CircuitOpen_StillTriesOnFinalAttempt(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	}, "shuffle", 2, time.Millisecond, 1, time.Minute)

	deployments, err := rt.registry.GetDeployments("gpt-4")
	require.NoError(t, err)
	require.Len(t, deployments, 1)

	// Force the circuit open before routing (threshold=1).
	rt.circuitBreaker.RecordFailure(deployments[0].ID)
	require.True(t, rt.circuitBreaker.IsOpen(deployments[0].ID))

	var attempts int32
	result, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		atomic.AddInt32(&attempts, 1)
		return nil
	})

	// Even with the circuit open, the router must still try the deployment
	// on the final attempt (half-open probe) rather than failing outright.
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts))
	assert.Equal(t, 1, result.Attempts)
}

func TestRouter_AllDeploymentsCircuitOpen_HalfOpenProbeFails(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v2"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v3"},
	}, "shuffle", 2, time.Millisecond, 1, time.Minute)

	deployments, err := rt.registry.GetDeployments("gpt-4")
	require.NoError(t, err)
	for _, d := range deployments {
		rt.circuitBreaker.RecordFailure(d.ID)
		require.True(t, rt.circuitBreaker.IsOpen(d.ID))
	}

	_, err = rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		return &provider.UpstreamError{StatusCode: 500, Message: "still down"}
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "attempts failed")
}

// --- Cancellation-during-backoff Tests ---

func TestRouter_ContextCancelledDuringBackoffWait_ReturnsPromptly(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	}, "shuffle", 5, 5*time.Second, 100, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := rt.Route(ctx, "gpt-4", func(dep *provider.Deployment) error {
		return &provider.UpstreamError{StatusCode: 500, Message: "fail"}
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	// The configured backoff (5s) must be interrupted by context cancellation
	// almost immediately, not waited out in full.
	assert.Less(t, elapsed, 1*time.Second, "context cancellation should interrupt the backoff wait promptly")
}

// --- Exponential backoff Tests ---

func TestRouter_ExponentialBackoff_DelayGrowsBetweenAttempts(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
	}, "shuffle", 3, 20*time.Millisecond, 100, time.Minute)

	var timestamps []time.Time
	_, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		timestamps = append(timestamps, time.Now())
		return &provider.UpstreamError{StatusCode: 500, Message: "fail"}
	})
	require.Error(t, err)
	require.Len(t, timestamps, 4) // attempts 0..3

	gap1 := timestamps[1].Sub(timestamps[0])
	gap2 := timestamps[2].Sub(timestamps[1])
	gap3 := timestamps[3].Sub(timestamps[2])

	// Expected nominal gaps: ~20ms, ~40ms, ~80ms (base * 2^attempt).
	assert.Greater(t, gap2, gap1, "second backoff gap should be larger than the first (exponential growth)")
	assert.Greater(t, gap3, gap2, "third backoff gap should be larger than the second (exponential growth)")
}

// --- Concurrency stress Tests ---

func TestRouter_ConcurrentRouteCalls_NoRaceAndConsistentResults(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v1"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v2"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "gpt-4-v3"},
	}, "round-robin", 2, time.Millisecond, 3, 50*time.Millisecond)

	var wg sync.WaitGroup
	var successes int32
	var failures int32

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
				if i%7 == 0 {
					return &provider.UpstreamError{StatusCode: 500, Message: "flaky"}
				}
				return nil
			})
			if err != nil {
				atomic.AddInt32(&failures, 1)
			} else {
				atomic.AddInt32(&successes, 1)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(100), successes+failures)
}

// --- Strategy-specific ordering Tests ---

func TestRouter_RoundRobinStrategy_CyclesThroughDeployments(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "v1"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "v2"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "v3"},
	}, "round-robin", 0, time.Millisecond, 100, time.Minute)

	var picked []string
	for i := 0; i < 6; i++ {
		_, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
			picked = append(picked, dep.ProviderModel)
			return nil
		})
		require.NoError(t, err)
	}

	// Each deployment should appear roughly evenly across the 6 calls.
	counts := map[string]int{}
	for _, p := range picked {
		counts[p]++
	}
	assert.Len(t, counts, 3)
	for _, c := range counts {
		assert.Equal(t, 2, c)
	}
}

func TestRouter_LatencyStrategy_PrefersFasterDeployment(t *testing.T) {
	rt := testRouterWithConfig(t, []config.ModelConfig{
		{Name: "gpt-4", Provider: "mock", ProviderModel: "slow"},
		{Name: "gpt-4", Provider: "mock", ProviderModel: "fast"},
	}, "latency", 0, time.Millisecond, 100, time.Minute)

	deployments, err := rt.registry.GetDeployments("gpt-4")
	require.NoError(t, err)

	var slowID, fastID string
	for _, d := range deployments {
		if d.ProviderModel == "slow" {
			slowID = d.ID
		} else {
			fastID = d.ID
		}
	}

	// Seed latency history: slow deployment has high EMA, fast has low EMA.
	rt.latency.record(slowID, 2*time.Second)
	rt.latency.record(fastID, 10*time.Millisecond)

	result, err := rt.Route(context.Background(), "gpt-4", func(dep *provider.Deployment) error {
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "fast", result.Deployment.ProviderModel)
}
