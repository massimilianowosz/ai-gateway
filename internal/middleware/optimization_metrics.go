package middleware

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// Context-optimization counters.
//
// These answer the two operational questions the prefix-cache guard and CCR
// raise: how often did the gateway decline to compress in order to protect a
// provider cache, and how much recovered context did it re-inject. Both are
// otherwise only visible in per-request headers and logs, which cannot be
// aggregated.
//
// Labels are deliberately low-cardinality — a decision reason and an API
// surface, both drawn from fixed sets. Request IDs, session IDs and key
// prefixes are never used as labels.
type optimizationMetrics struct {
	mu sync.RWMutex
	// guardDecisions counts prefix-cache guard outcomes by "reason:api".
	guardDecisions map[string]*atomic.Int64

	ccrInjections  atomic.Int64 // requests where recovered context was re-injected
	ccrSkipsBudget atomic.Int64 // requests where retrieval was dropped to protect savings
	ccrFilesTotal  atomic.Int64 // recovered files injected
	ccrTokensTotal atomic.Int64 // tokens of recovered context injected

	cachedPromptTokens  atomic.Int64 // provider-reported prompt-cache reads
	cacheCreationTokens atomic.Int64 // provider-reported prompt-cache writes

	// liveBlocks counts compressed tool-output blocks by transformer.
	liveBlocks     map[string]*atomic.Int64
	liveSkips      map[string]*atomic.Int64
	liveBytesSaved atomic.Int64

	// steerVerbosity counts requests given the terse-output note, by API.
	steerVerbosity map[string]*atomic.Int64
}

// RecordSteeringVerbosity records one request given the terse-output note.
func (m *Metrics) RecordSteeringVerbosity(api string) {
	if api == "" {
		api = "unknown"
	}
	m.counterFor(m.opt.steerVerbosity, api).Add(1)
}

// RecordGuardDecision records one prefix-cache guard outcome. reason and api
// must come from the guard's fixed vocabulary.
func (m *Metrics) RecordGuardDecision(reason, api string) {
	if reason == "" {
		return
	}
	key := reason + ":" + api
	m.opt.mu.RLock()
	c, ok := m.opt.guardDecisions[key]
	m.opt.mu.RUnlock()
	if !ok {
		m.opt.mu.Lock()
		if c, ok = m.opt.guardDecisions[key]; !ok {
			c = &atomic.Int64{}
			m.opt.guardDecisions[key] = c
		}
		m.opt.mu.Unlock()
	}
	c.Add(1)
}

// RecordCCRInjection records recovered context reaching the request body.
func (m *Metrics) RecordCCRInjection(files, tokens int) {
	m.opt.ccrInjections.Add(1)
	m.opt.ccrFilesTotal.Add(int64(files))
	m.opt.ccrTokensTotal.Add(int64(tokens))
}

// RecordCCRBudgetSkip records retrieval declined because re-injecting would
// have eaten the compression savings.
func (m *Metrics) RecordCCRBudgetSkip() { m.opt.ccrSkipsBudget.Add(1) }

// RecordLiveCompression records the tool-output blocks one transformer
// compressed in a request, and the bytes those blocks shed.
func (m *Metrics) RecordLiveCompression(transformer string, blocks, before, after int) {
	if transformer == "" {
		transformer = "unknown"
	}
	if blocks <= 0 {
		return
	}
	m.counterFor(m.opt.liveBlocks, transformer).Add(int64(blocks))
	if d := before - after; d > 0 {
		m.opt.liveBytesSaved.Add(int64(d))
	}
}

// RecordLiveCompressionSkip records a request the transform declined, by cause.
func (m *Metrics) RecordLiveCompressionSkip(reason string) {
	if reason == "" {
		reason = "unknown"
	}
	m.counterFor(m.opt.liveSkips, reason).Add(1)
}

// counterFor returns the counter for key, creating it under the write lock.
//
// It takes the map by value rather than by pointer on purpose: every counter
// map is built in NewMetrics and never reassigned, so the map header itself is
// immutable and readers may copy it without synchronisation. Lazy creation
// here would be a write to that header racing the unsynchronised reads in
// writeOptimizationMetrics.
func (m *Metrics) counterFor(set map[string]*atomic.Int64, key string) *atomic.Int64 {
	m.opt.mu.RLock()
	c, ok := set[key]
	m.opt.mu.RUnlock()
	if ok {
		return c
	}
	m.opt.mu.Lock()
	defer m.opt.mu.Unlock()
	if c, ok = set[key]; !ok {
		c = &atomic.Int64{}
		set[key] = c
	}
	return c
}

