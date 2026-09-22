package perf

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewHighPerfTransport_ConnectionPoolingSettings(t *testing.T) {
	tr := NewHighPerfTransport()

	assert.Equal(t, 2048, tr.MaxIdleConns)
	assert.Equal(t, 1024, tr.MaxIdleConnsPerHost)
	assert.Equal(t, 0, tr.MaxConnsPerHost)
	assert.Equal(t, 120*time.Second, tr.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	assert.True(t, tr.ForceAttemptHTTP2)
	assert.Equal(t, 1*time.Second, tr.ExpectContinueTimeout)
	assert.Equal(t, 300*time.Second, tr.ResponseHeaderTimeout)
	assert.Equal(t, 64*1024, tr.WriteBufferSize)
	assert.Equal(t, 64*1024, tr.ReadBufferSize)
}

func TestNewHighPerfTransport_TLSConfig(t *testing.T) {
	tr := NewHighPerfTransport()
	tlsConfig := tr.TLSClientConfig
	assert.NotNil(t, tlsConfig)
	assert.Equal(t, uint16(tls.VersionTLS12), tlsConfig.MinVersion)
}

func TestNewHighPerfTransport_ReturnsIndependentInstances(t *testing.T) {
	tr1 := NewHighPerfTransport()
	tr2 := NewHighPerfTransport()
	assert.NotSame(t, tr1, tr2, "each call must construct a fresh transport, not share state")
}

func TestNewHighPerfClient_SetsTimeoutAndSharesTransport(t *testing.T) {
	client := NewHighPerfClient(42 * time.Second)
	assert.Equal(t, 42*time.Second, client.Timeout)
	assert.Same(t, SharedTransport, client.Transport, "NewHighPerfClient must reuse the shared transport for connection pooling across providers")
}

func TestNewHighPerfClient_MultipleClientsShareSameTransport(t *testing.T) {
	c1 := NewHighPerfClient(10 * time.Second)
	c2 := NewHighPerfClient(20 * time.Second)
	assert.Same(t, c1.Transport, c2.Transport)
}
