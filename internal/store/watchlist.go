package store

import (
	"context"
	"time"
)

// WatchlistTerm is one name, address or number an organisation asked to be
// told about. Unlike the PII entities, which are a fixed catalogue the
// operator switches on and off, these are declared per appliance: the gateway
// cannot guess that "Rossi" matters here and not there.
//
// The pattern is stored as the operator typed it, wildcards and all, and
// compiled at load time. Storing a compiled regexp would make the row
// unreadable in the console and unfixable by hand.
type WatchlistTerm struct {
	ID      uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Label   string `json:"label" gorm:"column:label;not null"`
	Pattern string `json:"pattern" gorm:"column:pattern;not null;uniqueIndex"`
	// Enabled lets an operator silence a term without losing how it was
	// written. Turning a noisy term off and on again is a normal thing to
	// want; retyping the pattern from memory is not.
	Enabled   bool      `json:"enabled" gorm:"column:enabled;default:true"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
}

func (WatchlistTerm) TableName() string { return "gw_watchlist_terms" }

// ListWatchlistTerms returns every term, enabled or not, oldest first so the
// console shows a stable order.
func (s *GormStore) ListWatchlistTerms(ctx context.Context) ([]WatchlistTerm, error) {
	var terms []WatchlistTerm
	err := s.db.WithContext(ctx).Order("id ASC").Find(&terms).Error
	return terms, err
}

func (s *GormStore) CreateWatchlistTerm(ctx context.Context, term *WatchlistTerm) error {
	term.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Create(term).Error
}

func (s *GormStore) SetWatchlistTermEnabled(ctx context.Context, id uint, enabled bool) error {
	return s.db.WithContext(ctx).Model(&WatchlistTerm{}).
		Where("id = ?", id).Update("enabled", enabled).Error
}

func (s *GormStore) DeleteWatchlistTerm(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Delete(&WatchlistTerm{}, id).Error
}