// RecordCacheTokens records a provider-reported prompt-cache breakdown, so
// cache hit rate can be tracked against the guard's decisions.
func (m *Metrics) RecordCacheTokens(cached, created int) {
	m.opt.cachedPromptTokens.Add(int64(cached))
	m.opt.cacheCreationTokens.Add(int64(created))
}

// writeOptimizationMetrics appends the optimization series in Prometheus
// exposition format.
func (m *Metrics) writeOptimizationMetrics(w io.Writer) {
	fmt.Fprintf(w, "# HELP ubiquum_prefix_cache_decisions_total Prefix-cache guard outcomes.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_prefix_cache_decisions_total counter\n")
	m.opt.mu.RLock()
	keys := make([]string, 0, len(m.opt.guardDecisions))
	for k := range m.opt.guardDecisions {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output; scrapers and diffs both prefer it
	for _, k := range keys {
		parts := splitMetricKey(k)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(w, "ubiquum_prefix_cache_decisions_total{reason=%q,api=%q} %d\n",
			parts[0], parts[1], m.opt.guardDecisions[k].Load())
	}
	m.opt.mu.RUnlock()

	fmt.Fprintf(w, "# HELP ubiquum_ccr_injections_total Requests where recovered context was re-injected.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_ccr_injections_total counter\n")
	fmt.Fprintf(w, "ubiquum_ccr_injections_total %d\n", m.opt.ccrInjections.Load())

	fmt.Fprintf(w, "# HELP ubiquum_ccr_budget_skips_total Retrievals declined to preserve compression savings.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_ccr_budget_skips_total counter\n")
	fmt.Fprintf(w, "ubiquum_ccr_budget_skips_total %d\n", m.opt.ccrSkipsBudget.Load())

	fmt.Fprintf(w, "# HELP ubiquum_ccr_files_total Recovered files re-injected.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_ccr_files_total counter\n")
	fmt.Fprintf(w, "ubiquum_ccr_files_total %d\n", m.opt.ccrFilesTotal.Load())

	fmt.Fprintf(w, "# HELP ubiquum_ccr_tokens_total Tokens of recovered context re-injected.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_ccr_tokens_total counter\n")
	fmt.Fprintf(w, "ubiquum_ccr_tokens_total %d\n", m.opt.ccrTokensTotal.Load())

	fmt.Fprintf(w, "# HELP ubiquum_prompt_cache_read_tokens_total Prompt tokens served from the provider cache.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_prompt_cache_read_tokens_total counter\n")
	fmt.Fprintf(w, "ubiquum_prompt_cache_read_tokens_total %d\n", m.opt.cachedPromptTokens.Load())

	fmt.Fprintf(w, "# HELP ubiquum_prompt_cache_write_tokens_total Prompt tokens written into the provider cache.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_prompt_cache_write_tokens_total counter\n")
	fmt.Fprintf(w, "ubiquum_prompt_cache_write_tokens_total %d\n", m.opt.cacheCreationTokens.Load())

	fmt.Fprintf(w, "# HELP ubiquum_live_compression_blocks_total Tool-output blocks compressed, by transformer.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_live_compression_blocks_total counter\n")
	m.writeLabelled(w, "ubiquum_live_compression_blocks_total", "transformer", m.opt.liveBlocks)

	fmt.Fprintf(w, "# HELP ubiquum_live_compression_skips_total Requests the live transform declined, by cause.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_live_compression_skips_total counter\n")
	m.writeLabelled(w, "ubiquum_live_compression_skips_total", "reason", m.opt.liveSkips)

	fmt.Fprintf(w, "# HELP ubiquum_live_compression_bytes_saved_total Request bytes removed by live compression.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_live_compression_bytes_saved_total counter\n")
	fmt.Fprintf(w, "ubiquum_live_compression_bytes_saved_total %d\n", m.opt.liveBytesSaved.Load())

	fmt.Fprintf(w, "# HELP ubiquum_steering_verbosity_total Requests given the terse-output note, by API.\n")
	fmt.Fprintf(w, "# TYPE ubiquum_steering_verbosity_total counter\n")
	m.writeLabelled(w, "ubiquum_steering_verbosity_total", "api", m.opt.steerVerbosity)
}

// writeLabelled emits a single-label counter family in stable order.
func (m *Metrics) writeLabelled(w io.Writer, name, label string, set map[string]*atomic.Int64) {
	m.opt.mu.RLock()
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, set[k].Load())
	}
	m.opt.mu.RUnlock()
}
