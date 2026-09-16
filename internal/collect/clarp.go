package collect

import (
	"context"

	"github.com/Maxteabag/hotseat/internal/clarp"
	"github.com/Maxteabag/hotseat/internal/work"
)

// ClarpExtension is the Clarp integration as the collector sees it: available
// only when Clarp keeps state on this machine.
type ClarpExtension struct{ *clarp.Service }

// Available is clarp.Available: the state database exists.
func (ClarpExtension) Available() bool { return clarp.Available() }

// Snapshot is the plugin's contribution: the agents overview and the stopped
// Clarp agents with their readiness.
func (e ClarpExtension) Snapshot(ctx context.Context, accounts []work.AccountQuota, defaultAlias string) (any, error) {
	return e.Service.Snapshot(ctx, accounts, defaultAlias)
}

// NewCollector is a Collector against the real machine with the Clarp
// integration compiled in.
func NewCollector() *Collector {
	return &Collector{Extensions: map[string]Extension{"clarp": ClarpExtension{clarp.New()}}}
}
