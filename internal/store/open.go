package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// GormStore implements Store using GORM (supports SQLite + PostgreSQL).
type GormStore struct {
	db *gorm.DB
}

// Open creates a Store based on config. Defaults to SQLite if no driver specified.
func Open(cfg config.DatabaseConfig) (Store, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = "sqlite"
	}

	gormCfg := &gorm.Config{
		Logger: logger.Discard,
	}

	var dialector gorm.Dialector
	switch driver {
	case "sqlite":
		url := cfg.URL
		if url == "" {
			url = filepath.Join(defaultDataDir(), "ubiquum.db")
		}
		dialector = sqlite.Open(url + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	case "postgres", "postgresql":
		if cfg.URL == "" {
			return nil, fmt.Errorf("database.url is required for postgres driver")
		}
		dialector = postgres.Open(cfg.URL)
	default:
		return nil, fmt.Errorf("unsupported database driver: %q (use 'sqlite' or 'postgres')", driver)
	}

	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", driver, err)
	}

	// SQLite takes a single connection. It allows one writer at a time, and a
	// transaction that reads before it writes — as an append does, reading the
	// current position first — cannot upgrade its lock while another connection
	// holds one. busy_timeout does not help: retrying cannot resolve that, so
	// the write fails outright with SQLITE_BUSY. One connection turns the
	// contention into ordinary queueing.
	if driver == "sqlite" {
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(1)
	}

	// Connection pool settings for postgres
	if driver == "postgres" || driver == "postgresql" {
		sqlDB, _ := db.DB()
		poolSize := cfg.PoolSize
		if poolSize <= 0 {
			poolSize = 20
		}
		sqlDB.SetMaxOpenConns(poolSize)
		sqlDB.SetMaxIdleConns(poolSize / 2)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}

	return &GormStore{db: db}, nil
}

func (s *GormStore) Migrate(ctx context.Context) error {
	// Always auto-migrate gateway-specific tables.
	if err := s.db.WithContext(ctx).AutoMigrate(&SpendRecord{}, &User{}, &CacheMetric{}, &HiveStateMetric{}, &TenantSettings{}, &KeyRouteSettings{}, &FeedbackRecord{}, &ProviderFile{}, &StoredResponse{}, &StoredConversation{}, &StoredConversationItem{}, &KeyEvent{}, &SpendDailySummary{}, &TraceEvent{}, &TraceSession{}, &WatchlistTerm{}, &DetectorSetting{}); err != nil {
		return err
	}
	// For non-postgres (e.g. SQLite in tests), also create APIKey/Team tables
	// since there's no Alembic to manage them.
	if s.db.Name() != "postgres" {
		// security_events is the portal's table, created by its own migrations.
		// The gateway only writes to it, so it is created here — where there is
		// no portal — rather than migrated in Postgres, which would let the
		// gateway alter a schema it does not own.
		if err := s.db.WithContext(ctx).AutoMigrate(&APIKey{}, &Team{}, &SecurityEvent{}); err != nil {
			return err
		}
	}
	// The route revision was introduced after governed keys were already in
	// use. Backfill it from the atomically applied key pointer so an upgrade
	// does not reject every existing governed request until the next change.
	if s.db.Migrator().HasTable(&APIKey{}) {
		if err := s.db.WithContext(ctx).Exec(`
			UPDATE key_route_settings
			SET governance_revision = (
				SELECT governance_revision FROM api_keys_local
				WHERE api_keys_local.gateway_key_id = key_route_settings.key_id
			)
			WHERE governance_revision = 0
			AND EXISTS (
				SELECT 1 FROM api_keys_local
				WHERE api_keys_local.gateway_key_id = key_route_settings.key_id
				AND api_keys_local.governance_revision > 0
			)
		`).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *GormStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// GovernanceReadiness proves that this replica can read the shared applied
// pointers and the revision-stamped route projection. LIMIT 0 validates both
// connectivity and schema without scanning tenant data.
func (s *GormStore) GovernanceReadiness(ctx context.Context) error {
	for _, query := range []string{
		"SELECT governance_required, governance_revision, governance_digest FROM api_keys_local LIMIT 0",
		"SELECT governance_revision FROM key_route_settings LIMIT 0",
	} {
		rows, err := s.db.WithContext(ctx).Raw(query).Rows()
		if err != nil {
			return fmt.Errorf("store: governance readiness: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: governance readiness close: %w", err)
		}
	}
	return nil
}

// --- Keys ---

func (s *GormStore) CreateKey(ctx context.Context, key *APIKey) error {
	if key.ID == "" {
		key.ID = uuid.New().String()
	}
	query := s.db.WithContext(ctx)
	// These three are uuid columns on api_keys_local, and Go's zero value for
	// an unset string is "" rather than NULL. Postgres rejects "" as an
	// invalid uuid, where sqlite (used in tests) accepts it as text — so this
	// only ever broke against the real database. Omit whichever are unset so
	// the column stays NULL instead.
	var omit []string
	if key.TeamID == "" {
		omit = append(omit, "team_id")
	}
	if key.AgentID == "" {
		omit = append(omit, "agent_id")
	}
	if key.CreatedByUserID == "" {
		omit = append(omit, "created_by_user_id")
	}
	if len(omit) > 0 {
		query = query.Omit(omit...)
	}
	return query.Create(key).Error
}

func (s *GormStore) GetKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	var key APIKey
	err := s.db.WithContext(ctx).Where("gateway_key_id = ?", hash).First(&key).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get key: %w", err)
	}
	return &key, nil
}

