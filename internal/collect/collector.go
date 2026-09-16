// Package collect builds snapshots on demand, sharing cached data and in-flight
// refreshes, and is the in-process boundary the TUI consumes.
//
// It ports two Python modules. collect.py owned the cached snapshot and
// coordinated concurrent refresh requests: work happens when something asks for
// it, because refreshing on a timer means probing accounts and scanning
// transcripts for nobody whenever the page is closed, which is most of the
// time. tui_bridge.py was the JSON boundary for the TUI; only public account
// fields left it, and only they leave here.
package collect

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/work"
)

const (
	// RefreshInterval is how long a snapshot is reused before Snapshot rebuilds it.
	RefreshInterval = 300 * time.Second
	// MaxParallelProbes bounds the quota probes running at once.
	MaxParallelProbes = 4
	// SigninWarnDays: warn this far ahead of the hard interactive-sign-in deadline.
	SigninWarnDays = 7
)

// AccountView is one account as the snapshot publishes it: the public view plus
// the probe outcome and the two clocks that bound its usefulness.
type AccountView struct {
	claude.PublicAccount
	Error *string        `json:"error"`
	Usage *quota.Summary `json:"usage"`
	// AccessHoursLeft is how long the stored access token lasts; nil when unknown.
	AccessHoursLeft *float64 `json:"access_hours_left"`
	// SigninDaysLeft counts down to a hard browser sign-in. Refreshing renews the
	// access token but does not extend this window, so it runs out no matter how
	// much the account is used.
	SigninDaysLeft *float64 `json:"signin_days_left"`
	SigninDueSoon  bool     `json:"signin_due_soon"`
}

// Snapshot is what Collector.Build returns; field names match the Python dict.
type Snapshot struct {
	GeneratedAt  float64             `json:"generated_at"`
	Capabilities claude.Capabilities `json:"capabilities"`
	Sessions     int                 `json:"sessions"`
	Accounts     []AccountView       `json:"accounts"`
	// ModelUsage is machine-wide and token-based. Deliberately not merged into
	// the per-account quota above: different source, different unit.
	ModelUsage *claude.ModelSummary `json:"model_usage"`
	// Extensions holds optional integrations by name; {"clarp": ...} when Clarp
	// state exists on this machine, otherwise empty.
	Extensions map[string]any `json:"extensions"`
	// Codex is nil when Codex is not installed. Absent is a normal state, not an error.
	Codex *codex.Overview `json:"codex"`
	// Stopped is native work a usage limit stopped, with whether it can be
	// continued now.
	Stopped []work.StoppedItem `json:"stopped"`
	Error   *string            `json:"error"`
}

// AccountView builds the published view of one account from its probe outcome.
// now is epoch seconds; 0 means the wall clock.
func accountView(account claude.Account, usage *quota.Summary, probeErr *string, now float64) AccountView {
	if now == 0 {
		now = epochNow()
	}
	view := AccountView{PublicAccount: account.Public(), Error: probeErr, Usage: usage}
	if access := account.AccessExpiresAt; access != nil && *access != 0 {
		hours := (float64(*access)/1000 - now) / 3600
		view.AccessHoursLeft = &hours
	}
	if refresh := account.RefreshExpiresAt; refresh != nil && *refresh != 0 {
		days := (float64(*refresh)/1000 - now) / 86400
		view.SigninDaysLeft = &days
		view.SigninDueSoon = days <= SigninWarnDays
	}
	return view
}

func epochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Extension is an optional integration that contributes to Snapshot.Extensions.
type Extension interface {
	// Available reports whether the integration's state exists on this machine.
	Available() bool
	// Snapshot is the integration's contribution; an error becomes
	// {"error": message} rather than failing the whole snapshot.
	Snapshot(ctx context.Context, accounts []work.AccountQuota, defaultAlias string) (any, error)
}

