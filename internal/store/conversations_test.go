package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestConversation(id string, expiresAt *time.Time) *StoredConversation {
	return &StoredConversation{
		ID: id, OwnerID: "key:owner-1", Metadata: `{"topic":"support"}`,
		CreatedAt: time.Now(), ExpiresAt: expiresAt,
	}
}

func TestStore_Conversation_Lifecycle(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_1", nil)))

	got, err := s.GetConversation(ctx, "conv_1", "key:owner-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	other, err := s.GetConversation(ctx, "conv_1", "key:owner-2")
	require.NoError(t, err)
	assert.Nil(t, other, "conversations are scoped to their owner")

	require.NoError(t, s.UpdateConversationMetadata(ctx, "conv_1", "key:owner-1", `{"topic":"billing"}`))
	got, err = s.GetConversation(ctx, "conv_1", "key:owner-1")
	require.NoError(t, err)
	assert.Equal(t, `{"topic":"billing"}`, got.Metadata)

	require.NoError(t, s.DeleteConversation(ctx, "conv_1", "key:owner-1"))
	gone, err := s.GetConversation(ctx, "conv_1", "key:owner-1")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestStore_Conversation_ItemsKeepTheirOrder(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()
	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_items", nil)))

	first, err := s.AppendConversationItems(ctx, "conv_items", "key:owner-1", []string{`{"n":1}`, `{"n":2}`})
	require.NoError(t, err)
	require.Len(t, first, 2)

	// A second append continues after the first, rather than restarting.
	second, err := s.AppendConversationItems(ctx, "conv_items", "key:owner-1", []string{`{"n":3}`})
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Greater(t, second[0].Position, first[1].Position)

	items, err := s.ListConversationItems(ctx, "conv_items", "key:owner-1")
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, `{"n":1}`, items[0].Payload)
	assert.Equal(t, `{"n":3}`, items[2].Payload)

	item, err := s.GetConversationItem(ctx, "conv_items", "key:owner-1", items[1].ID)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, `{"n":2}`, item.Payload)

	require.NoError(t, s.DeleteConversationItem(ctx, "conv_items", "key:owner-1", items[1].ID))
	remaining, err := s.ListConversationItems(ctx, "conv_items", "key:owner-1")
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	assert.Equal(t, `{"n":1}`, remaining[0].Payload)
	assert.Equal(t, `{"n":3}`, remaining[1].Payload)
}

func TestStore_Conversation_DeleteRemovesItsItems(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()
	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_del", nil)))
	_, err := s.AppendConversationItems(ctx, "conv_del", "key:owner-1", []string{`{"n":1}`})
	require.NoError(t, err)

	require.NoError(t, s.DeleteConversation(ctx, "conv_del", "key:owner-1"))

	items, err := s.ListConversationItems(ctx, "conv_del", "key:owner-1")
	require.NoError(t, err)
	assert.Empty(t, items, "deleting a conversation must not leave its items behind")
}

func TestStore_Conversation_PurgeExpired(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_old", &past)))
	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_live", &future)))
	require.NoError(t, s.CreateConversation(ctx, newTestConversation("conv_legacy", nil)))
	_, err := s.AppendConversationItems(ctx, "conv_old", "key:owner-1", []string{`{"n":1}`})
	require.NoError(t, err)

	removed, err := s.PurgeExpiredConversations(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	orphans, err := s.ListConversationItems(ctx, "conv_old", "key:owner-1")
	require.NoError(t, err)
	assert.Empty(t, orphans, "a purge must take the items with the conversation")

	live, err := s.GetConversation(ctx, "conv_live", "key:owner-1")
	require.NoError(t, err)
	assert.NotNil(t, live)

	legacy, err := s.GetConversation(ctx, "conv_legacy", "key:owner-1")
	require.NoError(t, err)
	assert.NotNil(t, legacy)
}

// The old id scheme was msg_<unixnano><position%10000>: position is unique only
// within a conversation, so two conversations appending in the same nanosecond
// at congruent positions produced the same global primary key and the whole
// append failed.
func TestStore_Conversation_ItemIDsAreGloballyUnique(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	seen := map[string]string{}
	for c := 0; c < 8; c++ {
		id := fmt.Sprintf("conv_%d", c)
		require.NoError(t, s.CreateConversation(ctx, &StoredConversation{
			ID: id, OwnerID: "key:owner-1", CreatedAt: time.Now(),
		}))
		payloads := make([]string, 0, 16)
		for i := 0; i < 16; i++ {
			payloads = append(payloads, fmt.Sprintf(`{"type":"message","role":"user","content":"c%d-i%d"}`, c, i))
		}
		items, err := s.AppendConversationItems(ctx, id, "key:owner-1", payloads)
		require.NoError(t, err)
		require.Len(t, items, 16)
		for _, item := range items {
			if prev, dup := seen[item.ID]; dup {
				t.Fatalf("item id %s reused across %s and %s", item.ID, prev, id)
			}
			seen[item.ID] = id
		}
	}
	assert.Len(t, seen, 8*16)
}

// Concurrent appends to one conversation must each get their own positions, and
// every item must survive. On SQLite the write transaction already serialises
// them; on Postgres the parent row lock does.
func TestStore_Conversation_ConcurrentAppendsKeepEveryItem(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateConversation(ctx, &StoredConversation{
		ID: "conv_race", OwnerID: "key:owner-1", CreatedAt: time.Now(),
	}))

	const writers, perWriter = 6, 4
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			payloads := make([]string, 0, perWriter)
			for i := 0; i < perWriter; i++ {
				payloads = append(payloads, fmt.Sprintf(`{"type":"message","role":"user","content":"w%d-i%d"}`, w, i))
			}
			if _, err := s.AppendConversationItems(ctx, "conv_race", "key:owner-1", payloads); err != nil {
				errs <- err
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append failed: %v", err)
	}

	items, err := s.ListConversationItems(ctx, "conv_race", "key:owner-1")
	require.NoError(t, err)
	require.Len(t, items, writers*perWriter, "an append was lost")

	positions := map[int64]bool{}
	for _, item := range items {
		if positions[item.Position] {
			t.Fatalf("position %d was handed out twice", item.Position)
		}
		positions[item.Position] = true
	}
}