func (s *GormStore) ListKeys(ctx context.Context, filter KeyFilter) ([]APIKey, error) {
	query := s.db.WithContext(ctx).Model(&APIKey{})

	if filter.TeamID != "" {
		query = query.Where("team_id = ?", filter.TeamID)
	}
	if filter.Active != nil {
		query = query.Where("active = ?", *filter.Active)
	}

	query = query.Order("created_at DESC")

	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}

	var keys []APIKey
	if err := query.Find(&keys).Error; err != nil {
		return nil, fmt.Errorf("store: list keys: %w", err)
	}
	return keys, nil
}

func (s *GormStore) UpdateKey(ctx context.Context, hash string, params UpdateKeyParams) error {
	updates := map[string]any{}

	if params.Name != nil {
		updates["key_name"] = *params.Name
	}
	if params.Budget != nil {
		updates["budget"] = *params.Budget
	}
	if params.RateLimit != nil {
		updates["rate_limit"] = *params.RateLimit
	}
	if params.Models != nil {
		updates["models"] = StringList(params.Models)
	}
	if params.DeniedModels != nil {
		updates["denied_models"] = StringList(params.DeniedModels)
	}
	if params.AllowedProviders != nil {
		updates["allowed_providers"] = StringList(params.AllowedProviders)
	}
	if params.DeniedProviders != nil {
		updates["denied_providers"] = StringList(params.DeniedProviders)
	}
	if params.Active != nil {
		updates["active"] = *params.Active
	}
	if params.ExpiresAt != nil {
		updates["expires_at"] = *params.ExpiresAt
	}
	if params.ContextInstructions != nil {
		updates["context_instructions"] = *params.ContextInstructions
	}
	if params.ContextInstructionsDigest != nil {
		updates["context_instructions_digest"] = *params.ContextInstructionsDigest
	}
	if params.RequireEUResidency != nil {
		updates["require_eu_residency"] = *params.RequireEUResidency
	}
	if params.GovernanceBlocked != nil {
		updates["governance_blocked"] = *params.GovernanceBlocked
	}
	if params.GovernanceBlockReason != nil {
		updates["governance_block_reason"] = *params.GovernanceBlockReason
	}

	if len(updates) == 0 {
		return nil
	}

	return s.db.WithContext(ctx).Model(&APIKey{}).Where("gateway_key_id = ?", hash).Updates(updates).Error
}

