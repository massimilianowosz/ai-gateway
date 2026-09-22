package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CreateProviderFile persists metadata for a provider-owned file. The file
// bytes are deliberately absent from ProviderFile.
func (s *GormStore) CreateProviderFile(ctx context.Context, file *ProviderFile) error {
	if file == nil {
		return fmt.Errorf("store: provider file is required")
	}
	return s.db.WithContext(ctx).Create(file).Error
}

// GetProviderFile returns an unexpired file mapping scoped to its tenant/key
// owner. A nil result means the file does not exist in that scope.
func (s *GormStore) GetProviderFile(ctx context.Context, id, ownerID string) (*ProviderFile, error) {
	var file ProviderFile
	err := s.db.WithContext(ctx).
		Where("id = ? AND owner_id = ? AND (expires_at IS NULL OR expires_at > ?)", id, ownerID, time.Now()).
		First(&file).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get provider file: %w", err)
	}
	return &file, nil
}

// ListProviderFiles returns unexpired file mappings for one owner using the
// OpenAI Files API ordering and cursor conventions.
func (s *GormStore) ListProviderFiles(ctx context.Context, ownerID string, filter ProviderFileFilter) ([]ProviderFile, error) {
	query := s.db.WithContext(ctx).
		Model(&ProviderFile{}).
		Where("owner_id = ? AND (expires_at IS NULL OR expires_at > ?)", ownerID, time.Now())

	if filter.Purpose != "" {
		query = query.Where("purpose = ?", filter.Purpose)
	}

	order := strings.ToLower(filter.Order)
	if order != "asc" {
		order = "desc"
	}

	if filter.After != "" {
		var cursor ProviderFile
		err := s.db.WithContext(ctx).
			Where("id = ? AND owner_id = ?", filter.After, ownerID).
			First(&cursor).Error
		if err == nil {
			operator := "<"
			if order == "asc" {
				operator = ">"
			}
			query = query.Where(
				fmt.Sprintf("(created_at %s ? OR (created_at = ? AND id %s ?))", operator, operator),
				cursor.CreatedAt,
				cursor.CreatedAt,
				cursor.ID,
			)
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("store: resolve provider file cursor: %w", err)
		}
	}

	query = query.Order("created_at " + order).Order("id " + order)
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}

	var files []ProviderFile
	if err := query.Find(&files).Error; err != nil {
		return nil, fmt.Errorf("store: list provider files: %w", err)
	}
	return files, nil
}

// DeleteProviderFile removes a tenant-scoped provider-file mapping.
func (s *GormStore) DeleteProviderFile(ctx context.Context, id, ownerID string) error {
	result := s.db.WithContext(ctx).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Delete(&ProviderFile{})
	if result.Error != nil {
		return fmt.Errorf("store: delete provider file: %w", result.Error)
	}
	return nil
}
