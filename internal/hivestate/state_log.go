package hivestate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// An append-only state log.
//
// HiveState today re-extracts the whole conversation state on every turn and
// puts the result at the head of the prompt. That is the worst possible shape
// for a provider prefix cache: the first message changes, so every token after
// it has to be re-read at full price. It is why the prefix-cache guard exists,
// and why history compression spends most of its life switched off.
//
// A log fixes that by never rewriting anything. Each turn extracts only the
// messages no block covers yet and appends one new block. The bytes of every
// earlier block are frozen, so the rendered state grows strictly by appending —
// which is exactly the shape a prefix cache rewards.
//
// The invariant the whole design rests on, and the one the tests pin down:
//
//	Render(blocks[:n]) is a byte-exact prefix of Render(blocks[:n+1])
//
// Correction is expressed the way a log expresses it: a later block restating a
// fact supersedes an earlier one. Nothing is edited in place, so the reader has
// to be told that later wins — see stateLogPreamble.

// StateBlock is one frozen segment of the state. Once appended it is never
// modified: that immutability is what keeps the rendered prefix stable.
type StateBlock struct {
	// Covers is the number of history messages summarised by this block and
	// every block before it. It only ever grows.
	Covers int
	// JSON is what the extractor produced for the messages this block added.
	JSON string
	// State is the same content parsed. Only the newest block's copy is
	// authoritative, since a later block supersedes an earlier one.
	State *State
	// PrefixHash identifies the exact history this block was built from, so a
	// log can be matched against a conversation without trusting message
	// counts alone.
	PrefixHash string
	// CreatedAt is used only for expiry, never for ordering.
	CreatedAt time.Time
}

// StateLog is the ordered, append-only sequence of blocks for one conversation.
type StateLog struct {
	Blocks []StateBlock
}

// Covered reports how many history messages the log already summarises.
func (l *StateLog) Covered() int {
	if l == nil || len(l.Blocks) == 0 {
		return 0
	}
	return l.Blocks[len(l.Blocks)-1].Covers
}

// Append adds a block covering history up to upTo. It refuses anything that
// would break the append-only contract: a block that covers less than what is
// already covered would make the rendered text shrink or change, which is the
// one thing this type exists to prevent.
func (l *StateLog) Append(upTo int, res *ExtractionResult, prefixHash string) bool {
	if l == nil || res == nil || strings.TrimSpace(res.JSON) == "" || upTo <= l.Covered() {
		return false
	}
	l.Blocks = append(l.Blocks, StateBlock{
		Covers:     upTo,
		JSON:       res.JSON,
		State:      res.State,
		PrefixHash: prefixHash,
		CreatedAt:  time.Now(),
	})
	return true
}

// Current returns the most recent parsed state, which is the one downstream
// routing should read: where entries disagree, the later one wins.
func (l *StateLog) Current() *State {
	for i := len(l.Blocks) - 1; i >= 0; i-- {
		if l.Blocks[i].State != nil {
			return l.Blocks[i].State
		}
	}
	return nil
}

// stateLogPreamble tells the model how to read a log rather than a summary.
// Without it, an early fact that a later block corrects reads as a
// contradiction instead of as history.
const stateLogPreamble = "Conversation state so far, oldest first. " +
	"Each entry summarises a span of earlier turns. Where entries disagree, " +
	"the later one is current.\n"

// Render produces the text that goes into the prompt.
//
// Every block contributes a self-delimited chunk that depends on nothing after
// it, which is what makes a shorter render a byte-exact prefix of a longer one.
func (l *StateLog) Render() string {
	if l == nil || len(l.Blocks) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(stateLogPreamble)
	for _, b := range l.Blocks {
		sb.WriteString("\n--- state ---\n")
		sb.WriteString(b.JSON)
		sb.WriteString("\n")
	}
	return sb.String()
}

// RenderParts returns the same text as Render, split so that each frozen block
// is its own piece.
//
// Anthropic matches its cache block by block, not character by character: a
// single growing text block never matches the previous turn's, and the whole
// append-only property is lost on the wire. Emitted as separate parts, every
// earlier piece stays byte-identical and only the newest one has to be written.
//
// strings.Join(RenderParts(), "") == Render().
func (l *StateLog) RenderParts() []string {
	if l == nil || len(l.Blocks) == 0 {
		return nil
	}
	parts := make([]string, 0, len(l.Blocks)+1)
	parts = append(parts, stateLogPreamble)
	for _, b := range l.Blocks {
		parts = append(parts, "\n--- state ---\n"+b.JSON+"\n")
	}
	return parts
}