// ApplyGovernanceSnapshot serializes writers for a key and atomically replaces
// every snapshot-backed key value together with the applied pointer. Replays
// with the same revision/digest are successful no-ops; older or divergent
// deliveries cannot move the data plane backward.
func (s *GormStore) ApplyGovernanceSnapshot(ctx context.Context, params GovernanceApplyParams) (GovernanceApplyResult, error) {
	result := GovernanceApplyResult{Revision: params.Revision, Digest: params.Digest}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var key APIKey
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gateway_key_id = ?", params.KeyHash).First(&key).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrKeyNotFound
			}
			return err
		}
		if key.GovernanceAgentEnvironmentID != "" && key.GovernanceAgentEnvironmentID != params.AgentEnvironmentID {
			return ErrGovernanceRevisionConflict
		}
		if key.AgentID != "" && key.AgentID != params.AgentID {
			return ErrGovernanceRevisionConflict
		}
		if key.GovernanceRevision > params.Revision {
			return ErrStaleGovernanceRevision
		}
		if key.GovernanceRevision == params.Revision {
			if key.GovernanceDigest != params.Digest {
				return ErrGovernanceRevisionConflict
			}
			return nil
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"models": params.Models, "denied_models": params.DeniedModels,
			"allowed_providers": params.AllowedProviders, "denied_providers": params.DeniedProviders,
			"rate_limit": params.RateLimit, "require_eu_residency": params.RequireEUResidency,
			"governance_blocked": params.GovernanceBlocked, "governance_block_reason": params.GovernanceBlockReason,
			"context_instructions": params.ContextInstructions, "context_instructions_digest": params.ContextInstructionsDigest,
			"auto_recharge_enabled": params.AutoRechargeEnabled, "recharge_threshold": params.RechargeThreshold,
			"recharge_amount": params.RechargeAmount, "governance_revision": params.Revision,
			"governance_digest": params.Digest, "governance_schema_version": params.SchemaVersion,
			"governance_compiler_version":     params.CompilerVersion,
			"governance_agent_environment_id": params.AgentEnvironmentID,
			"governance_snapshot":             params.CanonicalSnapshot, "governance_applied_at": now,
		}
		allocation := key.WalletAllocatedBudget
		if params.WalletAllocatedBudget != nil {
			allocation = *params.WalletAllocatedBudget
			updates["wallet_allocated_budget"] = allocation
		}
		if params.Budget != nil {
			updates["budget"] = math.Min(*params.Budget, allocation)
		} else {
			updates["budget"] = allocation
		}
		res := tx.Model(&APIKey{}).Where("gateway_key_id = ? AND governance_revision < ?", params.KeyHash, params.Revision).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrGovernanceRevisionConflict
		}
		route := params.RouteSettings
		settings := KeyRouteSettings{
			KeyID: params.KeyHash, TeamID: key.TeamID, GovernanceRevision: params.Revision, Enabled: route.Enabled,
			Levels: route.Levels, ThinkingBudgets: route.ThinkingBudgets,
			FeedbackEnabled: route.FeedbackEnabled, CacheEnabled: route.CacheEnabled,
			CompressionEnabled:   route.CompressionEnabled,
			CacheDirectThreshold: route.CacheDirectThreshold, CacheReuseThreshold: route.CacheReuseThreshold,
			CacheTweakThreshold:  route.CacheTweakThreshold,
			CompressionThreshold: route.CompressionThreshold, CompressionTokenBudget: route.CompressionTokenBudget,
			CompressionStepWindow:      route.CompressionStepWindow,
			CompressionAppendOnlyState: route.CompressionAppendOnlyState,
			GuardrailOverride:          route.GuardrailOverride, LogRetentionDays: route.LogRetentionDays,
		}
		if err := tx.Save(&settings).Error; err != nil {
			return err
		}
		result.Applied = true
		return nil
	})
	return result, err
}

func (s *GormStore) DeleteKey(ctx context.Context, hash string) error {
	result := s.db.WithContext(ctx).Where("gateway_key_id = ?", hash).Delete(&APIKey{})
	if result.Error != nil {
		return fmt.Errorf("store: delete key: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// --- Spend ---

func (s *GormStore) LogSpend(ctx context.Context, record SpendRecord) error {
	if record.ID == "" {
		record.ID = generateID()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return fmt.Errorf("store: log spend: %w", err)
		}
		// Atomically increment key spend
		if record.KeyHash != "" {
			if err := tx.Model(&APIKey{}).Where("gateway_key_id = ?", record.KeyHash).
				Update("spend", gorm.Expr("spend + ?", record.Cost)).Error; err != nil {
				return fmt.Errorf("store: increment spend: %w", err)
			}
		}
		if record.TeamID != "" {
			if err := tx.Model(&Team{}).Where("gateway_team_id = ?", record.TeamID).
				Update("spend", gorm.Expr("spend + ?", record.Cost)).Error; err != nil {
				return fmt.Errorf("store: increment team spend: %w", err)
			}
		}
		return nil
	})
}

func (s *GormStore) GetSpend(ctx context.Context, filter SpendFilter) ([]SpendRecord, error) {
	query := s.db.WithContext(ctx).Model(&SpendRecord{})

	if filter.KeyHash != "" {
		query = query.Where("key_hash = ?", filter.KeyHash)
	}
	if filter.TeamID != "" {
		query = query.Where("team_id = ?", filter.TeamID)
	}
	if filter.Model != "" {
		query = query.Where("model = ?", filter.Model)
	}
	if filter.StartDate != nil {
		query = query.Where("created_at >= ?", *filter.StartDate)
	}
	if filter.EndDate != nil {
		query = query.Where("created_at <= ?", *filter.EndDate)
	}

	query = query.Order("created_at DESC")

	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}

	var records []SpendRecord
	if err := query.Find(&records).Error; err != nil {
		return nil, fmt.Errorf("store: get spend: %w", err)
	}
	return records, nil
}

