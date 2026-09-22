package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateConversation persists a new conversation.
func (s *GormStore) CreateConversation(ctx context.Context, conversation *StoredConversation) error {
	if conversation == nil {
		return fmt.Errorf("store: conversation is required")
	}
	return s.db.WithContext(ctx).Create(conversation).Error
}

// GetConversation returns an unexpired conversation scoped to its owner. A nil
// result means it does not exist in that scope.
func (s *GormStore) GetConversation(ctx context.Context, id, ownerID string) (*StoredConversation, error) {
	var conversation StoredConversation
	err := s.db.WithContext(ctx).
		Where("id = ? AND owner_id = ? AND (expires_at IS NULL OR expires_at > ?)", id, ownerID, time.Now()).
		First(&conversation).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get conversation: %w", err)
	}
	return &conversation, nil
}

// UpdateConversationMetadata replaces a conversation's metadata document.
func (s *GormStore) UpdateConversationMetadata(ctx context.Context, id, ownerID, metadata string) error {
	result := s.db.WithContext(ctx).
		Model(&StoredConversation{}).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Update("metadata", metadata)
	if result.Error != nil {
		return fmt.Errorf("store: update conversation: %w", result.Error)
	}
	return nil
}

// DeleteConversation removes a conversation and everything in it.
func (s *GormStore) DeleteConversation(ctx context.Context, id, ownerID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conversation_id = ? AND owner_id = ?", id, ownerID).
			Delete(&StoredConversationItem{}).Error; err != nil {
			return fmt.Errorf("store: delete conversation items: %w", err)
		}
		if err := tx.Where("id = ? AND owner_id = ?", id, ownerID).
			Delete(&StoredConversation{}).Error; err != nil {
			return fmt.Errorf("store: delete conversation: %w", err)
		}
		return nil
	})
}

// AppendConversationItems adds items to the end of a conversation. The whole
// append runs in one transaction so a turn's items either all land or none do,
// and the parent conversation row is locked first so two concurrent appends
// cannot be handed the same position.
//
// The lock is the part that has to be explicit. Reading MAX(position) with a
// plain SELECT is not enough under Postgres READ COMMITTED: two appends to the
// same conversation both see the same maximum, both compute the same next
// position, and the turn ordering silently interleaves. SQLite needs nothing —
// a write transaction there already excludes other writers.
func (s *GormStore) AppendConversationItems(ctx context.Context, conversationID, ownerID string, payloads []string) ([]StoredConversationItem, error) {
	if len(payloads) == 0 {
		return nil, nil
	}

	items := make([]StoredConversationItem, 0, len(payloads))
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Name() == "postgres" {
			var parent StoredConversation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND owner_id = ?", conversationID, ownerID).
				First(&parent).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return fmt.Errorf("store: conversation %q not found", conversationID)
				}
				return fmt.Errorf("store: lock conversation: %w", err)
			}
		}

		var next int64
		if err := tx.Model(&StoredConversationItem{}).
			Where("conversation_id = ?", conversationID).
			Select("COALESCE(MAX(position), 0)").
			Scan(&next).Error; err != nil {
			return fmt.Errorf("store: read conversation length: %w", err)
		}

		now := time.Now()
		items = items[:0]
		for i, payload := range payloads {
			next++
			id, err := newConversationItemID()
			if err != nil {
				return fmt.Errorf("store: mint conversation item id: %w", err)
			}
			items = append(items, StoredConversationItem{
				ID:             id,
				ConversationID: conversationID,
				OwnerID:        ownerID,
				Position:       next,
				Payload:        payload,
				CreatedAt:      now.Add(time.Duration(i) * time.Nanosecond),
			})
		}
		if err := tx.Create(&items).Error; err != nil {
			return fmt.Errorf("store: append conversation items: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// newConversationItemID mints an item id.
//
// Randomness rather than a timestamp plus position: the position is unique only
// within its conversation, so deriving a global primary key from it collided
// whenever two conversations appended in the same nanosecond at congruent
// positions. This also matches the shape real Responses item ids have.
func newConversationItemID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(raw[:]), nil
}

// ListConversationItems returns a conversation's items in the order they were
// appended.
func (s *GormStore) ListConversationItems(ctx context.Context, conversationID, ownerID string) ([]StoredConversationItem, error) {
	var items []StoredConversationItem
	err := s.db.WithContext(ctx).
		Where("conversation_id = ? AND owner_id = ?", conversationID, ownerID).
		Order("position ASC").
		Find(&items).Error
	if err != nil {
		return nil, fmt.Errorf("store: list conversation items: %w", err)
	}
	return items, nil
}

// GetConversationItem returns one item, or nil when it is not in that
// conversation.
func (s *GormStore) GetConversationItem(ctx context.Context, conversationID, ownerID, itemID string) (*StoredConversationItem, error) {
	var item StoredConversationItem
	err := s.db.WithContext(ctx).
		Where("id = ? AND conversation_id = ? AND owner_id = ?", itemID, conversationID, ownerID).
		First(&item).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get conversation item: %w", err)
	}
	return &item, nil
}

// DeleteConversationItem removes one item, leaving the positions of the others
// untouched: they only have to stay ordered, not contiguous.
func (s *GormStore) DeleteConversationItem(ctx context.Context, conversationID, ownerID, itemID string) error {
	result := s.db.WithContext(ctx).
		Where("id = ? AND conversation_id = ? AND owner_id = ?", itemID, conversationID, ownerID).
		Delete(&StoredConversationItem{})
	if result.Error != nil {
		return fmt.Errorf("store: delete conversation item: %w", result.Error)
	}
	return nil
}

// PurgeExpiredConversations removes conversations past their retention along
// with their items. Rows with no expiry are kept: they predate the setting.
func (s *GormStore) PurgeExpiredConversations(ctx context.Context) (int64, error) {
	var removed int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var expired []string
		if err := tx.Model(&StoredConversation{}).
			Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now()).
			Pluck("id", &expired).Error; err != nil {
			return fmt.Errorf("store: find expired conversations: %w", err)
		}
		if len(expired) == 0 {
			return nil
		}
		if err := tx.Where("conversation_id IN ?", expired).
			Delete(&StoredConversationItem{}).Error; err != nil {
			return fmt.Errorf("store: purge conversation items: %w", err)
		}
		result := tx.Where("id IN ?", expired).Delete(&StoredConversation{})
		if result.Error != nil {
			return fmt.Errorf("store: purge expired conversations: %w", result.Error)
		}
		removed = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
