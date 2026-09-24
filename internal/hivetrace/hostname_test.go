package hivetrace

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testResolver(answers map[string][]string, err error) (*hostnameResolver, *int) {
	calls := 0
	h := newHostnameResolver()
	h.self = "appliance-01"
	h.lookup = func(_ context.Context, addr string) ([]string, error) {
		calls++
		if err != nil {
			return nil, err
		}
		names, ok := answers[addr]
		if !ok {
			return nil, errors.New("no PTR record")
		}
		return names, nil
	}
	return h, &calls
}

func TestHostFor_ResolvesAndStripsTrailingDot(t *testing.T) {
	h, _ := testResolver(map[string][]string{
		"10.4.19.220": {"marco-laptop.corp.local."},
	}, nil)
	assert.Equal(t, "marco-laptop.corp.local", h.hostFor("10.4.19.220"))
}

// A loopback caller is the appliance itself, and "localhost" names nothing an
// operator can act on.
func TestHostFor_LoopbackIsThisMachine(t *testing.T) {
	h, calls := testResolver(nil, nil)
	assert.Equal(t, "appliance-01", h.hostFor("127.0.0.1"))
	assert.Equal(t, "appliance-01", h.hostFor("::1"))
	assert.Zero(t, *calls, "loopback must not reach the resolver")
}

// A network without reverse zones would otherwise pay the lookup timeout on
// every turn from every workstation, so the absence is cached too.
func TestHostFor_CachesFailures(t *testing.T) {
	h, calls := testResolver(nil, errors.New("timeout"))
	assert.Empty(t, h.hostFor("10.4.19.220"))
	assert.Empty(t, h.hostFor("10.4.19.220"))
	assert.Equal(t, 1, *calls, "a failed lookup must not be repeated")
}

func TestHostFor_CacheExpires(t *testing.T) {
	h, calls := testResolver(map[string][]string{"10.0.0.5": {"box.local"}}, nil)
	h.ttl = time.Nanosecond
	h.hostFor("10.0.0.5")
	time.Sleep(time.Millisecond)
	h.hostFor("10.0.0.5")
	assert.Equal(t, 2, *calls)
}

func TestHostFor_IgnoresWhatIsNotAnAddress(t *testing.T) {
	h, calls := testResolver(nil, nil)
	assert.Empty(t, h.hostFor(""))
	assert.Empty(t, h.hostFor("not-an-ip"))
	assert.Zero(t, *calls)
}

// A loopback caller is named by its host alone. The account is deliberately
// absent: no remote caller can ever supply one, and a label that only appears
// in development would promise a field the product cannot deliver.
func TestLocalCaller_IsHostOnly(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this machine")
	}
	got := localCaller()
	assert.Equal(t, host, got)
	assert.NotContains(t, got, "@")
}