// spendSummaryRow is the shared shape for the live gw_spend_records grouping
// and the gw_spend_daily_summaries grouping, carrying a raw duration total
// rather than a pre-averaged one so the two can be merged before the average
// is taken.
type spendSummaryRow struct {
	Model           string
	Provider        string
	KeyPrefix       string
	Requests        int
	Tokens          int
	Cost            float64
	TotalDurationMs int64
}

func (s *GormStore) GetSpendSummary(ctx context.Context, filter SpendFilter) ([]SpendSummary, error) {
	live := s.db.WithContext(ctx).Model(&SpendRecord{})
	if filter.KeyHash != "" {
		live = live.Where("key_hash = ?", filter.KeyHash)
	}
	if filter.TeamID != "" {
		live = live.Where("team_id = ?", filter.TeamID)
	}
	if filter.Model != "" {
		live = live.Where("model = ?", filter.Model)
	}
	if filter.StartDate != nil {
		live = live.Where("created_at >= ?", *filter.StartDate)
	}
	if filter.EndDate != nil {
		live = live.Where("created_at <= ?", *filter.EndDate)
	}

	var liveRows []spendSummaryRow
	if err := live.Select(
		"model, provider, key_prefix, COUNT(*) as requests, SUM(total_tokens) as tokens, SUM(cost) as cost, SUM(duration) as total_duration_ms",
	).Group("model, provider, key_prefix").Find(&liveRows).Error; err != nil {
		return nil, fmt.Errorf("store: get spend summary: %w", err)
	}

	// A retention purge folds expired rows into gw_spend_daily_summaries
	// before deleting them, so a summary spanning a purged range must read
	// that table too or its total quietly drops the moment the raw rows age
	// out from under it.
	aggregated := s.db.WithContext(ctx).Model(&SpendDailySummary{})
	if filter.KeyHash != "" {
		aggregated = aggregated.Where("key_hash = ?", filter.KeyHash)
	}
	if filter.TeamID != "" {
		aggregated = aggregated.Where("team_id = ?", filter.TeamID)
	}
	if filter.Model != "" {
		aggregated = aggregated.Where("model = ?", filter.Model)
	}
	if filter.StartDate != nil {
		aggregated = aggregated.Where("day >= ?", filter.StartDate.UTC().Truncate(24*time.Hour))
	}
	if filter.EndDate != nil {
		aggregated = aggregated.Where("day <= ?", *filter.EndDate)
	}

	var aggregatedRows []spendSummaryRow
	if err := aggregated.Select(
		"model, provider, key_prefix, SUM(requests) as requests, SUM(total_tokens) as tokens, SUM(total_cost) as cost, SUM(total_duration_ms) as total_duration_ms",
	).Group("model, provider, key_prefix").Find(&aggregatedRows).Error; err != nil {
		return nil, fmt.Errorf("store: get aggregated spend summary: %w", err)
	}

	type mergeKey struct{ model, provider, keyPrefix string }
	merged := make(map[mergeKey]*spendSummaryRow, len(liveRows)+len(aggregatedRows))
	for _, rows := range [][]spendSummaryRow{liveRows, aggregatedRows} {
		for _, r := range rows {
			k := mergeKey{r.Model, r.Provider, r.KeyPrefix}
			if existing, ok := merged[k]; ok {
				existing.Requests += r.Requests
				existing.Tokens += r.Tokens
				existing.Cost += r.Cost
				existing.TotalDurationMs += r.TotalDurationMs
				continue
			}
			row := r
			merged[k] = &row
		}
	}

	results := make([]SpendSummary, 0, len(merged))
	for _, m := range merged {
		var avg float64
		if m.Requests > 0 {
			avg = float64(m.TotalDurationMs) / float64(m.Requests)
		}
		results = append(results, SpendSummary{
			Model: m.Model, Provider: m.Provider, KeyPrefix: m.KeyPrefix,
			Requests: m.Requests, Tokens: m.Tokens, Cost: m.Cost, AvgMs: avg,
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Cost > results[j].Cost })
	return results, nil
}

func (s *GormStore) GetTotalSpend(ctx context.Context, keyHash string) (float64, error) {
	var key APIKey
	err := s.db.WithContext(ctx).Select("spend").Where("gateway_key_id = ?", keyHash).First(&key).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return 0, nil
		}
		return 0, err
	}
	return key.Spend, nil
}

