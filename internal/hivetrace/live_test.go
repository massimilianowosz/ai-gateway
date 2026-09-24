package hivetrace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHub_FansOutToEverySubscriber(t *testing.T) {
	hub := NewHub()
	a, closeA := hub.Subscribe(4)
	defer closeA()
	b, closeB := hub.Subscribe(4)
	defer closeB()

	assert.Equal(t, 2, hub.Watchers())
	hub.PublishEvent(Event{ID: "e1"})

	for _, ch := range []<-chan LiveMessage{a, b} {
		select {
		case msg := <-ch:
			assert.Equal(t, LiveEvent, msg.Type)
			require.NotNil(t, msg.Event)
			assert.Equal(t, "e1", msg.Event.ID)
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive the event")
		}
	}
}

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	hub := NewHub()
	ch, unsubscribe := hub.Subscribe(2)

	unsubscribe()
	assert.Equal(t, 0, hub.Watchers())

	_, open := <-ch
	assert.False(t, open, "unsubscribing closes the channel so the reader exits")

	hub.PublishEvent(Event{ID: "e1"})
	unsubscribe()
}

// A stalled browser must never slow the ingestion worker: a full buffer drops
// updates instead of blocking the publisher.
func TestHub_DropsWhenSubscriberIsFull(t *testing.T) {
	hub := NewHub()
	_, unsubscribe := hub.Subscribe(1)
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			hub.PublishEvent(Event{ID: "e"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}
}

func TestHub_NilIsSafe(t *testing.T) {
	var hub *Hub
	assert.Equal(t, 0, hub.Watchers())
	hub.PublishEvent(Event{})
	hub.PublishSession(SessionSummary{})

	ch, unsubscribe := hub.Subscribe(1)
	assert.Nil(t, ch)
	unsubscribe()
}

func TestHub_SessionMessagesCarryTheSummary(t *testing.T) {
	hub := NewHub()
	ch, unsubscribe := hub.Subscribe(2)
	defer unsubscribe()

	hub.PublishSession(SessionSummary{SessionID: "s1", Requests: 3})

	msg := <-ch
	assert.Equal(t, LiveSession, msg.Type)
	require.NotNil(t, msg.Session)
	assert.Equal(t, 3, msg.Session.Requests)
	assert.Nil(t, msg.Event)
}
