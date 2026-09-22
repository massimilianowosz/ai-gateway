package livezone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// MetricWriter persists one row per compressed request. Narrow on purpose, and
// nil-safe: an environment without a database still compresses, it just cannot
// attribute the saving.
//
// The row goes into the same table HiveState writes, under mode "live". The
// two shrink disjoint parts of the prompt — history versus the newest tool
// output — so a single aggregation over both is the honest total, and the
// analytics query already sums `mode IN ('state','live')`. Counters in memory
// cannot serve that purpose: they reset with the pod, they are per replica,
// and they carry no key, so nothing can be attributed to the customer whose
// bill it reduced.
type MetricWriter interface {
	LogHiveStateMetric(ctx context.Context, metric store.HiveStateMetric) error
}

// Recorder receives aggregate counters. Narrow on purpose, and nil-safe.
type Recorder interface {
	// RecordLiveCompression reports the blocks one transformer changed in a
	// single request, with the total size of those blocks before and after.
	RecordLiveCompression(transformer string, blocks, bytesBefore, bytesAfter int)
	RecordLiveCompressionSkip(reason string)
}

// Middleware compresses the newest tool output in a request before it reaches
// the provider.
//
// It runs ahead of HiveState so history compression sees an already-slimmer
// delta, and it only ever rewrites the live zone, so the two cannot fight over
// the same bytes.
//
// Every failure path forwards the original body: an unparseable request, a
// transform that did not help, a disabled tenant, a panic. Compression is an
// optimisation, and no optimisation is worth a failed request.
func Middleware(cfg config.LiveCompressionConfig, rec Recorder, db MetricWriter, logger *slog.Logger) func(http.Handler) http.Handler {
	pol := policyFrom(cfg)
	if cfg.RetrieveTTLMinutes > 0 {
		defaultVault = NewVault(cfg.RetrieveMaxEntries, cfg.RetrieveMaxMB<<20,
			time.Duration(cfg.RetrieveTTLMinutes)*time.Minute)
	}
	pol.Vault = defaultVault

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			var rewrite func([]byte, Policy) ([]byte, *Stats, error)
			switch r.URL.Path {
			case "/v1/messages":
				rewrite = RewriteAnthropic
			case "/v1/chat/completions":
				rewrite = RewriteOpenAI
			case "/v1/responses":
				rewrite = RewriteResponses
			default:
				next.ServeHTTP(w, r)
				return
			}

			// An explicit opt-out is a contract assertion about the bytes the
			// caller wants delivered, and outranks the operator's setting.
			if r.Header.Get("x-ubiquum-live-compression") == "false" {
				skip(rec, "opt_out_header")
				next.ServeHTTP(w, r)
				return
			}

			if !inCanary(r, cfg.CanaryPercent) {
				skip(rec, "canary_excluded")
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				// An oversized body is answered here, with the status that says
				// so. Forwarding the truncated reader made the handler report a
				// JSON parse error instead — a 400 sending the caller to look
				// for a syntax mistake that is not there.
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					skip(rec, "body_too_large")
					writeJSONError(w, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("request body exceeds the %d byte limit", tooLarge.Limit))
					return
				}
				logger.Warn("livezone: request body read failed, forwarding as-is", "error", err)
				skip(rec, "body_read_error")
				r.Body = io.NopCloser(bytes.NewReader(body))
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The vault partition is the caller's, so what one key had removed
			// can only ever be read back by that same key.
			reqPol := pol
			if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
				reqPol.Scope = ScopeFor(ki.TeamID, ki.KeyHash)
			}

			out, stats, err := safeRewrite(rewrite, body, reqPol)
			if err != nil || len(out) == 0 || len(out) >= len(body) {
				if err != nil {
					logger.Warn("livezone: rewrite failed, forwarding original", "error", err)
					skip(rec, "rewrite_error")
				} else {
					skip(rec, "no_gain")
					// "Nothing was compressed" has three very different
					// meanings — every block was protected by policy, no
					// transformer recognised the content, or there was no
					// tool output at all — and only the counts tell them
					// apart. Without this a no-op reads the same as a
					// misconfiguration.
					logger.Info("livezone: nothing compressed",
						"path", r.URL.Path, "body_bytes", len(body),
						"blocks_seen", stats.BlocksSeen,
						"protected_skipped", stats.SkippedProtect,
						"unchanged", stats.SkippedNoChange,
						"reasons", stats.SkipReasons)
				}
				next.ServeHTTP(w, r)
				return
			}

			// One call per transformer, carrying that transformer's own byte
			// totals. Passing the whole-request aggregate once per block would
			// multiply the reported saving by the number of blocks.
			for name, n := range stats.Transformers {
				d := stats.BytesByTransformer[name]
				rec2(rec, name, n, d.Before, d.After)
			}
			w.Header().Set("x-livezone-blocks", itoa(stats.BlocksChanged))
			w.Header().Set("x-livezone-bytes-saved", itoa(len(body)-len(out)))
			recordSaving(r, db, body, out, logger)

			logger.Info("livezone: compressed live tool output",
				"path", r.URL.Path,
				"blocks_changed", stats.BlocksChanged,
				"protected_skipped", stats.SkippedProtect,
				"body_before", len(body),
				"body_after", len(out),
				"transformers", stats.Transformers,
			)

			r.Body = io.NopCloser(bytes.NewReader(out))
			r.ContentLength = int64(len(out))
			next.ServeHTTP(w, r)
		})
	}
}

