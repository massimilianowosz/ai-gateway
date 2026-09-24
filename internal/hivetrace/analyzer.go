package hivetrace

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Analyzer recomputes session summaries from stored events.
//
// Summaries are derived state, rebuilt whole from the events of a session
// rather than updated incrementally. That costs a read per changed session and
// buys idempotence: a replayed batch, a late flush or a restart mid-pass all
// converge on the same answer instead of double-counting.
type Analyzer struct {
	store  TrafficStore
	hub    *Hub
	logger *slog.Logger
	prices PriceLookup
	// lookback extends each pass back beyond the last one, so an event whose
	// flush landed after a pass had already read its session is still picked
	// up on the next.
	lookback time.Duration
	lastRun  time.Time
}

// NewAnalyzer creates an analyzer over a traffic store. hub is optional and
// receives each recomputed summary.
func NewAnalyzer(ts TrafficStore, interval time.Duration, hub *Hub, logger *slog.Logger) *Analyzer {
	return &Analyzer{store: ts, hub: hub, logger: logger, lookback: 2 * interval}
}

// WithPrices enables model tiers and fit flags.
func (a *Analyzer) WithPrices(p PriceLookup) *Analyzer {
	a.prices = p
	return a
}

// Run recomputes every session touched since the previous pass.
func (a *Analyzer) Run(ctx context.Context) error {
	since := time.Now().UTC().Add(-a.lookback)
	if !a.lastRun.IsZero() && a.lastRun.Before(since) {
		since = a.lastRun.Add(-a.lookback)
	}
	start := time.Now().UTC()

	ids, err := a.store.SessionIDsSince(ctx, since, 0)
	if err != nil {
		return err
	}
	for _, id := range ids {
		events, err := a.store.ListEvents(ctx, Filter{SessionID: id})
		if err != nil {
			a.logger.Warn("hivetrace: failed to read session events", "error", err, "session_id", id)
			continue
		}
		summary := Summarize(id, events)
		if summary == nil {
			continue
		}
		assessModelFit(summary, a.prices)
		if err := a.store.UpsertSession(ctx, summary); err != nil {
			a.logger.Warn("hivetrace: failed to store session summary", "error", err, "session_id", id)
			continue
		}
		if a.hub.Watchers() > 0 {
			a.hub.PublishSession(*summary)
		}
	}

	a.lastRun = start
	a.logger.Debug("hivetrace sessions analyzed", "count", len(ids), "took_ms", time.Since(start).Milliseconds())
	return nil
}

