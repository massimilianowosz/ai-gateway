package hivetrace

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// Metrics is the counter surface the recorder reports to. Implementations must
// keep labels low-cardinality: session and request ids are never labels.
type Metrics interface {
	RecordTraceEvent(api string)
	RecordTraceDropped()
	RecordTraceFinding(kind, origin string)
}

// capture is the raw material one request produced. Nothing here is parsed,
// scanned or redacted yet: that work belongs to the worker, so the request
// path pays only for two body copies.
type capture struct {
	event         Event
	sessionHeader string
	clientApp     string
	upstream      string
	requestBody   []byte
	responseBody  []byte
	responseCut   bool
}

// anonymizationConfigStore mirrors the optional capability the guardrail
// middleware uses, so PII detection here honours the same tenant entity
// selection instead of reporting entities the tenant switched off.
type anonymizationConfigStore interface {
	GetAnonymizationEntities(ctx context.Context, teamID string) (*[]string, error)
}

// watchlistStore is the appliance's declared terms. Optional, like the
// anonymisation config: a gateway running without a database still records
// traffic, it just has nothing on the watchlist.
type watchlistStore interface {
	ListWatchlistTerms(ctx context.Context) ([]store.WatchlistTerm, error)
}

// detectorSettingsStore holds which PII types the operator wants reported.
type detectorSettingsStore interface {
	ListDetectorSettings(ctx context.Context) ([]store.DetectorSetting, error)
}

// watchlistReload is how long a term edited in the console takes to reach the
// scanner. Polling rather than invalidating keeps the admin handler and the
// recorder from having to know about each other, and one small query every
// half minute is cheaper than the plumbing would be.
const watchlistReload = 30 * time.Second

// Recorder buffers captures and turns them into stored events on a background
// worker, following the same shape as spend.BatchWriter.
//
// Record never blocks and never fails a request. When the queue is full the
// capture is dropped and counted: traffic observability that applies
// back-pressure to the proxy has become a reliability problem, which is a
// worse outcome than a gap in the trace.
type Recorder struct {
	store    TrafficStore
	cfg      config.HiveTraceConfig
	det      *detector
	entities anonymizationConfigStore
	terms    watchlistStore
	settings detectorSettingsStore
	tasks    *taskClassifier
	metrics  Metrics
	hub      *Hub
	logger   *slog.Logger
	hosts    *hostnameResolver

	mu      sync.Mutex
	buffer  []capture
	dropped int

	stopCh  chan struct{}
	stopped chan struct{}
}

// NewRecorder starts the ingestion worker. db is optional and is read only to
// resolve a tenant's PII entity selection; hub is optional and receives live
// updates for connected consoles.
func NewRecorder(ts TrafficStore, db store.Store, cfg config.HiveTraceConfig, metrics Metrics, hub *Hub, logger *slog.Logger) *Recorder {
	cfg.ApplyDefaults()
	r := &Recorder{
		store:   ts,
		cfg:     cfg,
		det:     newDetector(),
		metrics: metrics,
		hub:     hub,
		logger:  logger,
		hosts:   newHostnameResolver(),
		tasks:   newTaskClassifier(cfg.TaskClassifier),
		buffer:  make([]capture, 0, 128),
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if cs, ok := db.(anonymizationConfigStore); ok {
		r.entities = cs
	}
	if ws, ok := db.(watchlistStore); ok {
		r.terms = ws
		r.reloadWatchlist()
	}
	if ss, ok := db.(detectorSettingsStore); ok {
		r.settings = ss
		r.reloadDetectorSettings()
	}
	go r.run()
	return r
}

// reloadDetectorSettings follows reloadWatchlist: a failed read keeps what is
// already loaded.
func (r *Recorder) reloadDetectorSettings() {
	if r.settings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.settings.ListDetectorSettings(ctx)
	if err != nil {
		r.logger.Warn("hivetrace: failed to load detector settings", "error", err)
		return
	}
	overrides := make(map[string]bool, len(rows))
	for _, row := range rows {
		overrides[row.Type] = row.Enabled
	}
	r.det.setReported(overrides)
}

// reloadWatchlist rebuilds the scanner from the stored terms. A failure keeps
// the scanner that is already loaded: dropping every watched term because one
// query timed out would turn a database hiccup into silent under-reporting.
func (r *Recorder) reloadWatchlist() {
	if r.terms == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.terms.ListWatchlistTerms(ctx)
	if err != nil {
		r.logger.Warn("hivetrace: failed to load watchlist terms", "error", err)
		return
	}
	terms := make([]guardrail.WatchTerm, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		terms = append(terms, guardrail.WatchTerm{Label: row.Label, Pattern: row.Pattern})
	}
	r.det.setWatchlist(guardrail.NewWatchlistScanner(terms))
}

// Record enqueues a capture. Nil-safe, because the middleware holds this as a
// pointer and records from a path where a panic is not recovered.
func (r *Recorder) Record(c capture) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.buffer) >= r.cfg.MaxQueue {
		r.dropped++
		r.mu.Unlock()
		if r.metrics != nil {
			r.metrics.RecordTraceDropped()
		}
		return
	}
	r.buffer = append(r.buffer, c)
	r.mu.Unlock()

	// A watching console sees the turn the moment it completes, before the
	// worker has enriched it. The check keeps this free when nobody is looking:
	// with no console open the publish never happens.
	if r.hub.Watchers() > 0 {
		r.hub.publish(LiveMessage{Type: LiveCapture, Event: &c.event})
	}
}