// Collector owns the cached snapshot and coordinates concurrent refresh
// requests. The zero value is usable and reads the real machine; the exported
// fields are seams for tests and callers that already hold the dependencies.
type Collector struct {
	// Backend supplies Claude accounts; nil means claude.ForPlatform("").
	Backend claude.Backend
	// Interval is how long Snapshot reuses a build; zero means RefreshInterval.
	Interval time.Duration
	// Codex reads Codex accounts; nil means codex.New(codex.DefaultHome()).
	Codex CodexSource
	// Work lists stopped native work; nil means work.New(nil, nil).
	Work *work.Service
	// Extensions are keyed by name in Snapshot.Extensions when available.
	Extensions map[string]Extension

	// Usage reads quota from the OAuth usage endpoint; nil means quota.ForToken.
	Usage func(ctx context.Context, token string) (quota.Summary, error)
	// HeaderProbe is the fallback rate-limit probe; nil means quota.Probe.
	HeaderProbe func(ctx context.Context, token string) (quota.Headers, error)
	// AutoRefresh brings an expired account's token back through the CLI; nil
	// means claude.Auto with default options.
	AutoRefresh func(backend claude.Backend, account *claude.Account) error
	// RunningSessions counts live claude processes; nil means claude.RunningSessions.
	RunningSessions func() int
	// ModelUsage aggregates recent per-model tokens; nil means claude.RecentByModel.
	ModelUsage func() *claude.ModelSummary
	// Now is the clock in epoch seconds; nil means the wall clock.
	Now func() float64
	// build replaces Build; tests use it to observe sharing.
	build func(ctx context.Context) (Snapshot, error)

	mu       sync.Mutex
	snapshot *Snapshot
	inflight singleflight.Group
}

// CodexSource is the part of *codex.Codex the collector and the bridge use.
type CodexSource interface {
	Overview(ctx context.Context, withLimits bool, maxAge time.Duration) (*codex.Overview, error)
	Balances(ctx context.Context, aliases []string) ([]codex.Balance, error)
	Accounts() []codex.Account
}

func (c *Collector) backend() claude.Backend {
	if c.Backend == nil {
		c.Backend = claude.ForPlatform("")
	}
	return c.Backend
}

func (c *Collector) codex() CodexSource {
	if c.Codex == nil {
		c.Codex = codex.New(codex.DefaultHome())
	}
	return c.Codex
}

func (c *Collector) work() *work.Service {
	if c.Work == nil {
		c.Work = work.New(nil, nil)
	}
	return c.Work
}

func (c *Collector) now() float64 {
	if c.Now != nil {
		return c.Now()
	}
	return epochNow()
}

func (c *Collector) interval() time.Duration {
	if c.Interval <= 0 {
		return RefreshInterval
	}
	return c.Interval
}

func (c *Collector) usage(ctx context.Context, token string) (quota.Summary, error) {
	if c.Usage != nil {
		return c.Usage(ctx, token)
	}
	return quota.ForToken(ctx, token)
}

func (c *Collector) headerProbe(ctx context.Context, token string) (quota.Headers, error) {
	if c.HeaderProbe != nil {
		return c.HeaderProbe(ctx, token)
	}
	return quota.Probe(ctx, token)
}

func (c *Collector) refresh(backend claude.Backend, account *claude.Account) error {
	if c.AutoRefresh != nil {
		return c.AutoRefresh(backend, account)
	}
	_, err := claude.Auto(backend, account, claude.RefreshOptions{})
	return err
}

func (c *Collector) runningSessions() int {
	if c.RunningSessions != nil {
		return c.RunningSessions()
	}
	return claude.RunningSessions()
}

func (c *Collector) modelUsage() *claude.ModelSummary {
	if c.ModelUsage != nil {
		return c.ModelUsage()
	}
	return claude.RecentByModel(claude.DefaultDays, "", time.Time{})
}

// --- building ---------------------------------------------------------------

