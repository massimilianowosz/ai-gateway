package middleware

import (
	"net/http"
	"sync/atomic"
)

// Semaphore implements adaptive concurrency limiting with backpressure.
// Unlike a simple rate limiter, it limits IN-FLIGHT requests (not req/s),
// which is what matters for LLM proxying where requests take 1-60s.
//
// When the limit is reached, new requests get 503 Service Unavailable
// immediately instead of queuing (fail-fast = better UX for LLM callers).
type Semaphore struct {
	maxConcurrent int64
	active        atomic.Int64
}

// NewSemaphore creates a concurrency limiter.
// maxConcurrent is the maximum number of in-flight requests.
// Recommended: 5000-10000 for a well-provisioned gateway.
func NewSemaphore(maxConcurrent int64) *Semaphore {
	return &Semaphore{maxConcurrent: maxConcurrent}
}

// Middleware returns the HTTP middleware that enforces the concurrency limit.
func (s *Semaphore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := s.active.Add(1)
		defer s.active.Add(-1)

		if current > s.maxConcurrent {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"gateway at capacity, please retry","type":"capacity_error"}}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Active returns the current number of in-flight requests.
func (s *Semaphore) Active() int64 {
	return s.active.Load()
}

// MaxConcurrent returns the configured limit.
func (s *Semaphore) MaxConcurrent() int64 {
	return s.maxConcurrent
}