// Matches reports whether this log was built from the given history: every
// block's prefix hash must still describe that history. A conversation that
// was edited or replayed differently gets a fresh log rather than a state
// describing turns that no longer exist.
func (l *StateLog) Matches(history []Message) bool {
	return l != nil && l.MatchingBlocks(history) == len(l.Blocks)
}

// MatchingBlocks returns how many leading blocks still describe this history.
//
// The step window is not fixed: a token budget can narrow it and later let it
// widen again, which makes the History zone shrink and stops it being a prefix
// of what earlier blocks were built from. Dropping the whole log on that
// mismatch restarts the state from nothing — the prompt collapses, every state
// item changes at once, and the provider cache the log exists to earn is lost.
// Keeping the blocks that still hold costs nothing and survives the wobble.
func (l *StateLog) MatchingBlocks(history []Message) int {
	if l == nil {
		return 0
	}
	n := 0
	for _, b := range l.Blocks {
		if b.Covers > len(history) {
			break
		}
		if historyHash(history[:b.Covers]) != b.PrefixHash {
			break
		}
		n++
	}
	return n
}

// TruncateTo keeps the first n blocks and discards the rest.
func (l *StateLog) TruncateTo(n int) {
	if l == nil || n < 0 || n >= len(l.Blocks) {
		return
	}
	l.Blocks = l.Blocks[:n]
}

// conversationKey identifies a conversation across turns.
//
// It hashes the opening of the history, which is the one part that does not
// change as the conversation grows, together with the scope. SessionID is what
// keeps two unrelated conversations of the same caller apart; the opening
// message is what keeps a resumed conversation matched to its own log.
func conversationKey(scope Scope, history []Message) string {
	if len(history) == 0 {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{scope.TeamID, scope.KeyHash, scope.SessionID,
		history[0].Role, history[0].Content} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LogStore keeps state logs between turns. The in-memory implementation is
// enough for a single gateway; a shared one is required before this is worth
// anything in production, because a log that is not found is a log that gets
// rebuilt differently — and a rebuilt state is a busted prompt cache.
type LogStore interface {
	Get(key string, history []Message) (*StateLog, bool)
	Put(key string, log *StateLog)
}

// logsPerKey bounds how many conversations may share a key before the least
// recently written one is dropped.
const logsPerKey = 4

type memoryLogStore struct {
	mu      sync.RWMutex
	logs    map[string][]*StateLog
	maxSize int
	ttl     time.Duration
}

func newMemoryLogStore(maxSize int, ttl time.Duration) *memoryLogStore {
	return &memoryLogStore{logs: make(map[string][]*StateLog, maxSize), maxSize: maxSize, ttl: ttl}
}

// Get returns the log that best describes this history.
//
// A key is an opening fingerprint, not an identity: two conversations that
// begin the same way collide on it. Assuming one log per key made them
// overwrite each other every turn, so both restarted from nothing forever.
// Choosing by longest matching prefix keeps them apart without needing the
// client to supply a session id.
func (s *memoryLogStore) Get(key string, history []Message) (*StateLog, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *StateLog
	bestMatch := -1
	for _, log := range s.logs[key] {
		if len(log.Blocks) == 0 || s.expired(log) {
			continue
		}
		if m := log.MatchingBlocks(history); m > bestMatch {
			best, bestMatch = log, m
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

func (s *memoryLogStore) expired(log *StateLog) bool {
	return time.Since(log.Blocks[len(log.Blocks)-1].CreatedAt) > s.ttl
}

func (s *memoryLogStore) Put(key string, log *StateLog) {
	if key == "" || log == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.logs[key]
	for i, existing := range bucket {
		if existing == log {
			bucket[i] = log
			s.logs[key] = bucket
			return
		}
	}
	bucket = append(bucket, log)
	if len(bucket) > logsPerKey {
		bucket = bucket[len(bucket)-logsPerKey:]
	}
	if _, known := s.logs[key]; !known && len(s.logs) >= s.maxSize {
		s.evictOldestLocked()
	}
	s.logs[key] = bucket
}

func (s *memoryLogStore) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, bucket := range s.logs {
		if len(bucket) == 0 {
			delete(s.logs, k)
			return
		}
		last := bucket[len(bucket)-1]
		if len(last.Blocks) == 0 {
			delete(s.logs, k)
			return
		}
		at := last.Blocks[len(last.Blocks)-1].CreatedAt
		if oldestKey == "" || at.Before(oldest) {
			oldestKey, oldest = k, at
		}
	}
	if oldestKey != "" {
		delete(s.logs, oldestKey)
	}
}
