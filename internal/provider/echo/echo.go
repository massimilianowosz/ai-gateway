// Package echo implements a mock provider that returns instant synthetic responses.
// Use it for cache benchmarking without real LLM latency.
package echo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// Provider returns instant canned responses. Zero network calls.
type Provider struct {
	model string
}

// New creates an echo provider for the given model name.
func New(model string) *Provider {
	return &Provider{model: model}
}

func (p *Provider) Name() string { return "echo" }

// Complete returns a synthetic chat completion response instantly.
func (p *Provider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	now := time.Now().Unix()
	finish := "stop"
	return &provider.CompletionResponse{
		ID:      fmt.Sprintf("echo-%d", now),
		Object:  "chat.completion",
		Created: now,
		Model:   req.Model,
		Choices: []provider.Choice{{
			Index: 0,
			Message: &provider.Message{
				Role:    "assistant",
				Content: "This is a synthetic response from the echo provider for benchmarking purposes.",
			},
			FinishReason: &finish,
		}},
		Usage: &provider.Usage{
			PromptTokens:     25,
			CompletionTokens: 15,
			TotalTokens:      40,
		},
	}, nil
}

// Stream returns a single-chunk stream that immediately completes.
func (p *Provider) Stream(_ context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return &echoStream{model: req.Model, done: false}, nil
}

type echoStream struct {
	model string
	done  bool
}

func (s *echoStream) Next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	now := time.Now().Unix()
	chunk := fmt.Sprintf(`{"id":"echo-%d","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":{"role":"assistant","content":"Synthetic echo response for benchmarking."},"finish_reason":"stop"}]}`, now, now, s.model)
	return []byte(chunk), nil
}

func (s *echoStream) Close() error         { return nil }
func (s *echoStream) Headers() http.Header { return http.Header{} }
