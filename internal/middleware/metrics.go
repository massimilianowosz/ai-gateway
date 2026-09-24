package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects request-level metrics for Prometheus exposition.
type Metrics struct {
	mu       sync.RWMutex
	counters map[string]*metricCounter // "method:path:status" → counter
	// Histograms are approximated via pre-defined buckets
	latencyBuckets []float64
	latencies      map[string]*histogram // "method:path" → histogram

	totalRequests  atomic.Int64
	activeRequests atomic.Int64
	totalTokensIn  atomic.Int64
	totalTokensOut atomic.Int64

	// Context-optimization counters; see optimization_metrics.go.
	opt optimizationMetrics
}

type metricCounter struct {
	count atomic.Int64
}

type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []int64
	sum     float64
	count   int64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{
		buckets: buckets,
		counts:  make([]int64, len(buckets)+1), // +1 for +Inf
	}
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	h.sum += v
	h.count++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.counts[len(h.buckets)]++ // +Inf always
	h.mu.Unlock()
}

// NewMetrics creates a new metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{
		counters: make(map[string]*metricCounter),
		// Every counter map is created here and never reassigned, which is what
		// lets readers copy the map header without holding the lock.
		opt: optimizationMetrics{
			guardDecisions: make(map[string]*atomic.Int64),
			liveBlocks:     make(map[string]*atomic.Int64),
			liveSkips:      make(map[string]*atomic.Int64),
			steerVerbosity: make(map[string]*atomic.Int64),
			traceEvents:    make(map[string]*atomic.Int64),
			traceFindings:  make(map[string]*atomic.Int64),
		},
		latencyBuckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		latencies:      make(map[string]*histogram),
	}
}

// Middleware records request metrics.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.activeRequests.Add(1)
		m.totalRequests.Add(1)
		start := time.Now()

		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		m.activeRequests.Add(-1)
		duration := time.Since(start).Seconds()

		// Record counter
		key := r.Method + ":" + r.URL.Path + ":" + strconv.Itoa(wrapped.status)
		m.getCounter(key).count.Add(1)

		// Record latency
		latKey := r.Method + ":" + r.URL.Path
		m.getHistogram(latKey).observe(duration)
	})
}

// RecordTokens records token usage for metrics.
func (m *Metrics) RecordTokens(promptTokens, completionTokens int) {
	m.totalTokensIn.Add(int64(promptTokens))
	m.totalTokensOut.Add(int64(completionTokens))
}

// Handler returns an HTTP handler that serves metrics in Prometheus text format.
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		// Gateway metrics
		fmt.Fprintf(w, "# HELP ubiquum_requests_total Total number of requests.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_requests_total counter\n")
		fmt.Fprintf(w, "ubiquum_requests_total %d\n", m.totalRequests.Load())

		fmt.Fprintf(w, "# HELP ubiquum_requests_active Current in-flight requests.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_requests_active gauge\n")
		fmt.Fprintf(w, "ubiquum_requests_active %d\n", m.activeRequests.Load())

		fmt.Fprintf(w, "# HELP ubiquum_tokens_prompt_total Total prompt tokens processed.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_tokens_prompt_total counter\n")
		fmt.Fprintf(w, "ubiquum_tokens_prompt_total %d\n", m.totalTokensIn.Load())

		fmt.Fprintf(w, "# HELP ubiquum_tokens_completion_total Total completion tokens generated.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_tokens_completion_total counter\n")
		fmt.Fprintf(w, "ubiquum_tokens_completion_total %d\n", m.totalTokensOut.Load())

		// Per-path request counters
		fmt.Fprintf(w, "# HELP ubiquum_http_requests_total HTTP requests by method, path, and status.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_http_requests_total counter\n")
		m.mu.RLock()
		for key, c := range m.counters {
			// Parse "METHOD:PATH:STATUS"
			parts := splitMetricKey(key)
			if len(parts) == 3 {
				fmt.Fprintf(w, "ubiquum_http_requests_total{method=%q,path=%q,status=%q} %d\n",
					parts[0], parts[1], parts[2], c.count.Load())
			}
		}
		m.mu.RUnlock()

		// Latency histograms
		fmt.Fprintf(w, "# HELP ubiquum_request_duration_seconds Request latency histogram.\n")
		fmt.Fprintf(w, "# TYPE ubiquum_request_duration_seconds histogram\n")
		m.mu.RLock()
		for key, h := range m.latencies {
			parts := splitMetricKey(key)
			if len(parts) < 2 {
				continue
			}
			labels := fmt.Sprintf("method=%q,path=%q", parts[0], parts[1])
			h.mu.Lock()
			cumulative := int64(0)
			for i, b := range h.buckets {
				cumulative += h.counts[i]
				fmt.Fprintf(w, "ubiquum_request_duration_seconds_bucket{%s,le=\"%g\"} %d\n", labels, b, cumulative)
			}
			cumulative += h.counts[len(h.buckets)]
			fmt.Fprintf(w, "ubiquum_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, cumulative)
			fmt.Fprintf(w, "ubiquum_request_duration_seconds_sum{%s} %f\n", labels, h.sum)
			fmt.Fprintf(w, "ubiquum_request_duration_seconds_count{%s} %d\n", labels, h.count)
			h.mu.Unlock()
		}
		m.mu.RUnlock()

		m.writeOptimizationMetrics(w)
	}
}

func (m *Metrics) getCounter(key string) *metricCounter {
	m.mu.RLock()
	c, ok := m.counters[key]
	m.mu.RUnlock()
	if ok {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok = m.counters[key]; ok {
		return c
	}
	c = &metricCounter{}
	m.counters[key] = c
	return c
}

func (m *Metrics) getHistogram(key string) *histogram {
	m.mu.RLock()
	h, ok := m.latencies[key]
	m.mu.RUnlock()
	if ok {
		return h
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok = m.latencies[key]; ok {
		return h
	}
	h = newHistogram(m.latencyBuckets)
	m.latencies[key] = h
	return h
}

func splitMetricKey(key string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			parts = append(parts, key[start:i])
			start = i + 1
		}
	}
	parts = append(parts, key[start:])
	return parts
}