// writeJSONError emits an OpenAI-shaped error, which every surface this
// middleware sits on understands.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": message, "type": "invalid_request_error"},
	})
}

// safeRewrite turns a panic in a transformer into an ordinary error.
func safeRewrite(fn func([]byte, Policy) ([]byte, *Stats, error), body []byte, pol Policy) (out []byte, stats *Stats, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, stats, err = nil, newStats(), errPanic
		}
	}()
	return fn(body, pol)
}

type panicErr struct{}

func (panicErr) Error() string { return "panic in live compression" }

var errPanic = panicErr{}

func policyFrom(cfg config.LiveCompressionConfig) Policy {
	opts := DefaultOptions()
	opts.MinBytes = cfg.MinBytes
	opts.MaxBytes = cfg.MaxBytes
	opts.AllowLossy = cfg.AllowLossy
	opts.MinGainRatio = float64(cfg.MinGainPercent) / 100
	opts.JSONTruncate = JSONTruncateConfig{
		MinItems: cfg.JSONMinItems,
		KeepHead: cfg.JSONKeepHead,
		KeepTail: cfg.JSONKeepTail,
	}
	opts.Diff = DiffConfig{Context: cfg.DiffContext, MinHunkLines: DefaultDiffConfig().MinHunkLines}
	opts.CSV = CSVConfig{
		MinRows:  cfg.CSVMinRows,
		KeepHead: cfg.CSVKeepHead,
		KeepTail: cfg.CSVKeepTail,
	}
	return Policy{
		Options:        opts,
		ProtectedTools: cfg.ProtectedTools,
		LiveTurns:      cfg.LiveTurns,
		DedupeRepeats:  cfg.DedupeRepeats,
		DeltaRepeats:   cfg.DeltaRepeats,
		PruneReasoning: cfg.PruneReasoning,
	}
}

// inCanary decides whether a request participates, keyed by the caller's API
// key so a given caller sees consistent behaviour instead of flickering
// between compressed and uncompressed turns mid-session.
func inCanary(r *http.Request, percent int) bool {
	if percent <= 0 || percent >= 100 {
		return true
	}
	var seed string
	if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
		seed = ki.KeyHash
	}
	if seed == "" {
		// No stable identity: leave the request out rather than flip-flop.
		return false
	}
	sum := sha256.Sum256([]byte(seed))
	return int(binary.BigEndian.Uint32(sum[:4])%100) < percent
}

func skip(rec Recorder, reason string) {
	if rec != nil {
		rec.RecordLiveCompressionSkip(reason)
	}
}

func rec2(rec Recorder, name string, blocks, before, after int) {
	if rec != nil {
		rec.RecordLiveCompression(name, blocks, before, after)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// recordSaving files what this request saved against the calling key.
//
// Tokens are estimated at four characters each, the same ratio HiveState uses
// for the side of its own figure the provider never reports. Both are
// estimates of a body that was never sent, which is the only thing either can
// be: the provider only ever counts what it received.
func recordSaving(r *http.Request, db MetricWriter, before, after []byte, logger *slog.Logger) {
	if db == nil {
		return
	}
	saved := len(before) - len(after)
	if saved <= 0 {
		return
	}

	keyPrefix, teamID := "", ""
	if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
		keyPrefix, teamID = ki.KeyPrefix, ki.TeamID
	}
	if keyPrefix == "" {
		// Nothing to attribute the saving to; the counters still have it.
		return
	}

	originalTokens := estimateTokens(len(before))
	resultTokens := estimateTokens(len(after))
	ratio := 0.0
	if originalTokens > 0 {
		ratio = 1.0 - float64(resultTokens)/float64(originalTokens)
	}

	metric := store.HiveStateMetric{
		RequestID:      r.Header.Get("x-request-id"),
		RequestModel:   requestModel(before),
		KeyPrefix:      keyPrefix,
		Mode:           "live",
		OriginalTokens: originalTokens,
		ResultTokens:   resultTokens,
		Ratio:          ratio,
		CreatedAt:      time.Now(),
	}

	go func() {
		if err := db.LogHiveStateMetric(context.Background(), metric); err != nil {
			logger.Warn("livezone: saving not recorded",
				"error", err, "key_prefix", keyPrefix, "team_id", teamID)
		}
	}()
}

// estimateTokens converts bytes to tokens at the ratio the rest of the gateway
// uses. A body that is not empty is never worth zero tokens.
func estimateTokens(size int) int {
	n := size / 4
	if n == 0 && size > 0 {
		return 1
	}
	return n
}

// requestModel reads the model the caller asked for, which is what the savings
// aggregation prices the removed tokens at.
func requestModel(body []byte) string {
	var parsed struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Model
}
