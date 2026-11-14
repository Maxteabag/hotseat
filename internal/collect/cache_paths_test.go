package collect

import (
	"context"
	"github.com/Maxteabag/hotseat/internal/quota"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cacheTransport func(*http.Request) (*http.Response, error)

func (f cacheTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCollectorAndBridgeShareUsageCooldown(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := quota.Client
	t.Cleanup(func() { quota.Client = old })
	calls := 0
	quota.Client = &http.Client{Transport: cacheTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`))}, nil
	})}
	bridge, backend, _ := newBridge(t, nil)
	bridge.Quota = &quota.Cache{}
	a, err := bridge.Rows(context.Background(), true)
	if err != nil || len(a.Accounts) == 0 {
		t.Fatalf("bridge read failed: %v", err)
	}
	c := newCollector(t, backend)
	snap, err := c.Build(context.Background())
	if err != nil || len(snap.Accounts) == 0 || snap.Accounts[0].Usage == nil {
		t.Fatalf("collector read failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("usage requests=%d, want one shared read across collection paths", calls)
	}
}