// PurgeExpiredSpendRecords deletes gw_spend_records rows for keys that carry
// a request_logging retention override on key_route_settings, past that many
// days old. A key with no override (log_retention_days IS NULL) is never
// touched here — nil means keep forever, matching every other kind's "no
// enforced policy means unrestricted".
func (s *GormStore) PurgeExpiredSpendRecords(ctx context.Context) (int64, error) {
	var overrides []struct {
		KeyID            string
		LogRetentionDays int
	}
	if err := s.db.WithContext(ctx).
		Table("key_route_settings").
		Select("key_id, log_retention_days").
		Where("log_retention_days IS NOT NULL").
		Scan(&overrides).Error; err != nil {
		return 0, fmt.Errorf("store: list log retention overrides: %w", err)
	}

	var totalDeleted int64
	for _, o := range overrides {
		if o.LogRetentionDays <= 0 {
			continue
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -o.LogRetentionDays)
		deleted, err := s.purgeExpiredSpendForKey(ctx, o.KeyID, cutoff)
		if err != nil {
			return totalDeleted, fmt.Errorf("store: purge expired spend for key: %w", err)
		}
		totalDeleted += deleted
	}
	return totalDeleted, nil
}

// purgeExpiredSpendForKey folds every doomed row into SpendDailySummary and
// deletes it, both in one transaction: a crash between the two would either
// double-count a day's total on the next run (folded twice) or drop it
// entirely (deleted unfolded), and only committing them together rules both
// out.
func (s *GormStore) purgeExpiredSpendForKey(ctx context.Context, keyHash string, cutoff time.Time) (int64, error) {
	var deleted int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var doomed []SpendRecord
		if err := tx.Where("key_hash = ? AND created_at < ?", keyHash, cutoff).Find(&doomed).Error; err != nil {
			return err
		}
		if len(doomed) == 0 {
			return nil
		}

		type groupKey struct {
			day      time.Time
			model    string
			provider string
		}
		groups := make(map[groupKey]*SpendDailySummary, len(doomed))
		for _, r := range doomed {
			day := r.CreatedAt.UTC().Truncate(24 * time.Hour)
			gk := groupKey{day: day, model: r.Model, provider: r.Provider}
			g, ok := groups[gk]
			if !ok {
				g = &SpendDailySummary{
					Day: day, KeyHash: keyHash, KeyPrefix: r.KeyPrefix, TeamID: r.TeamID,
					Model: r.Model, Provider: r.Provider,
				}
				groups[gk] = g
			}
			g.Requests++
			g.TotalTokens += r.TotalTokens
			g.TotalCost += r.Cost
			g.TotalDurationMs += r.Duration
		}

		for _, g := range groups {
			q := tx
			// Row-lock on Postgres so a concurrent replica's purge cannot
			// read the same accumulator between this read and its own write
			// and lose an increment; sqlite (tests) has no such concurrent
			// writer to race against and does not support the clause.
			if tx.Name() == "postgres" {
				q = tx.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var existing SpendDailySummary
			err := q.Where("day = ? AND key_hash = ? AND model = ? AND provider = ?", g.Day, g.KeyHash, g.Model, g.Provider).
				First(&existing).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				g.ID = generateID()
				if err := tx.Create(g).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				existing.Requests += g.Requests
				existing.TotalTokens += g.TotalTokens
				existing.TotalCost += g.TotalCost
				existing.TotalDurationMs += g.TotalDurationMs
				if err := tx.Save(&existing).Error; err != nil {
					return err
				}
			}
		}

		res := tx.Where("key_hash = ? AND created_at < ?", keyHash, cutoff).Delete(&SpendRecord{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	return deleted, err
}

func (s *GormStore) LogCacheMetric(ctx context.Context, metric CacheMetric) error {
	return s.db.WithContext(ctx).Create(&metric).Error
}

func (s *GormStore) ListCacheMetrics(ctx context.Context, limit int) ([]CacheMetric, error) {
	if limit <= 0 {
		limit = 100
	}
	var metrics []CacheMetric
	err := s.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&metrics).Error
	return metrics, err
}

func (s *GormStore) DeleteCacheMetrics(ctx context.Context) error {
	return s.db.WithContext(ctx).Where("1=1").Delete(&CacheMetric{}).Error
}

func (s *GormStore) LogHiveStateMetric(ctx context.Context, metric HiveStateMetric) error {
	return s.db.WithContext(ctx).Create(&metric).Error
}

func (s *GormStore) ListHiveStateMetrics(ctx context.Context, limit int) ([]HiveStateMetric, error) {
	if limit <= 0 {
		limit = 100
	}
	var metrics []HiveStateMetric
	err := s.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&metrics).Error
	return metrics, err
}

