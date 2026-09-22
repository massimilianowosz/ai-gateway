// Package steering trims what a model is asked to produce, rather than what it
// is asked to read.
//
// Live compression and HiveState both work on the prompt. This package works
// on the answer, which is where the money is: output tokens cost several times
// what input tokens cost. The lever is a terse-output note appended to the end
// of the system prompt, so the model stops restating context it was just
// given.
//
// Reasoning effort is deliberately *not* handled here. HiveRoute already owns
// that decision, and two middlewares writing the same field would eventually
// disagree about which one won.
//
// The note is opt-in, and every failure path forwards the body untouched.
package steering

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// Recorder receives aggregate counters. Narrow on purpose, and nil-safe.
type Recorder interface {
	RecordSteeringVerbosity(api string)
}

// Actions records what a rewrite changed, for metrics and the response header.
type Actions struct {
	Verbosity bool
}

// Middleware appends the terse-output note to a request body.
func Middleware(cfg config.SteeringConfig, rec Recorder, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			var steer func([]byte, config.SteeringConfig) ([]byte, Actions)
			var api string
			switch r.URL.Path {
			case "/v1/messages":
				steer, api = steerAnthropic, "anthropic"
			case "/v1/chat/completions":
				steer, api = steerOpenAI, "openai"
			case "/v1/responses":
				steer, api = steerResponses, "responses"
			default:
				next.ServeHTTP(w, r)
				return
			}

			// The same contract the live-compression opt-out has: a caller
			// asserting what it wants delivered outranks the operator.
			if r.Header.Get("x-ubiquum-steering") == "false" {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			out, acts := safeSteer(steer, body, cfg)
			if len(out) == 0 || !acts.Verbosity {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("x-steering-verbosity", "applied")
			record(rec, func(rr Recorder) { rr.RecordSteeringVerbosity(api) })
			logger.Info("steering: applied",
				"path", r.URL.Path, "body_before", len(body), "body_after", len(out))

			r.Body = io.NopCloser(bytes.NewReader(out))
			r.ContentLength = int64(len(out))
			next.ServeHTTP(w, r)
		})
	}
}

// safeSteer turns a panic in a rewrite into an untouched body.
func safeSteer(fn func([]byte, config.SteeringConfig) ([]byte, Actions), body []byte, cfg config.SteeringConfig) (out []byte, acts Actions) {
	defer func() {
		if r := recover(); r != nil {
			out, acts = nil, Actions{}
		}
	}()
	return fn(body, cfg)
}

func record(rec Recorder, f func(Recorder)) {
	if rec != nil {
		f(rec)
	}
}
