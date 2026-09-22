package perf

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// NewHighPerfTransport creates an http.Transport optimized for high-throughput
// LLM gateway proxying. Supports thousands of concurrent connections per host.
//
// Key differences vs default Go transport:
//   - MaxIdleConnsPerHost: 1024 (vs 2 default, vs 20 in naive implementations)
//   - MaxConnsPerHost: 0 (unlimited) — let the OS handle it
//   - TLS session resumption enabled (reuses handshakes)
//   - TCP keepalive aggressive (detect dead connections fast)
//   - Dialer timeout 10s (fail fast on unreachable hosts)
func NewHighPerfTransport() *http.Transport {
	return &http.Transport{
		// Connection pooling: sized for 10k+ concurrent requests
		MaxIdleConns:        2048,
		MaxIdleConnsPerHost: 1024,
		MaxConnsPerHost:     0, // unlimited — backpressure handled at gateway level

		// Keep connections alive
		IdleConnTimeout: 120 * time.Second,

		// Fast connection establishment
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		// TLS performance
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			// Session resumption is enabled by default in Go
			MinVersion: tls.VersionTLS12,
		},

		// HTTP/2: enabled by default when using TLS
		ForceAttemptHTTP2: true,

		// Response handling
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second, // 5 min for slow edge/local models (Ollama on CPU)

		// Write buffer
		WriteBufferSize: 64 * 1024, // 64KB write buffer
		ReadBufferSize:  64 * 1024, // 64KB read buffer
	}
}

// SharedTransport is a global high-performance transport shared across all providers.
// Sharing a single transport maximizes connection reuse across providers that
// share the same host (e.g., multiple Azure deployments).
var SharedTransport = NewHighPerfTransport()

// NewHighPerfClient creates an HTTP client using the shared high-perf transport.
// Each provider gets its own client (for timeout customization) but shares the transport.
func NewHighPerfClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: SharedTransport,
	}
}

// NewStreamingClient returns a client with no whole-request deadline, for
// providers that stream.
//
// http.Client.Timeout covers reading the body, so any value at all truncates a
// long stream mid-response. The wait that does need bounding is the one for
// response headers — a dead endpoint that accepts the connection and then says
// nothing — and that is what headerTimeout bounds.
func NewStreamingClient(headerTimeout time.Duration) *http.Client {
	transport := NewHighPerfTransport()
	transport.ResponseHeaderTimeout = headerTimeout
	return &http.Client{Timeout: 0, Transport: transport}
}