// LogKeyEvent records one key lifecycle event. Failure to record is returned
// rather than swallowed: an audit trail that quietly loses entries is worse
// than none, because it invites trust it has not earned.
func (s *GormStore) LogKeyEvent(ctx context.Context, event KeyEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	return s.db.WithContext(ctx).Create(&event).Error
}

// LogSecurityEvent records one guardrail finding.
//
// It is not on the Store interface: the middleware asks for it through a
// one-method interface and skips the write if the store cannot do it, so a
// test double or an alternative store does not have to grow a method to
// stay compilable.
func (s *GormStore) LogSecurityEvent(ctx context.Context, event SecurityEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Create(&event).Error
}

// GetAgent reads one agent from the portal's registry.
//
// Like LogSecurityEvent it stays off the Store interface: auth asks for it
// through a one-method interface and skips the check if the store cannot
// answer, so a test double does not have to grow a method to compile.
func (s *GormStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	var agent Agent
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&agent).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &agent, nil
}

// ListKeyEvents returns the key audit trail, newest last, bounded by limit.
func (s *GormStore) ListKeyEvents(ctx context.Context, limit int) ([]KeyEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	var events []KeyEvent
	err := s.db.WithContext(ctx).Order("created_at asc, id asc").Limit(limit).Find(&events).Error
	return events, err
}

func (s *GormStore) GetTenantSettings(ctx context.Context, teamID string) (*TenantSettings, error) {
	var settings TenantSettings
	err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&settings).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get tenant settings: %w", err)
	}
	return &settings, nil
}

// GetAnonymizationEntities returns the explicitly configured PII entity list
// for a gateway team. The portal owns anonymization_configs and keys it by the
// tenant UUID, while authenticated gateway requests carry gateway_team_id, so
// the lookup deliberately joins through tenants instead of duplicating data.
//
// A nil result means no configuration row exists and preserves the historical
// behavior (all built-in PII detectors enabled). A non-nil empty slice means
// the tenant explicitly disabled every configurable PII entity.
func (s *GormStore) GetAnonymizationEntities(ctx context.Context, teamID string) (*[]string, error) {
	var row struct {
		EnabledEntities StringList `gorm:"column:enabled_entities"`
	}
	err := s.db.WithContext(ctx).
		Table("anonymization_configs AS ac").
		Select("ac.enabled_entities").
		Joins("JOIN tenants AS t ON t.id = ac.tenant_id").
		Where("t.gateway_team_id = ? OR CAST(t.id AS TEXT) = ?", teamID, teamID).
		Take(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get anonymization entities: %w", err)
	}

	entities := []string(row.EnabledEntities)
	return &entities, nil
}

func (s *GormStore) UpsertTenantSettings(ctx context.Context, settings *TenantSettings) error {
	return s.db.WithContext(ctx).Save(settings).Error
}

func (s *GormStore) GetKeyRouteSettings(ctx context.Context, keyID string) (*KeyRouteSettings, error) {
	var settings KeyRouteSettings
	err := s.db.WithContext(ctx).Where("key_id = ?", keyID).First(&settings).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get key route settings: %w", err)
	}
	return &settings, nil
}

