package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	timeout = 2 * time.Second
	tick    = 10 * time.Millisecond
)

func alwaysOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestSemaphore_AllowsWithinLimit(t *testing.T) {
	s := NewSemaphore(5)
	h := s.Middleware(alwaysOKHandler())

	for range 5 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.Equal(t, http.StatusOK, rr.Code)
	}
}

func TestSemaphore_BlocksAtCapacity(t *testing.T) {
	// Semaphore with limit 2; use a blocking handler so we can stack requests.
	gate := make(chan struct{})
	blocker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate // block until released
		w.WriteHeader(http.StatusOK)
	})

	s := NewSemaphore(2)
	h := s.Middleware(blocker)

	var wg sync.WaitGroup
	// Fill all slots
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}

	// Wait until both goroutines are actually inside the handler
	require.Eventually(t, func() bool { return s.Active() == 2 }, timeout, tick)

	// This extra request should be rejected
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"))
	assert.Contains(t, rr.Body.String(), "capacity_error")

	// Unblock the held goroutines
	close(gate)
	wg.Wait()
}

func TestSemaphore_ActiveCount(t *testing.T) {
	gate := make(chan struct{})
	blocker := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	})

	s := NewSemaphore(10)
	h := s.Middleware(blocker)

	assert.Equal(t, int64(0), s.Active())

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}

	require.Eventually(t, func() bool { return s.Active() == 3 }, timeout, tick)
	close(gate)
	wg.Wait()

	require.Eventually(t, func() bool { return s.Active() == 0 }, timeout, tick)
}

func TestSemaphore_MaxConcurrent(t *testing.T) {
	s := NewSemaphore(42)
	assert.Equal(t, int64(42), s.MaxConcurrent())
}