// Summarize derives a session's account from its events. Events are expected
// oldest-first, as the store returns them.
func Summarize(sessionID string, events []Event) *SessionSummary {
	if len(events) == 0 {
		return nil
	}

	s := &SessionSummary{SessionID: sessionID, Requests: len(events)}

	models := newOrderedSet()
	providers := newOrderedSet()
	filesRead := newOrderedSet()
	filesWritten := newOrderedSet()
	tools := map[string]*ToolUsage{}
	var toolOrder []string
	servers := map[string]int{}
	var serverOrder []string
	findings := map[string]*FindingUsage{}
	var findingOrder []string
	latencies := make([]int64, 0, len(events))
	var hint string
	served := map[string]int{}

	for i := range events {
		e := &events[i]
		if hint == "" {
			hint = e.TaskHint
		}

		// Identity is taken from the first event: a session belongs to one key
		// by construction, since the key is part of what scopes it.
		if i == 0 {
			s.TeamID, s.KeyHash, s.KeyPrefix = e.TeamID, e.KeyHash, e.KeyPrefix
			s.UserID, s.AgentID = e.UserID, e.AgentID
			s.ClientProduct = e.ClientProduct
			s.StartedAt = e.CreatedAt
		}
		// The agent is the exception: a session can open with a turn that named
		// nothing and only later declare a toolset, so take the best evidence
		// any turn produced rather than whatever the first one happened to hold.
		if betterAgentEvidence(e.AgentVia, s.AgentVia) {
			s.Agent, s.AgentVia = e.Agent, e.AgentVia
		}
		// One flat-billed turn makes the session total misleading, so the mode is
		// remembered as soon as any turn reports it.
		if s.BillingMode == "" || e.BillingMode == "flat" {
			if e.BillingMode != "" {
				s.BillingMode = e.BillingMode
			}
		}
		// The caller's address is taken from the latest turn rather than the
		// first: a laptop that moved networks mid-session is reported where it
		// is now, not where it started.
		if e.ClientIP != "" {
			s.ClientIP = e.ClientIP
		}
		// The name is kept separately, and only when there is one. A resolver
		// that failed on the most recent turn must not erase a name the same
		// session already produced — an intermittent lookup would otherwise
		// make the caller flicker between a name and an address.
		if e.ClientHost != "" {
			s.ClientHost = e.ClientHost
		}
		if e.CreatedAt.Before(s.StartedAt) {
			s.StartedAt = e.CreatedAt
		}
		if e.CreatedAt.After(s.EndedAt) {
			s.EndedAt = e.CreatedAt
		}
		if e.Status >= 400 || e.ErrorMessage != "" {
			s.Errors++
		}

		models.add(e.Model)
		providers.add(e.Provider)
		if e.Model != "" && e.Status < 400 && e.ErrorMessage == "" {
			served[e.Model]++
		}

		s.PromptTokens += e.PromptTokens
		s.CompletionTokens += e.CompletionTokens
		s.TotalTokens += e.TotalTokens
		s.CachedPromptTokens += e.CachedPromptTokens
		s.ReasoningTokens += e.ReasoningTokens
		s.Cost += e.Cost
		latencies = append(latencies, e.DurationMs)

		for _, t := range e.Tools {
			s.ToolCalls++
			key := t.Source + "\x00" + t.Server + "\x00" + t.Tool
			u, ok := tools[key]
			if !ok {
				u = &ToolUsage{Tool: t.Tool, Server: t.Server, Source: t.Source}
				tools[key] = u
				toolOrder = append(toolOrder, key)
			}
			u.Calls++
			if t.Server != "" {
				if _, seen := servers[t.Server]; !seen {
					serverOrder = append(serverOrder, t.Server)
				}
				servers[t.Server]++
			}
		}

		for _, f := range e.Files {
			switch f.Operation {
			case FileOpRead, FileOpSearch:
				filesRead.add(f.Path)
			case FileOpWrite:
				filesWritten.add(f.Path)
			}
		}

		for _, f := range e.Findings {
			key := f.Kind + "\x00" + f.Type + "\x00" + f.Origin
			u, ok := findings[key]
			if !ok {
				u = &FindingUsage{Kind: f.Kind, Type: f.Type, Origin: f.Origin}
				findings[key] = u
				findingOrder = append(findingOrder, key)
			}
			u.Occurrences += f.Occurrences
			// Samples are merged rather than summed, and stay bounded: a
			// session of a hundred turns must not accumulate three values per
			// turn into a three-hundred-entry list of somebody's data.
			for _, v := range f.Samples {
				if len(u.Samples) >= maxFindingSamples {
					break
				}
				if !contains(u.Samples, v) {
					u.Samples = append(u.Samples, v)
				}
			}
		}
	}

	s.Models = models.items
	s.Providers = providers.items
	// Ties go to the model seen first, so the pick is stable across passes.
	for _, m := range s.Models {
		if served[m] > served[s.PrimaryModel] {
			s.PrimaryModel = m
		}
	}
	s.FilesRead = filesRead.items
	s.FilesWritten = filesWritten.items

	for _, k := range toolOrder {
		s.Tools = append(s.Tools, *tools[k])
	}
	for _, name := range serverOrder {
		s.MCPServers = append(s.MCPServers, ServerUsage{Server: name, Calls: servers[name]})
	}
	for _, k := range findingOrder {
		s.Findings = append(s.Findings, *findings[k])
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	s.LatencyP50Ms = percentile(latencies, 0.50)
	s.LatencyP95Ms = percentile(latencies, 0.95)
	s.TaskType, s.TaskVia = classifyTask(s)
	// Evidence first; the model fills gaps, and is trusted over the files only
	// on debugging, which a list of edited files cannot tell apart from coding.
	if hint != "" && (s.TaskType == "" || (hint == TaskDebugging && s.TaskType == TaskCoding)) {
		s.TaskType, s.TaskVia = hint, TaskViaModel
	}
	s.Difficulty = measureDifficulty(s)
	s.UpdatedAt = time.Now().UTC()
	return s
}

// percentile reads off a sorted slice using nearest-rank, which for the small
// samples a session produces is the only definition that returns a value that
// actually occurred.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted))*p+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// orderedSet keeps insertion order so a summary lists models and files in the
// order the session touched them, not in map order.
type orderedSet struct {
	seen  map[string]struct{}
	items []string
}

func newOrderedSet() *orderedSet {
	return &orderedSet{seen: map[string]struct{}{}}
}

func (s *orderedSet) add(v string) {
	if v == "" {
		return
	}
	if _, dup := s.seen[v]; dup {
		return
	}
	s.seen[v] = struct{}{}
	s.items = append(s.items, v)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
