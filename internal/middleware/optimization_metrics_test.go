package middleware

import (
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The counters exist to answer "how often did the guard protect a cache" and
// "how much context did CCR recover" — questions per-request headers cannot
// answer. This asserts they reach the scrape output.
func TestOptimizationMetricsAreExposed(t *testing.T) {
	m := NewMetrics()

	m.RecordGuardDecision("prefix_cache_cheaper", "anthropic")
	m.RecordGuardDecision("prefix_cache_cheaper", "anthropic")
	m.RecordGuardDecision("no_cached_prefix", "openai")
	m.RecordCCRInjection(2, 340)
	m.RecordCCRBudgetSkip()
	m.RecordCacheTokens(18000, 400)

	rec := httptest.NewRecorder()
	m.Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		`ubiquum_prefix_cache_decisions_total{reason="prefix_cache_cheaper",api="anthropic"} 2`,
		`ubiquum_prefix_cache_decisions_total{reason="no_cached_prefix",api="openai"} 1`,
		"ubiquum_ccr_injections_total 1",
		"ubiquum_ccr_files_total 2",
		"ubiquum_ccr_tokens_total 340",
		"ubiquum_ccr_budget_skips_total 1",
		"ubiquum_prompt_cache_read_tokens_total 18000",
		"ubiquum_prompt_cache_write_tokens_total 400",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape output is missing %q", want)
		}
	}
}

// Labels must stay low-cardinality: a request or session id as a label would
// make the series unbounded.
func TestGuardDecisionLabelsStayBounded(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < 1000; i++ {
		m.RecordGuardDecision("prefix_cache_cheaper", "anthropic")
	}
	m.opt.mu.RLock()
	n := len(m.opt.guardDecisions)
	m.opt.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expected one series for one reason+api pair, got %d", n)
	}
}

func TestEmptyOptimizationMetricsStillScrape(t *testing.T) {
	rec := httptest.NewRecorder()
	NewMetrics().Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "ubiquum_ccr_injections_total 0") {
		t.Fatal("a fresh collector must still expose zeroed optimization counters")
	}
}

// The byte counter is the headline number for live compression, so it must
// count each request's saving once. Passing a whole-request aggregate once per
// changed block multiplied it by the block count.
func TestLiveCompressionBytesCountedOncePerRequest(t *testing.T) {
	m := NewMetrics()
	m.RecordLiveCompression("json_compact", 3, 3000, 1000)
	m.RecordLiveCompression("logs_collapse", 1, 900, 400)

	rec := httptest.NewRecorder()
	m.Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		`ubiquum_live_compression_blocks_total{transformer="json_compact"} 3`,
		`ubiquum_live_compression_blocks_total{transformer="logs_collapse"} 1`,
		"ubiquum_live_compression_bytes_saved_total 2500", // 2000 + 500, not 6000 + 500
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape output is missing %q\n%s", want, out)
		}
	}
}

// A scrape runs on the metrics endpoint's goroutine while request handlers
// record. Creating a counter map lazily on the first record made that a write
// to a map header the scrape reads unsynchronised — a one-shot race, so each
// iteration needs a fresh Metrics to have a first record at all. Meaningful
// under -race.
func TestLiveCompressionCountersAreRaceFree(t *testing.T) {
	for i := 0; i < 3000; i++ {
		m := NewMetrics()
		spin := i % 97
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for s := 0; s < spin; s++ {
				runtime.Gosched()
			}
			m.RecordLiveCompression("json_compact", 1, 100, 40)
			m.RecordLiveCompressionSkip("no_gain")
		}()
		go func() {
			defer wg.Done()
			<-start
			m.Handler()(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
		}()
		close(start)
		wg.Wait()
	}
}