// Build probes every account and returns a fresh snapshot. It fails only when
// the context ends; every other problem is reported inside the snapshot.
func (c *Collector) Build(ctx context.Context) (Snapshot, error) {
	if c.build != nil {
		return c.build(ctx)
	}
	backend := c.backend()
	accounts, err := backend.Accounts()
	var fatal *string
	if err != nil {
		accounts = nil
		message := err.Error()
		fatal = &message
	}

	views := make([]AccountView, len(accounts))
	if len(accounts) > 0 {
		group, gctx := errgroup.WithContext(ctx)
		group.SetLimit(MaxParallelProbes)
		for i := range accounts {
			group.Go(func() error {
				if err := gctx.Err(); err != nil {
					return err
				}
				account := accounts[i]
				usage, probeErr := c.probe(gctx, &account)
				views[i] = accountView(account, usage, probeErr, c.now())
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			return Snapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		GeneratedAt:  c.now(),
		Capabilities: backend.Capabilities(),
		Sessions:     c.runningSessions(),
		Accounts:     views,
		ModelUsage:   c.modelUsage(),
		Extensions:   c.extensions(ctx, views),
		Codex:        c.codexOverview(ctx),
		Stopped:      c.stopped(views),
		Error:        fatal,
	}, nil
}

// codexOverview is the Codex accounts, nil when Codex is absent.
//
// Quota comes from cache here. Probing spawns one process per saved account,
// which is not something a refresh timer should do; the windows are hourly and
// weekly, so a reading up to half an hour old is still useful.
func (c *Collector) codexOverview(ctx context.Context) *codex.Overview {
	overview, err := c.codex().Overview(ctx, true, codex.LimitsTTL)
	if err != nil {
		return nil
	}
	return overview
}

// quotas is the readiness view of the probed accounts: alias plus the part of
// the usage summary readiness looks at, nil when the probe failed.
func quotas(views []AccountView) ([]work.AccountQuota, string) {
	accounts := make([]work.AccountQuota, 0, len(views))
	defaultAlias := ""
	for _, view := range views {
		item := work.AccountQuota{Alias: view.Alias}
		if view.Usage != nil {
			item.Usage = &work.Quota{Limited: view.Usage.Limited, BlockedModels: view.Usage.BlockedModels}
		}
		accounts = append(accounts, item)
		if view.IsActive && defaultAlias == "" {
			defaultAlias = view.Alias
		}
	}
	return accounts, defaultAlias
}

// stopped is work a usage limit stopped, with whether it can be continued now.
func (c *Collector) stopped(views []AccountView) []work.StoppedItem {
	items := c.work().Stopped(work.DefaultWindowDays, true)
	if items == nil {
		items = []work.StoppedItem{}
	}
	accounts, defaultAlias := quotas(views)
	for i := range items {
		readiness := work.ReadinessOf(items[i], accounts, defaultAlias)
		items[i].Readiness = &readiness
	}
	return items
}

// extensions is every available integration's contribution, by name.
func (c *Collector) extensions(ctx context.Context, views []AccountView) map[string]any {
	out := map[string]any{}
	if len(c.Extensions) == 0 {
		return out
	}
	accounts, defaultAlias := quotas(views)
	for name, extension := range c.Extensions {
		if extension == nil || !extension.Available() {
			continue
		}
		result, err := extension.Snapshot(ctx, accounts, defaultAlias)
		if err != nil {
			out[name] = map[string]any{"error": err.Error()}
			continue
		}
		out[name] = result
	}
	return out
}

// probe reads one account's quota.
//
// The OAuth usage endpoint is the real source: it costs no inference, answers
// even when the account is rate limited, and reports per-model limits. The
// header probe is kept only as a fallback for when that endpoint is
// unavailable, and it cannot see per-model limits at all.
func (c *Collector) probe(ctx context.Context, account *claude.Account) (*quota.Summary, *string) {
	fail := func(message string) (*quota.Summary, *string) { return nil, &message }
	if account.Token == "" {
		return fail("no stored token")
	}
	if claude.Expired(*account, c.now(), 0) {
		// Never send a dead token to the API. Bring the profile back through
		// the CLI first; if that cannot be done, say why and stop there.
		if err := c.refresh(c.backend(), account); err != nil {
			return fail("Access token expired; refresh failed: " + err.Error())
		}
	}
	summary, first := c.usage(ctx, account.Token)
	if first == nil {
		return &summary, nil
	}
	var usageErr *quota.UsageError
	if !errors.As(first, &usageErr) {
		return fail(first.Error())
	}
	headers, err := c.headerProbe(ctx, account.Token)
	if err != nil {
		return fail(first.Error())
	}
	fallback := quota.SummariseHeaders(headers)
	fallback.Scoped = []quota.ScopedLimit{}
	fallback.BlockedModels = []string{}
	fallback.Degraded = "per-model limits unavailable"
	return &fallback, nil
}

// --- cache ------------------------------------------------------------------

// Snapshot returns the snapshot, rebuilding it only if the cached one is older
// than maxAge (negative means the collector's interval). Concurrent callers
// share one in-flight build.
func (c *Collector) Snapshot(ctx context.Context, maxAge time.Duration) (Snapshot, error) {
	if maxAge < 0 {
		maxAge = c.interval()
	}
	c.mu.Lock()
	cached := c.snapshot
	c.mu.Unlock()
	if cached != nil && c.now()-cached.GeneratedAt <= maxAge.Seconds() {
		return *cached, nil
	}
	return c.share(ctx)
}

// Refresh rebuilds the snapshot regardless of age, joining a build already in
// flight rather than starting a second one.
func (c *Collector) Refresh(ctx context.Context) (Snapshot, error) {
	return c.share(ctx)
}

func (c *Collector) share(ctx context.Context) (Snapshot, error) {
	results := c.inflight.DoChan("snapshot", func() (any, error) {
		fresh, err := c.Build(ctx)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.snapshot = &fresh
		c.mu.Unlock()
		return fresh, nil
	})
	select {
	case result := <-results:
		if result.Err != nil {
			return Snapshot{}, result.Err
		}
		return result.Val.(Snapshot), nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}
