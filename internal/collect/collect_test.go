package collect

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/tui"
	"github.com/Maxteabag/hotseat/internal/work"
)

// --- fakes ------------------------------------------------------------------

type fakeBackend struct {
	accounts  []claude.Account
	err       error
	canSwitch bool
}

func (f *fakeBackend) Name() string      { return "fake" }
func (f *fakeBackend) HasProfiles() bool { return true }
func (f *fakeBackend) CanSwitch() bool   { return f.canSwitch }
func (f *fakeBackend) Accounts() ([]claude.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.accounts, nil
}
func (f *fakeBackend) Capabilities() claude.Capabilities {
	return claude.Capabilities{Backend: "fake", Profiles: true, Switch: f.canSwitch}
}

type fakeCodex struct {
	mu         sync.Mutex
	overview   *codex.Overview
	err        error
	balances   []codex.Balance
	withLimits []bool
}

func (f *fakeCodex) Overview(_ context.Context, withLimits bool, _ time.Duration) (*codex.Overview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withLimits = append(f.withLimits, withLimits)
	if f.err != nil {
		return nil, f.err
	}
	if f.overview == nil {
		return &codex.Overview{Accounts: []codex.Account{}}, nil
	}
	return f.overview, nil
}
func (f *fakeCodex) Balances(context.Context, []string) ([]codex.Balance, error) {
	return f.balances, nil
}
func (f *fakeCodex) Accounts() []codex.Account {
	if f.overview == nil {
		return nil
	}
	return f.overview.Accounts
}

type fakeSwitcher struct{ calls []string }

func (f *fakeSwitcher) Switch(name string) error { f.calls = append(f.calls, name); return nil }

func ms(t time.Time) *int64 {
	v := t.UnixMilli()
	return &v
}

func testAccount(now time.Time) claude.Account {
	return claude.Account{
		Alias: "work", Email: "user@example.com", Org: "Example Org", Plan: "team", IsActive: true,
		AccessExpiresAt:  ms(now.Add(4 * time.Hour)),
		RefreshExpiresAt: ms(now.Add(20 * 24 * time.Hour)),
		Token:            "SECRET-NEVER-EXPORT",
	}
}

func emptyWork(t *testing.T) *work.Service {
	return &work.Service{ProjectsDir: t.TempDir(), CodexHome: t.TempDir(), ProcRoot: t.TempDir()}
}

func newBridge(t *testing.T, fetch func(context.Context, string) (quota.Summary, error)) (*Bridge, *fakeBackend, *fakeCodex) {
	t.Helper()
	now := time.Now()
	backend := &fakeBackend{accounts: []claude.Account{testAccount(now)}, canSwitch: true}
	cx := &fakeCodex{}
	b := &Bridge{
		Backend:         backend,
		Codex:           cx,
		Quota:           &quota.Cache{Root: t.TempDir(), Fetch: fetch},
		WorkService:     emptyWork(t),
		RunningSessions: func() int { return 2 },
		AutoRefresh: func(claude.Backend, *claude.Account) error {
			return errors.New("fixture refresh unavailable")
		},
	}
	return b, backend, cx
}

func floatPtr(v float64) *float64 { return &v }

// --- test_collect.py: _account_view -------------------------------------------

func TestAccountViewReportsBothClocks(t *testing.T) {
	now := time.Now()
	view := accountView(testAccount(now), nil, nil, float64(now.UnixNano())/1e9)
	if view.AccessHoursLeft == nil || math.Abs(*view.AccessHoursLeft-4) > 0.05 {
		t.Fatalf("access_hours_left = %v", view.AccessHoursLeft)
	}
	if view.SigninDaysLeft == nil || math.Abs(*view.SigninDaysLeft-20) > 0.05 {
		t.Fatalf("signin_days_left = %v", view.SigninDaysLeft)
	}
	if view.SigninDueSoon {
		t.Fatal("a distant deadline must not be flagged")
	}
}

