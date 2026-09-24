package hivetrace

import (
	"sync"
)

// Message kinds a watcher receives.
//
// A turn arrives twice on purpose. LiveCapture fires the instant the response
// finished, carrying timing, model, tokens and cost but nothing extracted yet;
// LiveEvent follows once the worker has parsed it, adding session, tools,
// files and findings. A console shows the row immediately and fills it in,
// rather than waiting a flush interval to show anything at all.
const (
	LiveCapture = "capture"
	LiveEvent   = "event"
	LiveSession = "session"
)

// LiveMessage is one update pushed to a connected console.
type LiveMessage struct {
	Type string `json:"type"`
	// Event is set on a captured turn, once the worker has extracted tools,
	// files and findings from it.
	Event *Event `json:"event,omitempty"`
	// Session is set when the analyzer has recomputed a session's account.
	Session *SessionSummary `json:"session,omitempty"`
}

// Hub fans trace activity out to connected watchers.
//
// Delivery is best-effort by design. A browser that stalls must not slow the
// ingestion worker down, so a subscriber whose buffer is full misses updates
// rather than applying back-pressure — it reloads the list and is current
// again. Observability is not allowed to become a bottleneck for the proxy it
// observes.
type Hub struct {
	mu     sync.RWMutex
	subs   map[int64]chan LiveMessage
	nextID int64
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int64]chan LiveMessage)}
}

// Subscribe registers a watcher and returns its channel plus an unsubscribe
// function the caller must defer.
func (h *Hub) Subscribe(buffer int) (<-chan LiveMessage, func()) {
	if h == nil {
		return nil, func() {}
	}
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan LiveMessage, buffer)

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if existing, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(existing)
		}
		h.mu.Unlock()
	}
}

// Watchers reports how many consoles are currently connected, so publishers
// can skip building a message nobody will read.
func (h *Hub) Watchers() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// PublishEvent pushes a captured turn.
func (h *Hub) PublishEvent(e Event) {
	h.publish(LiveMessage{Type: LiveEvent, Event: &e})
}

// PublishSession pushes a recomputed session account.
func (h *Hub) PublishSession(s SessionSummary) {
	h.publish(LiveMessage{Type: LiveSession, Session: &s})
}

func (h *Hub) publish(msg LiveMessage) {
	if h == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
