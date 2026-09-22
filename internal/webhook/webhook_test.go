package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// --- HMAC signing ---

func TestSign_Deterministic(t *testing.T) {
	s1 := sign([]byte("payload"), "secret")
	s2 := sign([]byte("payload"), "secret")
	assert.Equal(t, s1, s2)
	assert.Len(t, s1, 64, "SHA-256 hex is 64 chars")
}

func TestSign_DifferentSecrets(t *testing.T) {
	s1 := sign([]byte("payload"), "secret-a")
	s2 := sign([]byte("payload"), "secret-b")
	assert.NotEqual(t, s1, s2)
}

// --- Dispatcher delivery ---

func TestDispatcher_DeliversSignedEvent(t *testing.T) {
	var body []byte
	var signature string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signature = r.Header.Get("X-Ubiquum-Signature")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{
		URL:    server.URL,
		Secret: "top-secret",
		Events: []string{EventSpendTracked},
	}}, discardLogger())

	d.Emit(EventSpendTracked, map[string]any{"cost": 0.01})
	d.Close()

	require.NotEmpty(t, body)
	assert.Equal(t, "sha256="+sign(body, "top-secret"), signature)
}

func TestDispatcher_NoSecret_NoSignatureHeader(t *testing.T) {
	var gotSig string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Ubiquum-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventSpendTracked, map[string]any{"cost": 0.01})
	d.Close()

	assert.Empty(t, gotSig)
}

func TestDispatcher_PayloadShape(t *testing.T) {
	var payload Payload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventBudgetCrossed, map[string]any{"scope": "key"})
	d.Close()

	assert.Equal(t, EventBudgetCrossed, payload.Event)
	assert.False(t, payload.Timestamp.IsZero())
	require.NotNil(t, payload.Data)
}

// --- Event filtering ---

func TestDispatcher_EventFilter_SubscribedEvent_Delivered(t *testing.T) {
	var count int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{
		URL:    server.URL,
		Events: []string{EventBudgetCrossed},
	}}, discardLogger())

	d.Emit(EventBudgetCrossed, nil)
	d.Emit(EventBudgetCrossed, nil)
	d.Close()

	assert.EqualValues(t, 2, atomic.LoadInt32(&count))
}

func TestDispatcher_EventFilter_UnsubscribedEvent_NotDelivered(t *testing.T) {
	var count int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Only subscribes to budget.crossed — spend_tracked must NOT be delivered
	d := NewDispatcher([]Config{{
		URL:    server.URL,
		Events: []string{EventBudgetCrossed},
	}}, discardLogger())

	d.Emit(EventSpendTracked, nil) // should be dropped by filter
	d.Close()

	assert.EqualValues(t, 0, atomic.LoadInt32(&count))
}

func TestDispatcher_EmptyEventFilter_DeliversAll(t *testing.T) {
	var count int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// No Events filter → subscribe to everything
	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventSpendTracked, nil)
	d.Emit(EventBudgetCrossed, nil)
	d.Emit(EventKeyCreated, nil)
	d.Close()

	assert.EqualValues(t, 3, atomic.LoadInt32(&count))
}

// --- Multiple subscribers ---

func TestDispatcher_MultipleSubscribers(t *testing.T) {
	var countA, countB int32

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&countA, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&countB, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	d := NewDispatcher([]Config{
		{URL: serverA.URL, Events: []string{EventSpendTracked}},
		{URL: serverB.URL, Events: []string{EventSpendTracked}},
	}, discardLogger())

	d.Emit(EventSpendTracked, nil)
	d.Close()

	assert.EqualValues(t, 1, atomic.LoadInt32(&countA))
	assert.EqualValues(t, 1, atomic.LoadInt32(&countB))
}

// --- Error resilience ---

func TestDispatcher_ServerError_DoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventSpendTracked, nil)
	// Should not panic or block
	require.NotPanics(t, func() { d.Close() })
}

func TestDispatcher_UnreachableURL_DoesNotPanic(t *testing.T) {
	d := NewDispatcher([]Config{{URL: "http://127.0.0.1:1"}}, discardLogger())
	d.Emit(EventSpendTracked, nil)
	require.NotPanics(t, func() { d.Close() })
}

// --- No-op when no configs ---

func TestDispatcher_NoConfigs_Emit_IsNoOp(t *testing.T) {
	d := NewDispatcher(nil, discardLogger())
	// Should not panic, no worker goroutine
	require.NotPanics(t, func() {
		d.Emit(EventSpendTracked, nil)
	})
	// Close on a no-config dispatcher must not block
	done := make(chan struct{})
	go func() {
		d.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close() blocked on no-config dispatcher")
	}
}

func TestDispatcher_ContentTypeHeader(t *testing.T) {
	var ct string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventKeyCreated, nil)
	d.Close()

	assert.Equal(t, "application/json", ct)
}

func TestDispatcher_UserAgentHeader(t *testing.T) {
	var ua string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher([]Config{{URL: server.URL}}, discardLogger())
	d.Emit(EventKeyCreated, nil)
	d.Close()

	assert.Contains(t, ua, "Ubiquum-Gateway-Webhook")
}

func TestShouldDeliver_NoFilter(t *testing.T) {
	d := &Dispatcher{}
	assert.True(t, d.shouldDeliver(Config{Events: nil}, EventSpendTracked))
	assert.True(t, d.shouldDeliver(Config{Events: []string{}}, EventBudgetCrossed))
}

func TestShouldDeliver_WithFilter(t *testing.T) {
	d := &Dispatcher{}
	cfg := Config{Events: []string{EventBudgetCrossed, EventKeyCreated}}
	assert.True(t, d.shouldDeliver(cfg, EventBudgetCrossed))
	assert.True(t, d.shouldDeliver(cfg, EventKeyCreated))
	assert.False(t, d.shouldDeliver(cfg, EventSpendTracked))
	assert.False(t, d.shouldDeliver(cfg, EventRateLimited))
}