func TestAccountViewFlagsNearDeadline(t *testing.T) {
	now := time.Now()
	account := testAccount(now)
	account.RefreshExpiresAt = ms(now.Add(3 * 24 * time.Hour))
	if !accountView(account, nil, nil, 0).SigninDueSoon {
		t.Fatal("three days out must be flagged")
	}
}

func TestAccountViewMissingExpiryDoesNotFail(t *testing.T) {
	account := testAccount(time.Now())
	account.AccessExpiresAt, account.RefreshExpiresAt = nil, nil
	view := accountView(account, nil, nil, 0)
	if view.SigninDaysLeft != nil || view.AccessHoursLeft != nil || view.SigninDueSoon {
		t.Fatalf("view = %+v", view)
	}
}

func TestAccountViewNeverIncludesTheToken(t *testing.T) {
	raw, err := json.Marshal(accountView(testAccount(time.Now()), nil, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(strings.ToLower(string(raw)), `"token"`) {
		t.Fatalf("token leaked: %s", raw)
	}
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	for _, key := range []string{"alias", "email", "org", "plan", "plan_label", "rate_limit_tier", "is_active",
		"access_expires_at", "refresh_expires_at", "error", "usage", "access_hours_left", "signin_days_left", "signin_due_soon"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("missing key %s in %s", key, raw)
		}
	}
}

// --- Collector.Build ----------------------------------------------------------

func newCollector(t *testing.T, backend claude.Backend) *Collector {
	t.Helper()
	return &Collector{
		Backend:         backend,
		Codex:           &fakeCodex{err: &codex.Error{Msg: "No Codex installation found at ~/.codex."}},
		Work:            emptyWork(t),
		RunningSessions: func() int { return 1 },
		ModelUsage:      func() *claude.ModelSummary { return nil },
		AutoRefresh:     func(claude.Backend, *claude.Account) error { return errors.New("no refresh in tests") },
		HeaderProbe: func(context.Context, string) (quota.Headers, error) {
			return nil, &quota.ProbeError{Message: "unreachable"}
		},
	}
}

func TestBuildUsesTheUsageEndpointFirst(t *testing.T) {
	backend := &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}}
	c := newCollector(t, backend)
	c.Usage = func(_ context.Context, token string) (quota.Summary, error) {
		if token != "SECRET-NEVER-EXPORT" {
			t.Fatalf("token = %q", token)
		}
		return quota.Summary{Status: "allowed", Available: true, Used7d: floatPtr(0.5)}, nil
	}
	snap, err := c.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 1 || snap.Accounts[0].Error != nil || snap.Accounts[0].Usage == nil || *snap.Accounts[0].Usage.Used7d != 0.5 {
		t.Fatalf("accounts = %+v", snap.Accounts)
	}
	if snap.Codex != nil || snap.Error != nil || snap.Sessions != 1 || snap.Capabilities.Backend != "fake" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Extensions == nil || len(snap.Extensions) != 0 || snap.Stopped == nil {
		t.Fatalf("extensions/stopped must be empty, not null: %+v", snap)
	}
}

