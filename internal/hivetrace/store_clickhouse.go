package hivetrace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// clickHouseStore keeps traces in ClickHouse over its HTTP interface.
//
// Plain net/http rather than a driver, the same choice the Qdrant cache store
// made: the surface used here is three statements, and a database driver in
// go.mod is a dependency the appliance build would carry for a backend most
// deployments never turn on.
//
// Retention is a table-level TTL rather than a delete pass, so ClickHouse
// expires data during merges and Purge has nothing to do.
type clickHouseStore struct {
	endpoint string
	database string
	client   *http.Client
}

const (
	clickHouseEventsTable   = "gw_trace_events"
	clickHouseSessionsTable = "gw_trace_sessions"
)

// NewClickHouseStore connects to ClickHouse and creates the trace tables.
// dsn is the HTTP endpoint, optionally with credentials and a database path:
// http://user:pass@clickhouse:8123/ubiquum.
func NewClickHouseStore(dsn string, retention time.Duration) (TrafficStore, error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return nil, fmt.Errorf("hivetrace: invalid clickhouse_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("hivetrace: clickhouse_url must be http or https, got %q", u.Scheme)
	}

	database := strings.Trim(u.Path, "/")
	if database == "" {
		database = "default"
	}
	u.Path = "/"

	s := &clickHouseStore{
		endpoint: u.String(),
		database: database,
		client:   &http.Client{Timeout: 30 * time.Second},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.migrate(ctx, retention); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *clickHouseStore) migrate(ctx context.Context, retention time.Duration) error {
	ttlDays := int(retention.Hours() / 24)
	if ttlDays < 1 {
		ttlDays = 1
	}

	stmts := []string{
		fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, s.database),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
			id String,
			request_id String,
			session_id String,
			team_id String,
			key_hash String,
			key_prefix String,
			user_id String,
			agent_id String,
			api LowCardinality(String),
			path String,
			model LowCardinality(String),
			provider LowCardinality(String),
			client_product LowCardinality(String),
			client_version String,
			agent LowCardinality(String),
			agent_via LowCardinality(String),
			client_ip String,
			client_host String,
			billing_mode LowCardinality(String),
			status Int32,
			streaming UInt8,
			started_at DateTime64(3),
			duration_ms Int64,
			ttfb_ms Int64,
			prompt_tokens Int32,
			completion_tokens Int32,
			total_tokens Int32,
			cached_prompt_tokens Int32,
			reasoning_tokens Int32,
			cost Float64,
			request_body String,
			response_body String,
			truncated UInt8,
			redactions Array(String),
			tools String,
			files String,
			findings String,
			finding_count Int32,
			error_message String,
			task_hint LowCardinality(String),
			task_hint_score Float32,
			created_at DateTime64(3)
		) ENGINE = MergeTree()
		PARTITION BY toYYYYMM(created_at)
		ORDER BY (session_id, created_at, id)
		TTL toDateTime(created_at) + INTERVAL %d DAY`, s.database, clickHouseEventsTable, ttlDays),
		fmt.Sprintf(`ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS task_hint LowCardinality(String), ADD COLUMN IF NOT EXISTS task_hint_score Float32`,
			s.database, clickHouseEventsTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
			session_id String,
			team_id String,
			key_hash String,
			key_prefix String,
			user_id String,
			agent_id String,
			client_product LowCardinality(String),
			agent LowCardinality(String),
			agent_via LowCardinality(String),
			client_ip String,
			client_host String,
			billing_mode LowCardinality(String),
			started_at DateTime64(3),
			ended_at DateTime64(3),
			requests Int32,
			errors Int32,
			models Array(String),
			providers Array(String),
			prompt_tokens Int32,
			completion_tokens Int32,
			total_tokens Int32,
			cached_prompt_tokens Int32,
			reasoning_tokens Int32,
			cost Float64,
			latency_p50_ms Int64,
			latency_p95_ms Int64,
			tool_calls Int32,
			tools String,
			mcp_servers String,
			files_read Array(String),
			files_written Array(String),
			findings String,
			finding_count Int32,
			task_type LowCardinality(String),
			task_via LowCardinality(String),
			difficulty Int8,
			primary_model String,
			model_tier LowCardinality(String),
			model_fit LowCardinality(String),
			updated_at DateTime64(3)
		) ENGINE = ReplacingMergeTree(updated_at)
		ORDER BY session_id
		TTL toDateTime(ended_at) + INTERVAL %d DAY`, s.database, clickHouseSessionsTable, ttlDays),
		// CREATE IF NOT EXISTS leaves an older table as it was.
		fmt.Sprintf(`ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS task_type LowCardinality(String), ADD COLUMN IF NOT EXISTS task_via LowCardinality(String), ADD COLUMN IF NOT EXISTS difficulty Int8, ADD COLUMN IF NOT EXISTS primary_model String, ADD COLUMN IF NOT EXISTS model_tier LowCardinality(String), ADD COLUMN IF NOT EXISTS model_fit LowCardinality(String)`,
			s.database, clickHouseSessionsTable),
	}

	for _, stmt := range stmts {
		if _, err := s.exec(ctx, stmt, nil); err != nil {
			return fmt.Errorf("hivetrace: clickhouse migrate: %w", err)
		}
	}
	return nil
}

