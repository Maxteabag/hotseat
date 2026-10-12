package collect

import (
	"context"
	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/work"
	"testing"
	"time"
)

type waitingIntegration struct{}

func (waitingIntegration) Available() bool { return true }
func (waitingIntegration) Snapshot(ctx context.Context, _ []work.AccountQuota, _ string) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// An optional stopped-work query must not consume the caller's whole deadline
// and hide otherwise usable, already-probed account capacity.
func TestSlowIntegrationReturnsAccountCapacityBeforeCallerTimeout(t *testing.T) {
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}})
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{Status: "allowed", Available: true}, nil
	}
	c.Extensions = map[string]Extension{"clarp": waitingIntegration{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("optional integration exhausted caller deadline before returning account capacity")
	}
	if len(s.Accounts) != 1 || !s.Accounts[0].Usage.Available {
		t.Fatal("account capacity lost")
	}
	if e, ok := s.Extensions["clarp"].(map[string]any); !ok || e["error"] != context.DeadlineExceeded.Error() {
		t.Fatalf("timeout must remain visible: %v", s.Extensions)
	}
}
