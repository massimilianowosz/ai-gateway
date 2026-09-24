package hivetrace

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Reverse resolution of caller addresses.
//
// An operator reads "marco-laptop.corp.local", not "10.4.19.220", so the
// address is turned into a name where the network can say one. Three
// properties matter and none of them is free:
//
//   - It never runs on the request path. A PTR query can take seconds against
//     an unreachable resolver, and a turn must not wait for a label.
//   - Every answer is cached, including the failures. A network without
//     reverse zones would otherwise pay the full timeout on every turn from
//     every workstation, forever.
//   - A name is a hint, not proof. PTR records are controlled by whoever owns
//     the reverse zone, so this is a convenience for reading a console, and
//     the address stays recorded alongside as the fact.
type hostnameResolver struct {
	mu    sync.RWMutex
	cache map[string]hostnameEntry
	self  string
	ttl   time.Duration
	limit time.Duration
	// lookup is the resolver call, replaced in tests.
	lookup func(ctx context.Context, addr string) ([]string, error)
}

type hostnameEntry struct {
	name string
	at   time.Time
}

const (
	hostnameTTL     = 30 * time.Minute
	hostnameTimeout = 300 * time.Millisecond
)

func newHostnameResolver() *hostnameResolver {
	return &hostnameResolver{
		cache:  make(map[string]hostnameEntry),
		self:   localCaller(),
		ttl:    hostnameTTL,
		limit:  hostnameTimeout,
		lookup: net.DefaultResolver.LookupAddr,
	}
}

// localCaller names a loopback caller.
//
// Only the hostname, never the account: HTTP carries no operating-system user,
// and a loopback caller is the one case where this process could guess one
// from itself. Showing it here would put a label in the console that no real
// deployment can ever produce — every caller that matters arrives over the
// network, where there is nothing to read. Who authenticated is recorded
// separately, on the key, and that is the answer that holds everywhere.
func localCaller() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(host, ".")
}

// hostFor returns a name for an address, or "" when the network cannot give
// one. The caller keeps the address either way.
func (h *hostnameResolver) hostFor(ip string) string {
	if h == nil || ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.IsLoopback() {
		return h.self
	}

	if name, ok := h.cached(ip); ok {
		return name
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.limit)
	defer cancel()

	name := ""
	if names, err := h.lookup(ctx, ip); err == nil && len(names) > 0 {
		name = strings.TrimSuffix(names[0], ".")
	}
	h.store(ip, name)
	return name
}

func (h *hostnameResolver) cached(ip string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.cache[ip]
	if !ok || time.Since(e.at) > h.ttl {
		return "", false
	}
	return e.name, true
}

func (h *hostnameResolver) store(ip, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache[ip] = hostnameEntry{name: name, at: time.Now()}
}