// Close drains the buffer and stops the worker.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	close(r.stopCh)
	<-r.stopped
	r.flush()
	return r.store.Close()
}

func (r *Recorder) run() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()
	reload := time.NewTicker(watchlistReload)
	defer reload.Stop()
	for {
		select {
		case <-ticker.C:
			r.flush()
		case <-reload.C:
			r.reloadWatchlist()
			r.reloadDetectorSettings()
		case <-r.stopCh:
			return
		}
	}
}

func (r *Recorder) flush() {
	r.mu.Lock()
	if len(r.buffer) == 0 {
		if r.dropped > 0 {
			dropped := r.dropped
			r.dropped = 0
			r.mu.Unlock()
			r.logger.Warn("hivetrace: captures dropped, queue full", "count", dropped)
			return
		}
		r.mu.Unlock()
		return
	}
	batch := r.buffer
	dropped := r.dropped
	r.buffer = make([]capture, 0, 128)
	r.dropped = 0
	r.mu.Unlock()

	if dropped > 0 {
		r.logger.Warn("hivetrace: captures dropped, queue full", "count", dropped)
	}

	events := make([]Event, 0, len(batch))
	// One tenant dominates a flush in practice, so the entity selection is
	// looked up once per team rather than once per event.
	entityCache := make(map[string]*[]string)
	for i := range batch {
		if ev, ok := r.process(&batch[i], entityCache); ok {
			events = append(events, ev)
			if r.hub.Watchers() > 0 {
				r.hub.PublishEvent(ev)
			}
		}
	}
	if len(events) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.store.InsertEvents(ctx, events); err != nil {
		r.logger.Warn("hivetrace: failed to store events", "error", err, "count", len(events))
		return
	}
	r.logger.Debug("hivetrace batch flushed", "count", len(events))
}

