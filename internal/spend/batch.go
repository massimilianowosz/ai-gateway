package spend

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/webhook"
)

type eventEmitter interface {
	Emit(event string, data any)
}

// BatchWriter buffers spend records and flushes them periodically in batches.
// This reduces write amplification and DB contention at high throughput.
type BatchWriter struct {
	store    store.Store
	logger   *slog.Logger
	interval time.Duration
	emitter  eventEmitter

	mu      sync.Mutex
	buffer  []store.SpendRecord
	stopCh  chan struct{}
	stopped chan struct{}
}

// NewBatchWriter creates a batch writer that flushes every interval.
// If interval is 0, defaults to 5 seconds.
func NewBatchWriter(s store.Store, logger *slog.Logger, interval time.Duration, emitters ...eventEmitter) *BatchWriter {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var emitter eventEmitter
	if len(emitters) > 0 {
		emitter = emitters[0]
	}
	bw := &BatchWriter{
		store:    s,
		logger:   logger,
		interval: interval,
		emitter:  emitter,
		buffer:   make([]store.SpendRecord, 0, 128),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go bw.run()
	return bw
}

// Record enqueues a spend record for batch writing.
// This is non-blocking and safe for concurrent use.
func (bw *BatchWriter) Record(record store.SpendRecord) {
	// Nil-safe: callers hold this as a *BatchWriter and several record from a
	// detached goroutine, where a panic is not caught by the recovery
	// middleware — it takes the process down instead.
	if bw == nil {
		return
	}
	bw.mu.Lock()
	bw.buffer = append(bw.buffer, record)
	bw.mu.Unlock()
}

// RecordSync writes a spend record immediately. Use this for budgeted keys to
// keep admission checks close to the latest persisted spend.
func (bw *BatchWriter) RecordSync(ctx context.Context, record store.SpendRecord) error {
	if bw == nil {
		return nil
	}
	return bw.logRecord(ctx, record)
}

// Close flushes remaining records and stops the background goroutine.
func (bw *BatchWriter) Close() error {
	close(bw.stopCh)
	<-bw.stopped
	// Final flush
	bw.flush()
	return nil
}

func (bw *BatchWriter) run() {
	defer close(bw.stopped)
	ticker := time.NewTicker(bw.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			bw.flush()
		case <-bw.stopCh:
			return
		}
	}
}

func (bw *BatchWriter) flush() {
	bw.mu.Lock()
	if len(bw.buffer) == 0 {
		bw.mu.Unlock()
		return
	}
	records := bw.buffer
	bw.buffer = make([]store.SpendRecord, 0, 128)
	bw.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := range records {
		if err := bw.logRecord(ctx, records[i]); err != nil {
			bw.logger.Warn("batch writer: failed to log spend", "error", err, "record_id", records[i].ID)
		}
	}

	bw.logger.Debug("spend batch flushed", "count", len(records))
}

// spendTrackedPayload is the webhook body for spend_tracked events.
// It mirrors SpendRecord but explicitly includes key_hash for downstream integrations.
type spendTrackedPayload struct {
	ID               string  `json:"id"`
	KeyHash          string  `json:"key_hash"`
	KeyPrefix        string  `json:"key_prefix"`
	TeamID           string  `json:"team_id"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	DurationMs       int64   `json:"duration_ms"`
	Status           int     `json:"status"`
	CreatedAt        string  `json:"created_at"`
}

func (bw *BatchWriter) logRecord(ctx context.Context, record store.SpendRecord) error {
	if bw.store == nil {
		return nil
	}
	if err := bw.store.LogSpend(ctx, record); err != nil {
		return err
	}

	if bw.emitter != nil {
		bw.emitter.Emit(webhook.EventSpendTracked, spendTrackedPayload{
			ID:               record.ID,
			KeyHash:          record.KeyHash,
			KeyPrefix:        record.KeyPrefix,
			TeamID:           record.TeamID,
			Model:            record.Model,
			Provider:         record.Provider,
			PromptTokens:     record.PromptTokens,
			CompletionTokens: record.CompletionTokens,
			TotalTokens:      record.TotalTokens,
			Cost:             record.Cost,
			DurationMs:       record.Duration,
			Status:           record.Status,
			CreatedAt:        record.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
		bw.emitBudgetEvents(ctx, record)
	}
	return nil
}

func (bw *BatchWriter) emitBudgetEvents(ctx context.Context, record store.SpendRecord) {
	if record.Cost <= 0 {
		return
	}

	if record.KeyHash != "" {
		key, err := bw.store.GetKeyByHash(ctx, record.KeyHash)
		if err != nil {
			bw.logger.Warn("batch writer: failed to load key for budget event", "error", err, "key_prefix", record.KeyPrefix)
		} else if key != nil && crossedBudget(key.Budget, key.Spend, record.Cost) {
			bw.emitter.Emit(webhook.EventBudgetCrossed, map[string]any{
				"scope":      "key",
				"key_prefix": key.KeyPrefix,
				"budget":     key.Budget,
				"spend":      key.Spend,
			})
		}
	}

	if record.TeamID != "" {
		team, err := bw.store.GetTeam(ctx, record.TeamID)
		if err != nil {
			bw.logger.Warn("batch writer: failed to load team for budget event", "error", err, "team_id", record.TeamID)
		} else if team != nil && crossedBudget(team.Budget, team.Spend, record.Cost) {
			bw.emitter.Emit(webhook.EventBudgetCrossed, map[string]any{
				"scope":   "team",
				"team_id": team.ID,
				"budget":  team.Budget,
				"spend":   team.Spend,
			})
		}
	}
}

func crossedBudget(budget, spend, delta float64) bool {
	return budget > 0 && spend >= budget && spend-delta < budget
}