// exec posts a statement, optionally with a body of rows to insert.
func (s *clickHouseStore) exec(ctx context.Context, query string, body io.Reader) ([]byte, error) {
	endpoint := s.endpoint + "?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, bytes.TrimSpace(payload))
	}
	return payload, nil
}

func (s *clickHouseStore) InsertEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		row, err := toClickHouseEvent(&events[i])
		if err != nil {
			return err
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	query := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", s.database, clickHouseEventsTable)
	_, err := s.exec(ctx, query, &buf)
	return err
}

func (s *clickHouseStore) ListEvents(ctx context.Context, filter Filter) ([]Event, error) {
	query := fmt.Sprintf("SELECT * FROM %s.%s%s ORDER BY created_at ASC%s FORMAT JSONEachRow",
		s.database, clickHouseEventsTable, clickHouseWhere(filter, "created_at", "model"), clickHouseLimit(filter))

	payload, err := s.exec(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	var out []Event
	if err := decodeJSONEachRow(payload, func(raw json.RawMessage) error {
		var row clickHouseEvent
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		out = append(out, row.toEvent())
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *clickHouseStore) SessionIDsSince(ctx context.Context, since time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := fmt.Sprintf("SELECT DISTINCT session_id FROM %s.%s WHERE created_at >= %s LIMIT %d FORMAT JSONEachRow",
		s.database, clickHouseEventsTable, clickHouseTime(since), limit)

	payload, err := s.exec(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := decodeJSONEachRow(payload, func(raw json.RawMessage) error {
		var row struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		out = append(out, row.SessionID)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *clickHouseStore) UpsertSession(ctx context.Context, summary *SessionSummary) error {
	if summary == nil || summary.SessionID == "" {
		return nil
	}
	summary.UpdatedAt = time.Now().UTC()
	row, err := toClickHouseSession(summary)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(row); err != nil {
		return err
	}
	// ReplacingMergeTree keyed on session_id collapses the previous version at
	// merge time, so an insert is the upsert.
	query := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", s.database, clickHouseSessionsTable)
	_, err = s.exec(ctx, query, &buf)
	return err
}

func (s *clickHouseStore) GetSession(ctx context.Context, sessionID string) (*SessionSummary, error) {
	sessions, err := s.ListSessions(ctx, Filter{SessionID: sessionID, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	return &sessions[0], nil
}

func (s *clickHouseStore) ListSessions(ctx context.Context, filter Filter) ([]SessionSummary, error) {
	// FINAL forces the merge of pending ReplacingMergeTree versions, so a
	// summary recomputed seconds ago is not read back as its previous version.
	//
	// Filtered and ordered by ended_at, not started_at: "since" asks when a
	// session was last seen, and a long session that began before the window
	// but is still going belongs in it as much as one that just started.
	query := fmt.Sprintf("SELECT * FROM %s.%s FINAL%s ORDER BY ended_at DESC%s FORMAT JSONEachRow",
		s.database, clickHouseSessionsTable, clickHouseWhere(filter, "ended_at", ""), clickHouseLimit(filter))

	payload, err := s.exec(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	var out []SessionSummary
	if err := decodeJSONEachRow(payload, func(raw json.RawMessage) error {
		var row clickHouseSession
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		out = append(out, row.toSummary())
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Purge is a no-op: retention is enforced by the table TTL, which expires rows
// during merges without a scan.
func (s *clickHouseStore) Purge(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *clickHouseStore) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

func clickHouseWhere(f Filter, timeColumn, modelColumn string) string {
	var conds []string
	add := func(column, value string) {
		if value != "" {
			conds = append(conds, column+" = "+quoteClickHouse(value))
		}
	}
	add("session_id", f.SessionID)
	add("team_id", f.TeamID)
	add("key_hash", f.KeyHash)
	add("user_id", f.UserID)
	add("agent_id", f.AgentID)
	if modelColumn != "" {
		add(modelColumn, f.Model)
	}
	if !f.Since.IsZero() {
		conds = append(conds, timeColumn+" >= "+clickHouseTime(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, timeColumn+" <= "+clickHouseTime(f.Until))
	}
	if f.HasFindings {
		conds = append(conds, "finding_count > 0")
	}
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func clickHouseLimit(f Filter) string {
	if f.Limit <= 0 {
		return ""
	}
	out := " LIMIT " + strconv.Itoa(f.Limit)
	if f.Offset > 0 {
		out += " OFFSET " + strconv.Itoa(f.Offset)
	}
	return out
}

func clickHouseTime(t time.Time) string {
	return "toDateTime64(" + quoteClickHouse(t.UTC().Format("2006-01-02 15:04:05.000")) + ", 3)"
}

// quoteClickHouse renders a SQL string literal. Filter values reach here from
// admin query parameters, so escaping is the boundary that keeps a crafted
// team_id from becoming a second statement.
func quoteClickHouse(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0:
			// A NUL would terminate the literal early in some clients; drop it
			// rather than pass it through.
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func decodeJSONEachRow(payload []byte, fn func(json.RawMessage) error) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
}