func TestBuildFallsBackToHeaderProbe(t *testing.T) {
	backend := &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}}
	c := newCollector(t, backend)
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{}, &quota.UsageError{Message: "usage endpoint down"}
	}
	c.HeaderProbe = func(context.Context, string) (quota.Headers, error) {
		return quota.Headers{"anthropic-ratelimit-unified-status": "allowed",
			"anthropic-ratelimit-unified-5h-utilization": "0.25"}, nil
	}
	snap, _ := c.Build(context.Background())
	usage := snap.Accounts[0].Usage
	if usage == nil || snap.Accounts[0].Error != nil {
		t.Fatalf("account = %+v", snap.Accounts[0])
	}
	if usage.Degraded != "per-model limits unavailable" || usage.Scoped == nil || len(usage.Scoped) != 0 || usage.BlockedModels == nil {
		t.Fatalf("fallback = %+v", usage)
	}
	raw, _ := json.Marshal(usage)
	for _, key := range []string{`"scoped":[]`, `"blocked_models":[]`, `"degraded":"per-model limits unavailable"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("missing %s in %s", key, raw)
		}
	}
}

func TestBuildReportsTheFirstErrorWhenBothProbesFail(t *testing.T) {
	backend := &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}}
	c := newCollector(t, backend)
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{}, &quota.UsageError{Message: "rejected"}
	}
	snap, _ := c.Build(context.Background())
	if snap.Accounts[0].Error == nil || *snap.Accounts[0].Error != "rejected" || snap.Accounts[0].Usage != nil {
		t.Fatalf("account = %+v", snap.Accounts[0])
	}
}

func TestBuildRefreshesExpiredTokensBeforeProbing(t *testing.T) {
	account := testAccount(time.Now())
	account.AccessExpiresAt = ms(time.Now().Add(-time.Hour))
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	var probed atomic.Bool
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		probed.Store(true)
		return quota.Summary{}, nil
	}
	snap, _ := c.Build(context.Background())
	if probed.Load() {
		t.Fatal("a dead token must never reach the API")
	}
	if got := snap.Accounts[0].Error; got == nil || *got != "Access token expired; refresh failed: no refresh in tests" {
		t.Fatalf("error = %v", got)
	}

	// A successful refresh hands the renewed token to the probe.
	c.AutoRefresh = func(_ claude.Backend, a *claude.Account) error { a.Token = "RENEWED"; return nil }
	c.Usage = func(_ context.Context, token string) (quota.Summary, error) {
		if token != "RENEWED" {
			t.Fatalf("token = %q", token)
		}
		return quota.Summary{Status: "allowed"}, nil
	}
	snap, _ = c.Build(context.Background())
	if snap.Accounts[0].Error != nil {
		t.Fatalf("error = %v", *snap.Accounts[0].Error)
	}
}

func TestBuildWithoutTokenAndWithBackendFailure(t *testing.T) {
	account := testAccount(time.Now())
	account.Token = ""
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	snap, _ := c.Build(context.Background())
	if got := snap.Accounts[0].Error; got == nil || *got != "no stored token" {
		t.Fatalf("error = %v", got)
	}

	c = newCollector(t, &fakeBackend{err: &claude.BackendError{Msg: "No credentials"}})
	snap, err := c.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Error == nil || *snap.Error != "No credentials" || len(snap.Accounts) != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

type fakeExtension struct {
	available bool
	err       error
	accounts  []work.AccountQuota
	def       string
}

func (f *fakeExtension) Available() bool { return f.available }
func (f *fakeExtension) Snapshot(_ context.Context, accounts []work.AccountQuota, def string) (any, error) {
	f.accounts, f.def = accounts, def
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"agents": nil, "stopped": []any{}}, nil
}

func TestBuildIncludesAvailableExtensionsAndTheirErrors(t *testing.T) {
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}})
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{Limited: true, BlockedModels: []string{"opus"}}, nil
	}
	ok := &fakeExtension{available: true}
	c.Extensions = map[string]Extension{"clarp": ok, "off": &fakeExtension{}, "broken": &fakeExtension{available: true, err: errors.New("boom")}}
	snap, _ := c.Build(context.Background())
	if _, present := snap.Extensions["off"]; present {
		t.Fatal("an unavailable extension must not appear")
	}
	if snap.Extensions["clarp"] == nil {
		t.Fatalf("extensions = %+v", snap.Extensions)
	}
	if broken, _ := snap.Extensions["broken"].(map[string]any); broken["error"] != "boom" {
		t.Fatalf("broken = %+v", snap.Extensions["broken"])
	}
	if ok.def != "work" || len(ok.accounts) != 1 || ok.accounts[0].Usage == nil || !ok.accounts[0].Usage.Limited {
		t.Fatalf("extension saw accounts=%+v default=%q", ok.accounts, ok.def)
	}
}

func TestBuildStopsWhenTheContextEnds(t *testing.T) {
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{testAccount(time.Now())}})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	c.Usage = func(ctx context.Context, _ string) (quota.Summary, error) {
		close(started)
		<-ctx.Done()
		return quota.Summary{}, &quota.UsageError{Message: ctx.Err().Error()}
	}
	done := make(chan error, 1)
	go func() { _, err := c.Snapshot(ctx, -1); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not stop with its context")
	}
}

// --- Collector cache and sharing ------------------------------------------------

func TestSnapshotReusesFreshBuilds(t *testing.T) {
	c := &Collector{Interval: time.Minute}
	var builds atomic.Int32
	c.build = func(context.Context) (Snapshot, error) {
		builds.Add(1)
		return Snapshot{GeneratedAt: epochNow()}, nil
	}
	ctx := context.Background()
	if _, err := c.Snapshot(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 {
		t.Fatalf("builds = %d, want the cached snapshot reused", builds.Load())
	}
	if _, err := c.Snapshot(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 3 {
		t.Fatalf("builds = %d, want max_age 0 and Refresh to rebuild", builds.Load())
	}
}

// Ported from tests/test_regressions.py: concurrent refreshes share one build,
// both its success and its failure, and a later refresh can build again.
func TestConcurrentRefreshesShareSuccessAndFailureAndAllowRetry(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			c := &Collector{}
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			c.build = func(context.Context) (Snapshot, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					return Snapshot{}, errors.New("test did not release build")
				}
				if failure {
					return Snapshot{}, errors.New("fixture failure")
				}
				return Snapshot{GeneratedAt: epochNow()}, nil
			}
			type outcome struct {
				snap Snapshot
				err  error
			}
			first, second := make(chan outcome, 1), make(chan outcome, 1)
			ctx := context.Background()
			go func() { s, e := c.Snapshot(ctx, -1); first <- outcome{s, e} }()
			<-entered
			go func() { s, e := c.Refresh(ctx); second <- outcome{s, e} }()
			// Give the second caller time to join the in-flight build before it is released.
			time.Sleep(50 * time.Millisecond)
			close(release)
			a, b := <-first, <-second
			if failure {
				for _, o := range []outcome{a, b} {
					if o.err == nil || !strings.Contains(o.err.Error(), "fixture failure") {
						t.Fatalf("err = %v", o.err)
					}
				}
			} else if a.err != nil || b.err != nil || a.snap.GeneratedAt != b.snap.GeneratedAt {
				t.Fatalf("first=%+v/%v second=%+v/%v", a.snap, a.err, b.snap, b.err)
			}
			if calls.Load() != 1 {
				t.Fatalf("build called %d times", calls.Load())
			}
			var retries atomic.Int32
			c.build = func(context.Context) (Snapshot, error) {
				retries.Add(1)
				return Snapshot{GeneratedAt: epochNow()}, nil
			}
			if _, err := c.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if retries.Load() != 1 {
				t.Fatalf("retry built %d times", retries.Load())
			}
		})
	}
}

// --- test_tui_bridge.py ------------------------------------------------------------

func TestLocalSnapshotNeverProbesOrExportsTokens(t *testing.T) {
	var probed atomic.Bool
	b, _, cx := newBridge(t, func(context.Context, string) (quota.Summary, error) {
		probed.Store(true)
		return quota.Summary{}, nil
	})
	rows, err := b.Rows(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if probed.Load() {
		t.Fatal("a local snapshot must not probe")
	}
	if len(cx.withLimits) != 1 || cx.withLimits[0] {
		t.Fatalf("codex overview calls = %v", cx.withLimits)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatalf("token leaked: %s", raw)
	}
	if rows.Accounts[0].CheckedAt != 0 || rows.Sessions != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	snap := rows.Snapshot()
	if snap.Accounts[0].Alias != "work" || snap.Accounts[0].Plan != "Team" || snap.Accounts[0].Workspace != "Example Org" || !snap.Accounts[0].CanSwitch {
		t.Fatalf("tui account = %+v", snap.Accounts[0])
	}
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	row := keys["accounts"].([]any)[0].(map[string]any)
	for _, key := range []string{"cached", "warning", "limited", "reset_credits"} {
		if _, present := row[key]; present {
			t.Fatalf("%s must be absent from an unread claude row: %v", key, row)
		}
	}
	if _, present := row["rate_limit_tier"]; !present {
		t.Fatalf("rate_limit_tier must be present on claude rows: %v", row)
	}
}

func TestRefreshKeepsUnknownAndModelWindowsSeparate(t *testing.T) {
	b, _, _ := newBridge(t, func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{Used7d: floatPtr(0.4), Scoped: []quota.ScopedLimit{{Model: "Opus", Used: 1.1, Reset: floatPtr(123)}}}, nil
	})
	snap, err := b.Snapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	windows := snap.Accounts[0].Windows
	if len(windows) != 2 || windows[0].Used != 0.4 || windows[1].Used != 1.1 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Label != "All models · weekly" || windows[1].Label != "Opus · weekly" || windows[1].Reset != 123 {
		t.Fatalf("windows = %+v", windows)
	}
	if snap.Accounts[0].CheckedAt == 0 {
		t.Fatal("a successful read must set checked_at")
	}
}

func TestErrorIsNotZeroPercent(t *testing.T) {
	b, _, _ := newBridge(t, func(context.Context, string) (quota.Summary, error) {
		return quota.Summary{}, &quota.UsageError{Message: "rejected"}
	})
	snap, _ := b.Snapshot(context.Background(), true)
	row := snap.Accounts[0]
	if len(row.Windows) != 0 || row.Error != "rejected" || row.CheckedAt != 0 {
		t.Fatalf("row = %+v", row)
	}
}

func TestSwitchRequiresAcknowledgementAndExistingAlias(t *testing.T) {
	b, _, _ := newBridge(t, nil)
	switcher := &fakeSwitcher{}
	b.Profiles = switcher
	for _, ack := range []bool{false, true} {
		if err := b.Switch(context.Background(), tui.Account{Provider: "codex", Alias: "unknown"}, ack); err == nil {
			t.Fatalf("ack=%v must be refused", ack)
		}
	}
	if len(switcher.calls) != 0 {
		t.Fatalf("switch ran: %v", switcher.calls)
	}
	b.Codex = &fakeCodex{overview: &codex.Overview{Accounts: []codex.Account{{Alias: "cx", Saved: true}}}}
	if err := b.Switch(context.Background(), tui.Account{Provider: "codex", Alias: "cx"}, true); err != nil {
		t.Fatal(err)
	}
	if len(switcher.calls) != 1 || switcher.calls[0] != "cx" {
		t.Fatalf("switch calls = %v", switcher.calls)
	}
}

func TestUnsupportedActionsAreRejected(t *testing.T) {
	b, _, _ := newBridge(t, nil)
	ctx := context.Background()
	if err := b.Action(ctx, tui.Account{Provider: "other", Alias: "work"}, "switch"); err == nil || err.Error() != "This action is not available for that provider" {
		t.Fatalf("err = %v", err)
	}
	if err := b.Action(ctx, tui.Account{Provider: "claude", Alias: "work"}, "delete"); err == nil || err.Error() != "Unsupported action" {
		t.Fatalf("err = %v", err)
	}
	if err := b.Launch(ctx, tui.Account{Provider: "other", Alias: "work"}); err == nil {
		t.Fatal("launch on an unknown provider must be refused")
	}
}

func TestClaudeConfirmationDelegatesExistingGuard(t *testing.T) {
	b, _, _ := newBridge(t, nil)
	err := b.Switch(context.Background(), tui.Account{Provider: "claude", Alias: "work"}, false)
	var actionErr *claude.ActionError
	if !errors.As(err, &actionErr) || !strings.Contains(err.Error(), "2 running session(s)") {
		t.Fatalf("err = %v, want the switch guard with the running-session count", err)
	}
	if err := b.Switch(context.Background(), tui.Account{Provider: "claude", Alias: "missing"}, false); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("an unknown alias must be named: %v", err)
	}
}

func TestCodexResetBalanceReachesTUI(t *testing.T) {
	b, _, cx := newBridge(t, func(context.Context, string) (quota.Summary, error) { return quota.Summary{}, nil })
	cx.overview = &codex.Overview{Accounts: []codex.Account{{Alias: "cx", Saved: true, Usage: &codex.Usage{}}}}
	two := 2
	cx.balances = []codex.Balance{{Alias: "cx", Reading: codex.Reading{AvailableCount: &two}}}
	rows, err := b.Rows(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	last := rows.Accounts[len(rows.Accounts)-1]
	if last.Provider != "codex" || last.ResetCredits == nil || *last.ResetCredits != 2 || last.ResetCreditsError != "" {
		t.Fatalf("row = %+v", last)
	}
	if last.CheckedAt == 0 || last.Error != "" {
		t.Fatalf("an empty usage reading is still a reading: %+v", last)
	}
	raw, _ := json.Marshal(last)
	for _, key := range []string{`"reset_credits":2`, `"reset_credits_error":""`, "OpenAI reports no separate Spark allowance"} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("missing %s in %s", key, raw)
		}
	}
	if strings.Contains(string(raw), "rate_limit_tier") || strings.Contains(string(raw), `"cached"`) {
		t.Fatalf("claude-only keys on a codex row: %s", raw)
	}
}

func TestCodexRowsWithoutReadingsAndWithDiscoveryErrors(t *testing.T) {
	b, _, cx := newBridge(t, func(context.Context, string) (quota.Summary, error) { return quota.Summary{}, nil })
	email := "cx@example.com"
	failed := "limit_reached"
	cx.overview = &codex.Overview{Accounts: []codex.Account{
		{Alias: "none", Saved: true, Identity: codex.Identity{Email: &email}},
		{Alias: "bad", Saved: false, Usage: &codex.Usage{Error: &failed, Windows: []codex.UsageWindow{{Label: "Codex · weekly", Used: 0.3}}}},
	}}
	rows, _ := b.Rows(context.Background(), true)
	byAlias := map[string]Row{}
	for _, r := range rows.Accounts {
		byAlias[r.Alias] = r
	}
	if r := byAlias["none"]; r.Error != "No quota reading returned" || r.Email != "cx@example.com" || r.Plan != "Unknown plan" || !r.CanLaunch {
		t.Fatalf("row = %+v", r)
	}
	if r := byAlias["bad"]; r.Error != "limit_reached" || r.CheckedAt != 0 || len(r.Windows) != 1 || r.Saved || r.CanSwitch {
		t.Fatalf("row = %+v", r)
	}
	rows, _ = b.Rows(context.Background(), false)
	if r := rows.Accounts[len(rows.Accounts)-1]; r.Provider == "codex" && r.Alias == "none" && r.Error != "" {
		t.Fatalf("no error without refresh: %+v", r)
	}

	cx.err = &codex.Error{Msg: "No Codex installation found at ~/.codex."}
	rows, _ = b.Rows(context.Background(), false)
	if len(rows.Errors) != 1 || rows.Errors[0] != "Codex discovery: No Codex installation found at ~/.codex." {
		t.Fatalf("errors = %v", rows.Errors)
	}
	b.Backend = &fakeBackend{err: &claude.BackendError{Msg: "nothing signed in"}}
	rows, _ = b.Rows(context.Background(), false)
	if len(rows.Errors) != 2 || rows.Errors[0] != "Claude discovery: nothing signed in" {
		t.Fatalf("errors = %v", rows.Errors)
	}
}

func TestDuplicateIdentitiesPreferWorkingCredentials(t *testing.T) {
	rows := []Row{
		{Provider: "codex", Alias: "ai1", Email: "one@example.com", Workspace: "org", Error: "token_revoked", Active: true},
		{Provider: "codex", Alias: "ai1-test", Email: "one@example.com", Workspace: "org", Active: true, CheckedAt: 10, Windows: []tui.Window{}},
		{Provider: "codex", Alias: "other-workspace", Email: "one@example.com", Workspace: "other", Active: false, CheckedAt: 10},
	}
	grouped := GroupAccounts(rows)
	if len(grouped) != 2 {
		t.Fatalf("grouped = %+v", grouped)
	}
	var row Row
	for _, r := range grouped {
		if r.Workspace == "org" {
			row = r
		}
	}
	if row.Alias != "ai1-test" || row.DisplayAlias != "ai1" || len(row.Aliases) != 2 || row.Aliases[0] != "ai1" || row.Aliases[1] != "ai1-test" {
		t.Fatalf("row = %+v", row)
	}
	if len(row.ProfileNotes) == 0 || row.ProfileNotes[0] != "ai1: Sign-in expired" {
		t.Fatalf("notes = %v", row.ProfileNotes)
	}
	if grouped[0].Alias != "ai1-test" {
		t.Fatalf("active rows sort first: %+v", grouped)
	}
}

func TestUnknownIdentitiesNeverMerge(t *testing.T) {
	rows := []Row{
		{Provider: "codex", Alias: "a", Email: "Unknown email"},
		{Provider: "codex", Alias: "b", Email: "Unknown email"},
		{Provider: "claude", Alias: "c", Email: "x@example.com"},
		{Provider: "claude", Alias: "d", Email: "x@example.com"},
	}
	grouped := GroupAccounts(rows)
	if len(grouped) != 4 {
		t.Fatalf("grouped = %+v", grouped)
	}
	if grouped[0].Provider != "claude" || grouped[0].DisplayAlias != "c" || grouped[2].DisplayAlias != "a" {
		t.Fatalf("order = %+v", grouped)
	}
	for _, r := range grouped {
		if len(r.Aliases) != 1 || len(r.ProfileNotes) != 0 || !r.Grouped {
			t.Fatalf("row = %+v", r)
		}
	}
}

func TestGroupedRowsSortByProviderActivityErrorAndAlias(t *testing.T) {
	rows := []Row{
		{Provider: "claude", Alias: "zed", Active: false, Error: "boom"},
		{Provider: "claude", Alias: "beta", Active: false},
		{Provider: "claude", Alias: "alpha", Active: false},
		{Provider: "claude", Alias: "live", Active: true},
	}
	var got []string
	for _, r := range GroupAccounts(rows) {
		got = append(got, r.DisplayAlias)
	}
	if strings.Join(got, ",") != "live,alpha,beta,zed" {
		t.Fatalf("order = %v", got)
	}
}

func TestExpiredAccessTokenDoesNotHitUsageAPI(t *testing.T) {
	var fetched atomic.Bool
	b, backend, _ := newBridge(t, func(context.Context, string) (quota.Summary, error) {
		fetched.Store(true)
		return quota.Summary{}, nil
	})
	one := int64(1)
	backend.accounts[0].AccessExpiresAt = &one
	snap, _ := b.Snapshot(context.Background(), true)
	if fetched.Load() {
		t.Fatal("an expired token must not reach the usage API")
	}
	if !strings.Contains(snap.Accounts[0].Error, "Access token expired") {
		t.Fatalf("error = %q", snap.Accounts[0].Error)
	}
	if !strings.Contains(snap.Accounts[0].Error, "fixture refresh unavailable") {
		t.Fatalf("the refresh failure must be named: %q", snap.Accounts[0].Error)
	}
}

func TestMissingTokenIsAnError(t *testing.T) {
	b, backend, _ := newBridge(t, nil)
	backend.accounts[0].Token = ""
	backend.accounts[0].Alias = "(signed in)"
	snap, _ := b.Snapshot(context.Background(), true)
	row := snap.Accounts[0]
	if row.Error != "No stored token" || row.Saved || row.CanSwitch || !row.CanLaunch {
		t.Fatalf("row = %+v", row)
	}
	if row.Email != "user@example.com" || row.DisplayAlias != "(signed in)" {
		t.Fatalf("row = %+v", row)
	}
}

func TestRowJSONMatchesThePythonKeys(t *testing.T) {
	tier := "default_claude_max_20x"
	row := Row{Provider: "claude", Alias: "work", Email: "e", Plan: "Max 20×", RateLimitTier: &tier, Read: true, Grouped: true}
	raw, _ := json.Marshal(row)
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	want := []string{"provider", "alias", "email", "plan", "rate_limit_tier", "workspace", "active", "saved", "windows",
		"error", "can_switch", "can_launch", "checked_at", "cached", "warning", "limited", "display_alias", "aliases", "profile_notes"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing %s in %s", k, raw)
		}
	}
	if keys["windows"] == nil || keys["aliases"] == nil || keys["profile_notes"] == nil {
		t.Fatalf("lists must be empty, not null: %s", raw)
	}
}

// --- work delegation -------------------------------------------------------------

func TestWorkOperationsDelegateToTheService(t *testing.T) {
	b, _, _ := newBridge(t, nil)
	snap, err := b.Work(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Items == nil || snap.Errors == nil || len(snap.Items) != 0 {
		t.Fatalf("empty listing must have empty lists: %+v", snap)
	}
	item := tui.WorkItem{Provider: "claude", ID: "missing", Revision: "x"}
	if _, err := b.InspectWork(context.Background(), item); err == nil {
		t.Fatal("a missing session must be reported")
	}
	err = b.WorkAction(context.Background(), item, "resume")
	var workErr *work.WorkError
	if !errors.As(err, &workErr) || err.Error() != "Session is missing or ambiguous; refresh the work list" {
		t.Fatalf("err = %v", err)
	}
	if _, err := b.ResumeWork(context.Background(), item, false); err == nil || err.Error() != "Confirmation is required" {
		t.Fatalf("err = %v", err)
	}
	if _, err := b.RebootWork(context.Background(), item, false); err == nil || err.Error() != "Confirmation is required" {
		t.Fatalf("err = %v", err)
	}
	if err := b.WorkAction(context.Background(), item, "delete"); err == nil || err.Error() != "Unsupported work action" {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchersUseTheInjectedSpawnAndTerminal(t *testing.T) {
	var argv []string
	l := &terminalLauncher{spawn: func(a []string, _ []string) error { argv = a; return nil }, terminal: "kitty"}
	if err := l.LaunchCommand([]string{"claude", "--resume", "abc"}, "/work dir"); err != nil {
		t.Fatal(err)
	}
	if len(argv) < 5 || argv[0] != "kitty" || argv[1] != "-e" || argv[2] != "bash" || !strings.Contains(argv[4], "cd '/work dir' && exec claude --resume abc") {
		t.Fatalf("argv = %q", argv)
	}
	terminal, err := l.Detect()
	if err != nil || terminal != "kitty" || l.Flag("gnome-terminal") != "--" {
		t.Fatalf("detect = %q/%v flag = %q", terminal, err, l.Flag("gnome-terminal"))
	}
	if err := l.Spawn([]string{"x"}, nil); err != nil || argv[0] != "x" {
		t.Fatalf("spawn did not use the injected function: %q %v", argv, err)
	}
}