// GetKeyGovernanceState reads the hot-path key and its route projection for
// one request. The revision stamped on both rows lets authentication reject a
// read that straddled an atomic snapshot apply instead of mixing revisions.
func (s *GormStore) GetKeyGovernanceState(ctx context.Context, keyID string) (*APIKey, *KeyRouteSettings, error) {
	key, err := s.GetKeyByHash(ctx, keyID)
	if err != nil || key == nil {
		return key, nil, err
	}
	route, err := s.GetKeyRouteSettings(ctx, keyID)
	return key, route, err
}

func (s *GormStore) UpsertKeyRouteSettings(ctx context.Context, settings *KeyRouteSettings) error {
	return s.db.WithContext(ctx).Save(settings).Error
}

func (s *GormStore) DeleteKeyRouteSettings(ctx context.Context, keyID string) error {
	return s.db.WithContext(ctx).Where("key_id = ?", keyID).Delete(&KeyRouteSettings{}).Error
}

func (s *GormStore) SetKeyFeedbackEnabled(ctx context.Context, keyID string, enabled bool) error {
	var settings KeyRouteSettings
	err := s.db.WithContext(ctx).Where("key_id = ?", keyID).First(&settings).Error
	if err != nil {
		// Create new record
		settings = KeyRouteSettings{KeyID: keyID, FeedbackEnabled: &enabled}
		return s.db.WithContext(ctx).Create(&settings).Error
	}
	settings.FeedbackEnabled = &enabled
	return s.db.WithContext(ctx).Save(&settings).Error
}

// StoreFeedback writes a feedback record, creating it the first time and
// updating it thereafter.
//
// Save rather than Create: the prompt files the row when the question is shown
// and the answer completes that same row later. Creating twice left an
// answerless duplicate for every cycle, and GetFeedbackFiredToday counts rows
// as prompts — so each completed cycle spent two of the day's allowance.
func (s *GormStore) StoreFeedback(ctx context.Context, record *FeedbackRecord) error {
	if record.ID == "" {
		record.ID = generateID()
	}
	return s.db.WithContext(ctx).Save(record).Error
}

func (s *GormStore) ListFeedback(ctx context.Context, teamID string, limit int) ([]FeedbackRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var records []FeedbackRecord
	q := s.db.WithContext(ctx).Order("created_at DESC").Limit(limit)
	if teamID != "" {
		q = q.Where("team_id = ?", teamID)
	}
	err := q.Find(&records).Error
	return records, err
}

func (s *GormStore) GetFeedbackFiredToday(ctx context.Context, keyHash string) ([]string, int, error) {
	today := startOfLocalDay()
	var records []FeedbackRecord
	err := s.db.WithContext(ctx).
		Where("key_hash = ? AND created_at >= ?", keyHash, today).
		Select("trigger_type").
		Find(&records).Error
	if err != nil {
		return nil, 0, err
	}
	triggers := make([]string, 0, len(records))
	for _, r := range records {
		triggers = append(triggers, r.TriggerType)
	}
	return triggers, len(records), nil
}

// startOfLocalDay returns midnight in the host's own zone.
//
// time.Now().Truncate(24*time.Hour) truncates the absolute instant, which is
// UTC midnight — on a UTC+2 host "today" then started at 02:00 local. The
// feedback session's own day boundary is local (it formats "2006-01-02"), so
// the two disagreed for the first hours of every local day and the hydrated
// counters described yesterday.
func startOfLocalDay() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// GetFeedbackSessionSignals queries gw_state_metrics to hydrate feedback session state
// across pods. Returns today's request count, recent compression ratios, route levels, and savings.
func (s *GormStore) GetFeedbackSessionSignals(ctx context.Context, keyPrefix string) (*FeedbackSessionSignals, error) {
	today := startOfLocalDay()

	var count int64
	if err := s.db.WithContext(ctx).Model(&HiveStateMetric{}).
		Where("key_prefix = ? AND created_at >= ?", keyPrefix, today).
		Count(&count).Error; err != nil {
		return nil, err
	}

	// Get last 10 metrics with state mode for ratios and route levels
	var recent []HiveStateMetric
	if err := s.db.WithContext(ctx).
		Where("key_prefix = ? AND created_at >= ? AND mode = 'state'", keyPrefix, today).
		Order("created_at DESC").
		Limit(10).
		Find(&recent).Error; err != nil {
		return nil, err
	}

	signals := &FeedbackSessionSignals{
		RequestCount: int(count),
	}
	for i := len(recent) - 1; i >= 0; i-- {
		r := recent[i]
		if r.Ratio > 0 {
			signals.Ratios = append(signals.Ratios, r.Ratio)
		}
		if r.RouteLevel != "" {
			signals.RouteLevels = append(signals.RouteLevels, r.RouteLevel)
		}
		signals.TotalSavings += r.RouteSavedCost
	}

	return signals, nil
}

