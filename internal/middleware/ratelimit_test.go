package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func TestRateLimiter_AllowsWithinLimit(t *testing.T) {
	rl := NewRateLimiter()

	// 10 RPM = 10 requests per 60 seconds
	for i := 0; i < 10; i++ {
		assert.True(t, rl.allow("key-hash-1", 10), "request %d should be allowed", i)
	}
}

func TestRateLimiter_BlocksOverLimit(t *testing.T) {
	rl := NewRateLimiter()

	// Exhaust 5 RPM budget
	for i := 0; i < 5; i++ {
		assert.True(t, rl.allow("key-hash-2", 5))
	}

	// 6th request should be blocked
	assert.False(t, rl.allow("key-hash-2", 5))
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	rl := NewRateLimiter()

	// Exhaust budget
	for i := 0; i < 5; i++ {
		rl.allow("key-hash-3", 5)
	}
	assert.False(t, rl.allow("key-hash-3", 5))

	// Simulate time passing (manipulate lastRefill)
	rl.mu.Lock()
	bucket := rl.buckets["key-hash-3"]
	bucket.lastRefill = bucket.lastRefill.Add(-13 * time.Second) // 13s * (5/60) ≈ 1.08 tokens refilled
	rl.mu.Unlock()

	assert.True(t, rl.allow("key-hash-3", 5), "should be allowed after refill")
}

func TestRateLimiter_IndependentKeys(t *testing.T) {
	rl := NewRateLimiter()

	// Exhaust key A
	for i := 0; i < 3; i++ {
		rl.allow("key-a", 3)
	}
	assert.False(t, rl.allow("key-a", 3))

	// Key B should still work
	assert.True(t, rl.allow("key-b", 3))
}

// --- HTTP middleware behavior ---

func rateLimitedNext(t *testing.T, called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func makeRateLimitReq(keyInfo *store.APIKey) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if keyInfo != nil {
		req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), keyInfo))
	}
	return req
}

func TestRateLimiter_Middleware_NoKeyInfo_PassesThrough(t *testing.T) {
	rl := NewRateLimiter()
	called := false
	handler := rl.Middleware(rateLimitedNext(t, &called))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, makeRateLimitReq(nil))

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRateLimiter_Middleware_ZeroRateLimit_PassesThrough(t *testing.T) {
	rl := NewRateLimiter()
	called := false
	handler := rl.Middleware(rateLimitedNext(t, &called))

	// RateLimit == 0 means unlimited
	keyInfo := &store.APIKey{KeyHash: "hash-zero", RateLimit: 0}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, makeRateLimitReq(keyInfo))

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRateLimiter_Middleware_WithinLimit_Allows(t *testing.T) {
	rl := NewRateLimiter()
	keyInfo := &store.APIKey{KeyHash: "hash-ok", RateLimit: 10}

	for i := 0; i < 10; i++ {
		called := false
		handler := rl.Middleware(rateLimitedNext(t, &called))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, makeRateLimitReq(keyInfo))
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should pass", i)
		assert.True(t, called)
	}
}

func TestRateLimiter_Middleware_ExceededLimit_Returns429(t *testing.T) {
	rl := NewRateLimiter()
	keyInfo := &store.APIKey{KeyHash: "hash-limited", RateLimit: 3}

	// Exhaust the bucket
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, makeRateLimitReq(keyInfo))
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	// Next request must be blocked
	called := false
	rec := httptest.NewRecorder()
	rl.Middleware(rateLimitedNext(t, &called)).ServeHTTP(rec, makeRateLimitReq(keyInfo))

	assert.False(t, called, "handler must not be invoked when rate-limited")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), "rate_limit_exceeded")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestRateLimiter_Middleware_DifferentKeys_Independent(t *testing.T) {
	rl := NewRateLimiter()

	keyA := &store.APIKey{KeyHash: "hash-a", RateLimit: 1}
	keyB := &store.APIKey{KeyHash: "hash-b", RateLimit: 1}

	// Exhaust key A
	recA1 := httptest.NewRecorder()
	rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(recA1, makeRateLimitReq(keyA))
	assert.Equal(t, http.StatusOK, recA1.Code)

	recA2 := httptest.NewRecorder()
	rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(recA2, makeRateLimitReq(keyA))
	assert.Equal(t, http.StatusTooManyRequests, recA2.Code, "key A should be exhausted")

	// Key B must still work
	recB := httptest.NewRecorder()
	rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(recB, makeRateLimitReq(keyB))
	assert.Equal(t, http.StatusOK, recB.Code, "key B should not be affected by key A exhaustion")
}