// process turns a raw capture into a stored event. It runs on the worker, so
// parsing, scanning and redaction cost the request nothing.
func (r *Recorder) process(c *capture, entityCache map[string]*[]string) (Event, bool) {
	ev := c.event

	_, flavor := apiFor(ev.Path)
	messages := hivestate.ParseMessages(c.requestBody, flavor)
	ev.SessionID = hivestate.DeriveSessionID(c.sessionHeader, messages)
	// A conversation id the client declared beats one derived from the prompt,
	// which shifts whenever the agent rewrites its own system or first turn.
	if c.sessionHeader == "" {
		if declared := clientSessionID(c.requestBody); declared != "" {
			ev.SessionID = declared
		}
	}
	if ev.SessionID == "" {
		// Nothing to anchor a conversation on — an embeddings-style call, or a
		// body that did not parse. It still gets recorded, as a session of one,
		// so the turn is not silently missing from the account.
		ev.SessionID = ev.RequestID
	}
	if !sampled(ev.SessionID, r.cfg.SampleRate) {
		return Event{}, false
	}

	requestText := newInputText(messages)
	responseText := extractResponseText(ev.API, c.responseBody)
	piiEntities := r.piiEntitiesFor(ev.TeamID, entityCache)

	calls := extractToolCalls(ev.API, c.responseBody)
	ev.Files = extractFiles(calls)
	ev.Tools = make([]ToolInvocation, 0, len(calls))
	for _, call := range calls {
		ev.Tools = append(ev.Tools, call.ToolInvocation)
	}

	ev.Agent, ev.AgentVia = fingerprintAgent(ev.ClientProduct, c.clientApp, declaredToolNames(c.requestBody), ev.Tools, c.upstream)

	// Resolved here rather than in the middleware: this runs on the ingestion
	// worker, where a slow resolver costs a little throughput instead of
	// delaying somebody's first token.
	ev.ClientHost = r.hosts.hostFor(ev.ClientIP)

	if ev.Status >= 400 || ev.Streaming {
		ev.ErrorMessage = upstreamErrorMessage(c.responseBody)
	}

	// Masked first: the task type does not depend on a key or an address, so
	// the classifier has no reason to see one.
	if r.tasks.pending(ev.SessionID) {
		if prompt := openingRequest(messages); prompt != "" {
			masked, _ := r.det.redact(prompt, piiEntities)
			if label, score, ok := r.tasks.classify(ev.SessionID, masked); ok {
				ev.TaskHint, ev.TaskHintScore = label, score
			}
		}
	}

	samples := 0
	if r.cfg.FindingSamples {
		samples = maxFindingSamples
	}
	ev.Findings = append(
		r.det.findings(requestText, OriginRequest, piiEntities, samples),
		r.det.findings(responseText, OriginResponse, piiEntities, samples)...,
	)

	if r.cfg.CaptureBodies {
		reqBody, respBody := requestText, responseText
		if r.cfg.ShouldRedact() {
			var reqRedacted, respRedacted []string
			reqBody, reqRedacted = r.det.redact(reqBody, piiEntities)
			respBody, respRedacted = r.det.redact(respBody, piiEntities)
			ev.Redactions = dedupe(append(reqRedacted, respRedacted...))
		}
		ev.RequestBody, _ = truncate(reqBody, r.cfg.MaxBodyBytes)
		var bodyCut bool
		ev.ResponseBody, bodyCut = truncate(respBody, r.cfg.MaxBodyBytes)
		ev.Truncated = c.responseCut || bodyCut
	} else {
		ev.Truncated = c.responseCut
	}

	if r.metrics != nil {
		r.metrics.RecordTraceEvent(ev.API)
		for _, f := range ev.Findings {
			r.metrics.RecordTraceFinding(f.Kind, f.Origin)
		}
	}
	return ev, true
}

// piiEntitiesFor resolves a tenant's PII entity selection. A lookup failure
// yields nil, which means "every detector applies" — over-reporting a finding
// is a better failure than silently reporting none.
func (r *Recorder) piiEntitiesFor(teamID string, cache map[string]*[]string) *[]string {
	if r.entities == nil || teamID == "" {
		return nil
	}
	if entities, ok := cache[teamID]; ok {
		return entities
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entities, err := r.entities.GetAnonymizationEntities(ctx, teamID)
	if err != nil {
		entities = nil
	}
	cache[teamID] = entities
	return entities
}

// sampled decides inclusion from the session id, so a sampled conversation is
// kept whole. Sampling per request would leave half a session in the store,
// and half a session describes nothing.
func sampled(sessionID string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	return float64(h.Sum32()%10000)/10000 < rate
}

func truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	return s[:max], true
}

func dedupe(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		if _, dup := seen[it]; dup {
			continue
		}
		seen[it] = struct{}{}
		out = append(out, it)
	}
	return out
}