func generateID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- Teams ---

func (s *GormStore) CreateTeam(ctx context.Context, team *Team) error {
	if team.InternalID == "" {
		team.InternalID = uuid.New().String()
	}
	if team.ID == "" {
		team.ID = generateID()
	}
	if team.SubscriptionTier == "" {
		team.SubscriptionTier = "free"
	}
	if team.SubscriptionStatus == "" {
		team.SubscriptionStatus = "active"
	}
	return s.db.WithContext(ctx).Create(team).Error
}

func (s *GormStore) GetTeam(ctx context.Context, id string) (*Team, error) {
	var team Team
	err := s.db.WithContext(ctx).Where("gateway_team_id = ?", id).First(&team).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get team: %w", err)
	}
	return &team, nil
}

func (s *GormStore) ListTeams(ctx context.Context, filter TeamFilter) ([]Team, error) {
	query := s.db.WithContext(ctx).Model(&Team{})
	query = query.Order("created_at DESC")
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}
	var teams []Team
	if err := query.Find(&teams).Error; err != nil {
		return nil, fmt.Errorf("store: list teams: %w", err)
	}
	return teams, nil
}

func (s *GormStore) UpdateTeam(ctx context.Context, id string, params UpdateTeamParams) error {
	updates := map[string]any{}
	if params.Name != nil {
		updates["name"] = *params.Name
	}
	if params.Budget != nil {
		updates["budget"] = *params.Budget
	}
	if params.AllowedModels != nil {
		updates["allowed_models"] = StringList(params.AllowedModels)
	}
	if params.GrantedModels != nil {
		updates["granted_models"] = StringList(params.GrantedModels)
	}
	if len(updates) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&Team{}).Where("gateway_team_id = ?", id).Updates(updates).Error
}

func (s *GormStore) DeleteTeam(ctx context.Context, id string) error {
	result := s.db.WithContext(ctx).Where("gateway_team_id = ?", id).Delete(&Team{})
	if result.Error != nil {
		return fmt.Errorf("store: delete team: %w", result.Error)
	}
	return nil
}

// --- Users ---

func (s *GormStore) CreateUser(ctx context.Context, user *User) error {
	if user.ID == "" {
		user.ID = generateID()
	}
	return s.db.WithContext(ctx).Create(user).Error
}

func (s *GormStore) GetUser(ctx context.Context, id string) (*User, error) {
	var user User
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get user: %w", err)
	}
	return &user, nil
}

func (s *GormStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var user User
	err := s.db.WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get user by email: %w", err)
	}
	return &user, nil
}

func (s *GormStore) ListUsers(ctx context.Context, filter UserFilter) ([]User, error) {
	query := s.db.WithContext(ctx).Model(&User{})
	if filter.TeamID != "" {
		query = query.Where("team_id = ?", filter.TeamID)
	}
	if filter.Active != nil {
		query = query.Where("active = ?", *filter.Active)
	}
	query = query.Order("created_at DESC")
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}
	var users []User
	if err := query.Find(&users).Error; err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return users, nil
}

func (s *GormStore) UpdateUser(ctx context.Context, id string, params UpdateUserParams) error {
	updates := map[string]any{}
	if params.Name != nil {
		updates["name"] = *params.Name
	}
	if params.TeamID != nil {
		updates["team_id"] = *params.TeamID
	}
	if params.Role != nil {
		updates["role"] = *params.Role
	}
	if params.Active != nil {
		updates["active"] = *params.Active
	}
	if len(updates) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&User{}).Where("id = ?", id).Updates(updates).Error
}

func (s *GormStore) DeleteUser(ctx context.Context, id string) error {
	result := s.db.WithContext(ctx).Where("id = ?", id).Delete(&User{})
	if result.Error != nil {
		return fmt.Errorf("store: delete user: %w", result.Error)
	}
	return nil
}

// defaultDataDir returns the directory where ubiquum stores its data files.
// Uses UBIQUUM_HOME env var or defaults to ~/.ubiquum/
func defaultDataDir() string {
	if dir := os.Getenv("UBIQUUM_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ubiquum")
}
