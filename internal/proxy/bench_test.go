package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// BenchmarkMiddlewareChain measures overhead of the full middleware chain
// (RequestID + Recovery) without the actual proxy handler.
func BenchmarkMiddlewareChain(b *testing.B) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	logger := slog.Default()
	handler := middleware.Chain(noop,
		middleware.RequestID,
		middleware.Recovery(logger),
	)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}
}

// BenchmarkRequestParsing measures JSON decode speed of a typical chat completion request.
func BenchmarkRequestParsing(b *testing.B) {
	payload := provider.CompletionRequest{
		Model: "gpt-4o",
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Explain quantum computing in one paragraph."},
		},
		Stream: false,
	}
	data, _ := json.Marshal(payload)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req provider.CompletionRequest
		_ = json.NewDecoder(bytes.NewReader(data)).Decode(&req)
	}
}

// BenchmarkResponseSerialization measures JSON encode speed of a typical response.
func BenchmarkResponseSerialization(b *testing.B) {
	stop := "stop"
	resp := provider.CompletionResponse{
		ID:      "chatcmpl-abc123",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   "gpt-4o",
		Choices: []provider.Choice{
			{
				Index:        0,
				Message:      &provider.Message{Role: "assistant", Content: "Quantum computing uses qubits that can exist in superpositions..."},
				FinishReason: &stop,
			},
		},
		Usage: &provider.Usage{
			PromptTokens:     25,
			CompletionTokens: 42,
			TotalTokens:      67,
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(resp)
	}
}
