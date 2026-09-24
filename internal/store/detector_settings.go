package store

import (
	"context"
	"time"

	"gorm.io/gorm/clause"
)

// DetectorSetting overrides whether traffic analysis reports one detector.
// Only overrides are stored: a type with no row keeps its built-in default.
type DetectorSetting struct {
	Type      string    `json:"type" gorm:"primaryKey;column:type"`
	Enabled   bool      `json:"enabled" gorm:"column:enabled"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (DetectorSetting) TableName() string { return "gw_detector_settings" }

func (s *GormStore) ListDetectorSettings(ctx context.Context) ([]DetectorSetting, error) {
	var rows []DetectorSetting
	err := s.db.WithContext(ctx).Find(&rows).Error
	return rows, err
}

func (s *GormStore) SetDetectorEnabled(ctx context.Context, detector string, enabled bool) error {
	row := DetectorSetting{Type: detector, Enabled: enabled, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "type"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_at"}),
	}).Create(&row).Error
}
