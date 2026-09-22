package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
)

// RateLimiter implements a per-key token bucket rate limiter.
// Each virtual key with a RateLimit > 0 gets its own bucket.
// The limit is expressed in requests per minute (RPM).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

// NewRateLimiter creates a new per-key rate limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
	}
}

// Middleware returns the HTTP middleware that enforces per-key rate limits.
// Must be placed AFTER auth middleware so that key info is available in context.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyInfo := auth.KeyInfoFromContext(r.Context())
		if keyInfo == nil || keyInfo.RateLimit <= 0 {
			// No key info or no rate limit configured — pass through
			next.ServeHTTP(w, r)
			return
		}

		if !rl.allow(keyInfo.KeyHash, keyInfo.RateLimit) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded for this API key","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// allow checks if the key has available tokens. Returns true if the request is allowed.
func (rl *RateLimiter) allow(keyHash string, rpmLimit int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	bucket, exists := rl.buckets[keyHash]
	if !exists {
		maxTokens := float64(rpmLimit)
		bucket = &tokenBucket{
			tokens:     maxTokens - 1, // consume one token for this request
			maxTokens:  maxTokens,
			refillRate: maxTokens / 60.0, // RPM → tokens per second
			lastRefill: now,
		}
		rl.buckets[keyHash] = bucket
		return true
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(bucket.lastRefill).Seconds()
	bucket.tokens += elapsed * bucket.refillRate
	if bucket.tokens > bucket.maxTokens {
		bucket.tokens = bucket.maxTokens
	}
	bucket.lastRefill = now

	// Update limit if it changed (key was updated)
	if bucket.maxTokens != float64(rpmLimit) {
		bucket.maxTokens = float64(rpmLimit)
		bucket.refillRate = float64(rpmLimit) / 60.0
	}

	if bucket.tokens < 1 {
		return false
	}

	bucket.tokens--
	return true
}
