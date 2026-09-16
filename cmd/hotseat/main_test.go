package main

// The command line is the primary interface, so its contract is tested
// directly. Nothing here touches the network or real credentials: every seam
// in app is replaced with a fake, the way the Python tests patched modules.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Maxteabag/hotseat/internal/version"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/clarp"
	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/collect"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/work"
)

func ptrOf[T any](v T) *T { return &v }

func fixtureAccount() collect.AccountView {
	usage := &quota.Summary{Source: quota.SourceProbe, Status: "allowed", Available: true,
		Used5h: ptrOf(0.36), Used7d: ptrOf(0.53), Binding: "five_hour", OverageStatus: "allowed"}
	return collect.AccountView{
		PublicAccount: claude.PublicAccount{Alias: "work", Email: ptrOf("user@example.com"),
			Org: ptrOf("Example Org"), Plan: ptrOf("team"), IsActive: true},
		Usage: usage, AccessHoursLeft: ptrOf(4.0), SigninDaysLeft: ptrOf(20.0),
	}
}

func fixtureSnapshot() collect.Snapshot {
	return collect.Snapshot{Sessions: 3, Accounts: []collect.AccountView{fixtureAccount()},
		Extensions: map[string]any{}, Stopped: []work.StoppedItem{}}
}

type fakeBackend struct{ accounts []claude.Account }

func (fakeBackend) Name() string                          { return "file" }
func (fakeBackend) HasProfiles() bool                     { return true }
func (fakeBackend) CanSwitch() bool                       { return true }
func (b fakeBackend) Accounts() ([]claude.Account, error) { return b.accounts, nil }
func (fakeBackend) Capabilities() claude.Capabilities {
	return claude.Capabilities{Backend: "file", Profiles: true, Switch: true}
}

type fakeSessions struct {
	recent     []codex.Session
	recentErr  error
	resolved   *codex.Session
	resolveErr error
	nudged     []string
	nudgeErr   error
	released   []string
	closed     []int
	holders    func() []int
}

func (f *fakeSessions) Recent(int) ([]codex.Session, error) { return f.recent, f.recentErr }
func (f *fakeSessions) Resolve(string) (*codex.Session, error) {
	return f.resolved, f.resolveErr
}
func (f *fakeSessions) Nudge(_ context.Context, threadID, message string) (*codex.Nudged, error) {
	f.nudged = append(f.nudged, message)
	if f.nudgeErr != nil {
		return nil, f.nudgeErr
	}
	return &codex.Nudged{ThreadID: threadID, Queued: true, Message: message}, nil
}
func (f *fakeSessions) Release(threadID string, _ codex.Signaller) ([]int, error) {
	f.released = append(f.released, threadID)
	return f.closed, nil
}
func (f *fakeSessions) LockHolders(string) []int {
	if f.holders == nil {
		return nil
	}
	return f.holders()
}

// harness is an app whose every seam fails loudly unless a test replaces it,
// with a clock that only advances when something sleeps.
type harness struct {
	*app
	out, err *bytes.Buffer
	store    *fakeSessions
	clock    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, store: &fakeSessions{},
		clock: time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)}
	unexpected := func(name string) error { return errors.New(name + " should not have been called") }
	h.app = &app{
		stdout: h.out, stderr: h.err, stdin: strings.NewReader(""),
		stdoutIsTTY: func() bool { return false },
		stdinIsTTY:  func() bool { return false },
		now:         func() time.Time { return h.clock },
		sleep:       func(d time.Duration) { h.clock = h.clock.Add(d) },

		build:           func(context.Context) (collect.Snapshot, error) { return fixtureSnapshot(), nil },
		backend:         func() claude.Backend { return fakeBackend{} },
		runningSessions: func() int { return 4 },
		verify: func(context.Context, claude.Backend, string) (claude.VerifyResult, error) {
			return claude.VerifyResult{}, unexpected("verify")
		},
		execSession: func(claude.Backend, string, []string) error { return unexpected("exec") },
		launch: func(claude.Backend, string) (claude.LaunchResult, error) {
			return claude.LaunchResult{}, unexpected("launch")
		},
		switchDefault: func(claude.Backend, string, int) (claude.SwitchResult, error) {
			return claude.SwitchResult{}, unexpected("switch")
		},
		refresh: func(claude.Backend, string, bool) (claude.RefreshResult, error) {
			return claude.RefreshResult{}, unexpected("refresh")
		},
		expired:    func(claude.Account) bool { return false },
		modelUsage: func(int) *claude.ModelSummary { return nil },
		statusline: func() string { return "◆ work · user@example.com" },
		codexOverview: func(context.Context, bool) (*codex.Overview, error) {
			return nil, &codex.Error{Msg: "No Codex installation found at ~/.codex."}
		},
		sessions: func() sessionStore { return h.store },
		launchCommand: func([]string, string) (claude.LaunchCommandResult, error) {
			return claude.LaunchCommandResult{}, unexpected("launchCommand")
		},
		balances: func(context.Context, []string) ([]codex.Balance, error) {
			return nil, unexpected("balances")
		},
		codexAccount:   func(context.Context, codexAccountRequest) error { return unexpected("codexAccount") },
		clarpAvailable: func() bool { return true },
		stopped:        func(context.Context, int) ([]work.StoppedItem, error) { return nil, nil },
		continueItem: func(context.Context, work.StoppedItem) (work.Continuation, error) {
			return work.Continuation{}, unexpected("continue")
		},
		inspect: func(context.Context, string) (*work.Detail, error) {
			return nil, &work.InspectError{Msg: "nothing found for 'x'"}
		},
		clarpReport: func(context.Context, string, bool, string) (*clarp.Report, error) {
			return nil, unexpected("clarpReport")
		},
		runTUI: func(context.Context, tuiOptions) int { return 0 },
		bridge: func(context.Context, []string) int { return 0 },
	}
	return h
}

func (h *harness) run(argv ...string) int {
	h.out.Reset()
	h.err.Reset()
	return h.app.run(context.Background(), argv)
}

func (h *harness) json(t *testing.T) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, h.out.String())
	}
	return payload
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- list / show --------------------------------------------------------------

func TestListShowsTheAccount(t *testing.T) {
	h := newHarness(t)
	if code := h.run("list"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	for _, want := range []string{"work", "36%", "3 Claude session(s) running"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
}

func TestListJSONIsParseableAndCarriesNoToken(t *testing.T) {
	h := newHarness(t)
	if code := h.run("list", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	accounts := h.json(t)["accounts"].([]any)
	first := accounts[0].(map[string]any)
	if first["alias"] != "work" {
		t.Errorf("alias = %v", first["alias"])
	}
	if _, leaked := first["token"]; leaked {
		t.Error("token must never leave through --json")
	}
}

func TestImminentSigninIsCalledOut(t *testing.T) {
	h := newHarness(t)
	h.build = func(context.Context) (collect.Snapshot, error) {
		soon := fixtureSnapshot()
		soon.Accounts[0].SigninDaysLeft = ptrOf(2.0)
		soon.Accounts[0].SigninDueSoon = true
		return soon, nil
	}
	h.run("list")
	if !strings.Contains(strings.ToLower(h.out.String()), "sign-in needed soon") {
		t.Errorf("no warning in:\n%s", h.out.String())
	}
}

func TestListWithoutAccountsFails(t *testing.T) {
	h := newHarness(t)
	h.build = func(context.Context) (collect.Snapshot, error) {
		return collect.Snapshot{Error: ptrOf("credentials unreadable")}, nil
	}
	if code := h.run("list"); code != 1 || !strings.Contains(h.out.String(), "credentials unreadable") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestShowReportsOneAccount(t *testing.T) {
	h := newHarness(t)
	if code := h.run("show", "work"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Example Org", "binding limit  five hour", "access token   4.0h left", "extra usage    allowed"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
}

func TestShowUnknownAccountFails(t *testing.T) {
	h := newHarness(t)
	if code := h.run("show", "nope"); code != 1 || !strings.Contains(h.err.String(), "nope") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

func TestFlagsMayFollowPositionals(t *testing.T) {
	h := newHarness(t)
	if code := h.run("show", "work", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if h.json(t)["alias"] != "work" {
		t.Errorf("payload %v", h.json(t))
	}
}

// --- switch: retargets every running session, so it must never be quiet -------

func TestSwitchRefusesWithoutConfirmationWhenNotInteractive(t *testing.T) {
	h := newHarness(t)
	called := false
	h.switchDefault = func(claude.Backend, string, int) (claude.SwitchResult, error) {
		called = true
		return claude.SwitchResult{}, nil
	}
	code := h.run("switch", "other")
	if code != 1 || called {
		t.Errorf("exit %d, called %v", code, called)
	}
	if !strings.Contains(h.err.String(), "--yes") {
		t.Errorf("stderr %q", h.err.String())
	}
	for _, want := range []string{"4 running session", "hotseat use other"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in %q", want, h.out.String())
		}
	}
}

func TestSwitchYesProceeds(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.switchDefault = func(_ claude.Backend, alias string, sessions int) (claude.SwitchResult, error) {
		calls++
		return claude.SwitchResult{Alias: alias, Switched: true, SessionsAffected: sessions}, nil
	}
	if code := h.run("switch", "other", "--yes"); code != 0 || calls != 1 {
		t.Errorf("exit %d, calls %d", code, calls)
	}
	if !strings.Contains(h.out.String(), "Default account is now other.") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestSwitchInteractiveConfirmation(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   int
	}{{"y\n", 0}, {"yes\n", 0}, {"n\n", 1}, {"\n", 1}} {
		h := newHarness(t)
		h.stdinIsTTY = func() bool { return true }
		h.stdin = strings.NewReader(tc.answer)
		h.switchDefault = func(claude.Backend, string, int) (claude.SwitchResult, error) {
			return claude.SwitchResult{Switched: true}, nil
		}
		if code := h.run("switch", "other"); code != tc.want {
			t.Errorf("answer %q: exit %d, want %d", tc.answer, code, tc.want)
		}
	}
}

func TestFailedSwitchReportsFailure(t *testing.T) {
	h := newHarness(t)
	h.switchDefault = func(claude.Backend, string, int) (claude.SwitchResult, error) {
		return claude.SwitchResult{}, &claude.ActionError{Msg: "nope"}
	}
	if code := h.run("switch", "other", "--yes"); code != 1 || !strings.Contains(h.err.String(), "nope") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- use ------------------------------------------------------------------------

func TestUseReplacesTheProcessWithAPinnedSession(t *testing.T) {
	h := newHarness(t)
	var gotAlias string
	var gotArgs []string
	h.execSession = func(_ claude.Backend, alias string, args []string) error {
		gotAlias, gotArgs = alias, args
		return nil
	}
	h.run("use", "work", "--model", "haiku")
	if gotAlias != "work" || !reflect.DeepEqual(gotArgs, []string{"--model", "haiku"}) {
		t.Errorf("exec(%q, %v)", gotAlias, gotArgs)
	}
}

func TestUseFailureIsReportedRatherThanRaised(t *testing.T) {
	h := newHarness(t)
	h.execSession = func(claude.Backend, string, []string) error { return &claude.ActionError{Msg: "no token"} }
	if code := h.run("use", "work"); code != 1 || !strings.Contains(h.err.String(), "no token") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- verify ---------------------------------------------------------------------

func TestVerifySuccess(t *testing.T) {
	h := newHarness(t)
	h.verify = func(_ context.Context, _ claude.Backend, alias string) (claude.VerifyResult, error) {
		return claude.VerifyResult{Alias: alias, OK: true}, nil
	}
	if code := h.run("verify", "work"); code != 0 || !strings.Contains(h.out.String(), "live") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestVerifyFailureExitsNonzeroAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.verify = func(context.Context, claude.Backend, string) (claude.VerifyResult, error) {
		return claude.VerifyResult{}, &claude.ActionError{Msg: "expired"}
	}
	if code := h.run("verify", "work"); code != 1 || !strings.Contains(h.err.String(), "expired") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
	if code := h.run("verify", "work", "--json"); code != 1 {
		t.Errorf("json exit %d", code)
	}
	payload := h.json(t)
	if payload["ok"] != false || payload["error"] != "expired" {
		t.Errorf("payload %v", payload)
	}
}

// --- models ---------------------------------------------------------------------

func modelFixture(tokens int64, share float64) *claude.ModelSummary {
	return &claude.ModelSummary{Days: 7, AsOf: ptrOf("2026-01-01"), TotalTokens: tokens,
		Models: []claude.ModelUsage{{Model: "claude-fable-5-1", Label: "Fable 5.1", Family: "fable", Tokens: tokens, Share: share}}}
}

func TestModelsRenders(t *testing.T) {
	h := newHarness(t)
	h.modelUsage = func(int) *claude.ModelSummary { return modelFixture(1_500_000, 1.0) }
	if code := h.run("models"); code != 0 || !strings.Contains(h.out.String(), "Fable 5.1") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
	if !strings.Contains(h.out.String(), "1.5M tokens") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestSmallCountsAreNotRoundedAwayToZero(t *testing.T) {
	h := newHarness(t)
	h.modelUsage = func(int) *claude.ModelSummary { return modelFixture(225_000, 0.0001) }
	h.run("models")
	if !strings.Contains(h.out.String(), "225.0K") || strings.Contains(h.out.String(), "0M") {
		t.Errorf("out %q", h.out.String())
	}
	if !strings.Contains(h.out.String(), "<0.1%  ▏") {
		t.Errorf("tiny share should keep a tick: %q", h.out.String())
	}
}

func TestAbsentStatisticsFailCleanly(t *testing.T) {
	h := newHarness(t)
	if code := h.run("models"); code != 1 || !strings.Contains(h.err.String(), "No local usage statistics") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

func TestModelsDaysFlagIsPassedThrough(t *testing.T) {
	h := newHarness(t)
	var got int
	h.modelUsage = func(days int) *claude.ModelSummary { got = days; return modelFixture(1, 1) }
	h.run("models", "--days", "3")
	if got != 3 {
		t.Errorf("days = %d", got)
	}
}

// --- refresh --------------------------------------------------------------------

func TestRefreshWithNothingExpiredSaysSo(t *testing.T) {
	h := newHarness(t)
	h.backend = func() claude.Backend {
		return fakeBackend{accounts: []claude.Account{{Alias: "work"}, {Alias: "(signed in)"}}}
	}
	if code := h.run("refresh", "--json"); code != 0 || strings.TrimSpace(h.out.String()) != `{"refreshed": []}` {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
	h.run("refresh")
	if !strings.Contains(h.out.String(), "Every saved profile still has a usable token.") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestRefreshReportsEachProfileAndFailsOnAnyError(t *testing.T) {
	h := newHarness(t)
	h.refresh = func(_ claude.Backend, alias string, _ bool) (claude.RefreshResult, error) {
		if alias == "broken" {
			return claude.RefreshResult{}, &claude.RefreshError{Msg: "cli said no"}
		}
		return claude.RefreshResult{Alias: alias, Refreshed: true, Source: "cli", HoursLeft: 7.5}, nil
	}
	if code := h.run("refresh", "work", "broken"); code != 1 {
		t.Errorf("exit %d", code)
	}
	for _, want := range []string{"work: refreshed through the CLI, 7.5h left", "broken: cli said no"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in %q", want, h.out.String())
		}
	}
	h.run("refresh", "work", "broken", "--json")
	results := h.json(t)["refreshed"].([]any)
	if len(results) != 2 || results[1].(map[string]any)["error"] != "cli said no" {
		t.Errorf("results %v", results)
	}
}

func TestRefreshForceIncludesValidTokens(t *testing.T) {
	h := newHarness(t)
	h.backend = func() claude.Backend { return fakeBackend{accounts: []claude.Account{{Alias: "work"}}} }
	var forced bool
	h.refresh = func(_ claude.Backend, alias string, force bool) (claude.RefreshResult, error) {
		forced = force
		return claude.RefreshResult{Alias: alias, Source: "fresh", HoursLeft: 3}, nil
	}
	if code := h.run("refresh", "--force"); code != 0 || !forced {
		t.Errorf("exit %d, forced %v", code, forced)
	}
	if !strings.Contains(h.out.String(), "work: fresh, 3.0h left") {
		t.Errorf("out %q", h.out.String())
	}
}

// --- codex ----------------------------------------------------------------------

func codexFixture() *codex.Overview {
	return &codex.Overview{Accounts: []codex.Account{
		{Provider: "codex", Alias: "work", Saved: true, IsActive: true,
			Identity: codex.Identity{Email: ptrOf("w@example.com"), TokenHoursLeft: ptrOf(-1.0)},
			Usage:    &codex.Usage{Usable: true, WorstUsed: ptrOf(0.42)}},
		{Provider: "codex", Alias: "(live)", Saved: false, Identity: codex.Identity{Email: ptrOf("live@example.com")}},
	}, UnsavedLive: ptrOf("live@example.com"), QuotaRead: true, QuotaAgeS: ptrOf(1800.0)}
}

func TestCodexRenders(t *testing.T) {
	h := newHarness(t)
	h.codexOverview = func(context.Context, bool) (*codex.Overview, error) { return codexFixture(), nil }
	if code := h.run("codex"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"*work", "42%", "usable", "expired", "live (unsaved)", "unknown",
		"is not saved as a profile", "Quota figures are 30 minutes old"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
}

func TestCodexNoQuotaSkipsTheProbe(t *testing.T) {
	h := newHarness(t)
	var withLimits *bool
	h.codexOverview = func(_ context.Context, limits bool) (*codex.Overview, error) {
		withLimits = &limits
		return &codex.Overview{Accounts: []codex.Account{}}, nil
	}
	if code := h.run("codex", "--no-quota"); code != 1 || withLimits == nil || *withLimits {
		t.Errorf("exit %d, withLimits %v", code, withLimits)
	}
	if !strings.Contains(h.out.String(), "No Codex accounts found.") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestCodexAbsentFails(t *testing.T) {
	h := newHarness(t)
	if code := h.run("codex"); code != 1 || !strings.Contains(h.err.String(), "No Codex installation") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- sessions / nudge / reboot ----------------------------------------------------

func sessionFixture(live bool) *codex.Session {
	return &codex.Session{ThreadID: "0195abcd-thread", Short: "0195abcd", LastAt: 1.7e9, Turns: 4, Failed: 2,
		Topic: "Fix the flaky test", Holders: []int{4242}, Live: live, State: "stuck"}
}

func TestSessionsRenders(t *testing.T) {
	h := newHarness(t)
	h.store.recent = []codex.Session{*sessionFixture(true)}
	if code := h.run("sessions"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	for _, want := range []string{"0195abcd", "stuck", "Fix the flaky test", "1 stuck:", "hotseat reboot 0195abcd"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
	h.store.recent = nil
	h.run("sessions")
	if !strings.Contains(h.out.String(), "No recent Codex sessions.") {
		t.Errorf("out %q", h.out.String())
	}
	h.run("sessions", "--json")
	if strings.TrimSpace(h.out.String()) != "[]" {
		t.Errorf("empty list must be [] not null: %q", h.out.String())
	}
}

func TestNudgeRefusesADeadSession(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(false)
	code := h.run("nudge", "0195", "continue")
	if code != 1 || !strings.Contains(h.err.String(), "Reboot it instead: hotseat reboot 0195abcd") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
	if len(h.store.nudged) != 0 {
		t.Error("nothing should be queued for a dead session")
	}
}

func TestNudgeQueuesForALiveSession(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(true)
	if code := h.run("nudge", "0195", "continue"); code != 0 || !strings.Contains(h.out.String(), "✓ queued for 0195abcd: continue") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
	if !reflect.DeepEqual(h.store.nudged, []string{"continue"}) {
		t.Errorf("nudged %v", h.store.nudged)
	}
}

func TestRebootRefusesALiveSessionWithoutYesWhenNotInteractive(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(true)
	if code := h.run("reboot", "0195"); code != 1 || !strings.Contains(h.err.String(), "--yes") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
	if !strings.Contains(h.out.String(), "held by process(es) 4242") || len(h.store.released) != 0 {
		t.Errorf("out %q, released %v", h.out.String(), h.store.released)
	}
}

func TestRebootClosesWaitsAndResumes(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(true)
	h.store.closed = []int{4242}
	locked := true
	h.store.holders = func() []int {
		if locked {
			return []int{4242}
		}
		return nil
	}
	h.sleep = func(d time.Duration) {
		h.clock = h.clock.Add(d)
		locked = false // the holder exits during the first wait
	}
	var launched []string
	var launchedIn string
	h.launchCommand = func(argv []string, cwd string) (claude.LaunchCommandResult, error) {
		launched, launchedIn = argv, cwd
		return claude.LaunchCommandResult{Terminal: "kitty", Started: true}, nil
	}
	if code := h.run("reboot", "0195", "--yes", "--cwd", "/work"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if !reflect.DeepEqual(launched, []string{"codex", "resume", "0195abcd-thread"}) || launchedIn != "/work" {
		t.Errorf("launched %v in %q", launched, launchedIn)
	}
	if !reflect.DeepEqual(h.store.released, []string{"0195abcd-thread"}) {
		t.Errorf("released %v", h.store.released)
	}
	for _, want := range []string{"Closed 4242.", "✓ resumed 0195abcd in a new kitty window"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in %q", want, h.out.String())
		}
	}
}

func TestRebootWaitsForTheLockAtMostTenSeconds(t *testing.T) {
	h := newHarness(t)
	h.store.holders = func() []int { return []int{1} }
	start := h.clock
	if h.waitForLock("t", 10*time.Second) {
		t.Error("lock never cleared")
	}
	if waited := h.clock.Sub(start); waited < 10*time.Second || waited > 11*time.Second {
		t.Errorf("waited %v", waited)
	}
}

func TestRebootSendsTheMessageOnceAttached(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(false)
	attached := false
	h.store.holders = func() []int {
		if attached {
			return []int{99}
		}
		return nil
	}
	h.sleep = func(d time.Duration) { h.clock = h.clock.Add(d); attached = true }
	h.launchCommand = func([]string, string) (claude.LaunchCommandResult, error) {
		return claude.LaunchCommandResult{Terminal: "kitty", Started: true}, nil
	}
	if code := h.run("reboot", "0195", "--message", "carry on"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if !reflect.DeepEqual(h.store.nudged, []string{"carry on"}) || !strings.Contains(h.out.String(), "✓ sent: carry on") {
		t.Errorf("nudged %v, out %q", h.store.nudged, h.out.String())
	}
}

func TestRebootGivesUpWhenItNeverAttaches(t *testing.T) {
	h := newHarness(t)
	h.store.resolved = sessionFixture(false)
	h.launchCommand = func([]string, string) (claude.LaunchCommandResult, error) {
		return claude.LaunchCommandResult{Terminal: "kitty", Started: true}, nil
	}
	start := h.clock
	if code := h.run("reboot", "0195", "--message", "carry on"); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.err.String(), "did not attach in time") || h.clock.Sub(start) < 45*time.Second {
		t.Errorf("stderr %q after %v", h.err.String(), h.clock.Sub(start))
	}
}

// --- resets -----------------------------------------------------------------------

func TestResetsJSONPartialFailure(t *testing.T) {
	h := newHarness(t)
	var asked []string
	h.balances = func(_ context.Context, aliases []string) ([]codex.Balance, error) {
		asked = aliases
		return []codex.Balance{{Alias: "work", Reading: codex.Reading{Error: ptrOf("HTTP 401")}}}, nil
	}
	if code := h.run("resets", "work", "--json"); code != 1 || !reflect.DeepEqual(asked, []string{"work"}) {
		t.Errorf("exit %d, asked %v", code, asked)
	}
	rows := h.json(t)["accounts"].([]any)
	row := rows[0].(map[string]any)
	if row["alias"] != "work" || row["error"] != "HTTP 401" || row["available_count"] != nil {
		t.Errorf("row %v", row)
	}
}

func TestResetsUnknownProfileFails(t *testing.T) {
	h := newHarness(t)
	h.balances = func(context.Context, []string) ([]codex.Balance, error) {
		return nil, &codex.Error{Msg: "Unknown Codex profiles: ../other"}
	}
	if code := h.run("resets", "../other"); code != 1 || !strings.Contains(h.err.String(), "Unknown Codex profiles") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

func TestResetsTable(t *testing.T) {
	h := newHarness(t)
	h.balances = func(context.Context, []string) ([]codex.Balance, error) {
		return []codex.Balance{{Alias: "work", Reading: codex.Reading{AvailableCount: ptrOf(2)}}}, nil
	}
	if code := h.run("resets"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "work") || !strings.Contains(h.out.String(), "     2  ok") {
		t.Errorf("out %q", h.out.String())
	}
}

// --- resume -------------------------------------------------------------------------

func clarpItem() work.StoppedItem {
	return work.StoppedItem{Kind: "clarp", ID: "fixture", Name: "Fixture", Backend: "codex", Cause: "usage_limit"}
}

// Ported from plugins/clarp/tests/test_resume_regression.py.
func TestResumeGoJSONReportsSuccessOrFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		h := newHarness(t)
		h.build = func(context.Context) (collect.Snapshot, error) { return collect.Snapshot{}, nil }
		h.stopped = func(context.Context, int) ([]work.StoppedItem, error) { return []work.StoppedItem{clarpItem()}, nil }
		calls := 0
		h.continueItem = func(_ context.Context, item work.StoppedItem) (work.Continuation, error) {
			calls++
			if failure {
				return work.Continuation{}, &work.ResumeError{Msg: "fixture failure"}
			}
			return work.Continuation{ID: item.ID, Kind: "clarp", Continued: true}, nil
		}
		code := h.run("resume", "--go", "--json")
		if calls != 1 {
			t.Errorf("failure=%v: continue called %d times", failure, calls)
		}
		want := 0
		if failure {
			want = 1
		}
		if code != want {
			t.Errorf("failure=%v: exit %d", failure, code)
		}
		result := h.json(t)["results"].([]any)[0].(map[string]any)
		if result["continued"] != !failure {
			t.Errorf("failure=%v: result %v", failure, result)
		}
		if failure && result["error"] != "fixture failure" {
			t.Errorf("result %v", result)
		}
	}
}

func TestResumeHumanListingWithClarp(t *testing.T) {
	h := newHarness(t)
	native := work.StoppedItem{Kind: "native", ID: "abc123", Name: "abc123", Backend: "claude",
		Cause: "usage_limit", CWD: "/work/repo", StoppedAt: 1.7e9, LastMessage: "half done"}
	paused := clarpItem()
	paused.ID, paused.Name, paused.Cause, paused.Pending = "theo", "Theo", "queue_paused", 2
	h.stopped = func(context.Context, int) ([]work.StoppedItem, error) {
		return []work.StoppedItem{paused, clarpItem(), native}, nil
	}
	if code := h.run("resume"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	out := h.out.String()
	for _, want := range []string{"Theo · 2 waiting", "queue is paused", "said: half done", "needs a terminal",
		"1 Clarp agent(s) can be continued now: hotseat resume --go",
		"1 native session(s) have no supervisor to prompt", "cd /work/repo && claude --resume abc123",
		"1 agent(s) have a paused queue, holding 2 turn(s) that will not run"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	calls := 0
	h.continueItem = func(_ context.Context, item work.StoppedItem) (work.Continuation, error) {
		calls++
		return work.Continuation{ID: item.ID, Kind: "clarp", Continued: true}, nil
	}
	if code := h.run("resume", "--go"); code != 0 || calls != 1 {
		t.Errorf("exit %d, continued %d", code, calls)
	}
	if !strings.Contains(h.out.String(), "✓ continued Fixture") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestResumeNothingStopped(t *testing.T) {
	h := newHarness(t)
	h.run("resume", "--days", "3")
	if !strings.Contains(h.out.String(), "Nothing was stopped by a usage limit in the last 3 days.") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestResumeFiltersByKindAndCause(t *testing.T) {
	h := newHarness(t)
	native := work.StoppedItem{Kind: "native", ID: "abc", Name: "abc", Backend: "claude", Cause: "usage_limit"}
	h.stopped = func(context.Context, int) ([]work.StoppedItem, error) {
		return []work.StoppedItem{clarpItem(), native}, nil
	}
	h.run("resume", "--json", "--kind", "native")
	if items := h.json(t)["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != "abc" {
		t.Errorf("items %v", items)
	}
	h.run("resume", "--json", "--cause", "queue_paused")
	if items := h.json(t)["items"].([]any); len(items) != 0 {
		t.Errorf("items %v", items)
	}
	if code := h.run("resume", "--cause", "bogus"); code != 2 || !strings.Contains(h.err.String(), "invalid choice") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// Without Clarp state the core command applies: native sessions only, and --go
// hands back the resume commands instead of running anything.
func TestResumeWithoutClarpIsTheCoreCommand(t *testing.T) {
	h := newHarness(t)
	h.clarpAvailable = func() bool { return false }
	native := work.StoppedItem{Kind: "native", ID: "abc", Name: "abc", Backend: "claude", Cause: "usage_limit", CWD: "/w"}
	h.stopped = func(context.Context, int) ([]work.StoppedItem, error) { return []work.StoppedItem{native}, nil }
	h.continueItem = func(_ context.Context, item work.StoppedItem) (work.Continuation, error) {
		return work.ContinueItem(item)
	}
	if code := h.run("resume", "--go"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"abc · /w", "  claude --resume abc", "Native sessions require a terminal"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in %q", want, h.out.String())
		}
	}
	h.run("resume", "--go", "--json")
	results := h.json(t)["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results %v", results)
	}
	result := results[0].(map[string]any)
	if got := sortedKeys(result); !reflect.DeepEqual(got, []string{"command", "continued", "cwd", "id", "kind"}) {
		t.Errorf("keys %v", got)
	}
	if result["command"] != "claude --resume abc" || result["continued"] != false {
		t.Errorf("result %v", result)
	}
}

// --- inspect --------------------------------------------------------------------------

func detailFixture() *work.Detail {
	return &work.Detail{Kind: "clarp", ID: "theo", Name: "Theo", Backend: "codex", Model: ptrOf("gpt-5"),
		CWD: ptrOf("/work"), Branch: ptrOf("main"), LastUser: "ship it", Summary: "Shipping the release",
		LastAssistant: "done", LastActivity: ptrOf("2026-01-01 11:00"),
		Queued: []any{clarp.QueuedTurn{Origin: nil, Text: "queued text"}},
		Recent: []work.RecentTurn{{Role: "user", Text: "ship it"}, {Role: "assistant", Text: "done"}}}
}

func TestInspectRenders(t *testing.T) {
	h := newHarness(t)
	h.inspect = func(context.Context, string) (*work.Detail, error) { return detailFixture(), nil }
	if code := h.run("inspect", "theo", "--full"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Theo  clarp · codex · gpt-5", "working in /work (main)", "last activity 2026-01-01 11:00",
		"What it was working on", "Shipping the release", "Last asked", "Last said", "Queued and waiting (unknown)",
		"queued text", "Recent exchange", "you: ship it", "Theo: done"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
	h.run("inspect", "theo")
	if strings.Contains(h.out.String(), "Recent exchange") {
		t.Error("--full is opt-in")
	}
}

func TestInspectUnknownFails(t *testing.T) {
	h := newHarness(t)
	if code := h.run("inspect", "x"); code != 1 || !strings.Contains(h.err.String(), "nothing found for 'x'") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- clarp ------------------------------------------------------------------------------

func clarpFixture() *clarp.Report {
	agents := []clarp.Agent{
		{Session: "theo-1", Persona: ptrOf("Theo"), Backend: ptrOf("claude"), Live: true, Account: ptrOf("work"), Pinned: true},
		{Session: "nadia-2", Persona: ptrOf("Nadia"), Backend: ptrOf("claude"), WouldUse: ptrOf("work")},
		{Session: "axel-3", Persona: ptrOf("Axel"), Backend: ptrOf("codex")},
	}
	return &clarp.Report{Overview: &clarp.Overview{Agents: agents, Total: 3, Live: 1,
		ByBackend: map[string]int{"claude": 2, "codex": 1}, ClaudeBacked: 2, LiveByAccount: map[string]int{"work": 1}},
		Agents: agents}
}

func TestClarpRenders(t *testing.T) {
	h := newHarness(t)
	var gotDefault, gotBackend string
	var gotLive bool
	h.clarpReport = func(_ context.Context, defaultAlias string, live bool, backend string) (*clarp.Report, error) {
		gotDefault, gotLive, gotBackend = defaultAlias, live, backend
		return clarpFixture(), nil
	}
	if code := h.run("clarp", "--live", "--backend", "claude"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if gotDefault != "work" || !gotLive || gotBackend != "claude" {
		t.Errorf("report(%q, %v, %q)", gotDefault, gotLive, gotBackend)
	}
	for _, want := range []string{"Theo", "live", "work (pinned)", "would use work", "3 agents (2 claude, 1 codex); 1 live.",
		"Live Claude agents by account — work: 1"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
}

func TestClarpEmptyMessagesDependOnFilters(t *testing.T) {
	h := newHarness(t)
	h.clarpReport = func(_ context.Context, _ string, live bool, backend string) (*clarp.Report, error) {
		return &clarp.Report{Overview: &clarp.Overview{}, Filtered: live || backend != ""}, nil
	}
	h.run("clarp")
	if !strings.Contains(h.out.String(), "No Clarp agents defined.") {
		t.Errorf("out %q", h.out.String())
	}
	h.run("clarp", "--live")
	if !strings.Contains(h.out.String(), "No Clarp agents match.") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestClarpWithoutStateSaysSo(t *testing.T) {
	h := newHarness(t)
	h.clarpAvailable = func() bool { return false }
	if code := h.run("clarp"); code != 1 || !strings.Contains(h.err.String(), "not installed") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- codex-account ------------------------------------------------------------------------

func TestCodexAccountParsing(t *testing.T) {
	cases := []struct {
		args    []string
		want    codexAccountRequest
		wantErr string
	}{
		{[]string{"save", "work"}, codexAccountRequest{Command: "save", Name: "work"}, ""},
		{[]string{"list"}, codexAccountRequest{Command: "list"}, ""},
		{[]string{"switch", "work"}, codexAccountRequest{Command: "switch", Name: "work"}, ""},
		{[]string{"current"}, codexAccountRequest{Command: "current"}, ""},
		{[]string{"login", "work", "--email", "w@example.com", "--browser", "-f"},
			codexAccountRequest{Command: "login", Name: "work", Email: "w@example.com", Browser: true, Force: true}, ""},
		{[]string{"login", "--email=w@example.com", "work", "--force"},
			codexAccountRequest{Command: "login", Name: "work", Email: "w@example.com", Force: true}, ""},
		{[]string{"login", "work"}, codexAccountRequest{}, "required: --email"},
		{[]string{"verify", "work"}, codexAccountRequest{Command: "verify", Name: "work"}, ""},
		{[]string{"probe"}, codexAccountRequest{Command: "probe"}, ""},
		{[]string{"probe", "work", "--json"}, codexAccountRequest{Command: "probe", Name: "work", JSON: true}, ""},
		{[]string{"remove", "work"}, codexAccountRequest{Command: "remove", Name: "work"}, ""},
		{[]string{}, codexAccountRequest{}, "required: cmd"},
		{[]string{"bogus"}, codexAccountRequest{}, "invalid choice: 'bogus'"},
		{[]string{"save"}, codexAccountRequest{}, "required: name"},
		{[]string{"list", "extra"}, codexAccountRequest{}, "unrecognized arguments: extra"},
		{[]string{"save", "work", "--json"}, codexAccountRequest{}, "unrecognized arguments: --json"},
	}
	for _, tc := range cases {
		got, err := parseCodexAccount(tc.args)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%v: err %v, want %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%v: got %+v want %+v", tc.args, got, tc.want)
		}
	}
}

func TestCodexAccountDispatchesAndReportsFailures(t *testing.T) {
	h := newHarness(t)
	var got codexAccountRequest
	h.codexAccount = func(_ context.Context, request codexAccountRequest) error {
		got = request
		if request.Command == "switch" {
			return &codex.ProfileError{Msg: "no saved profile 'nope'"}
		}
		return nil
	}
	if code := h.run("codex-account", "save", "work"); code != 0 || got.Name != "work" {
		t.Errorf("exit %d, request %+v", code, got)
	}
	if code := h.run("codex-account", "switch", "nope"); code != 1 || h.err.String() != "error: no saved profile 'nope'\n" {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
	if code := h.run("codex-account"); code != 2 || !strings.Contains(h.err.String(), "required: cmd") {
		t.Errorf("exit %d, stderr %q", code, h.err.String())
	}
}

// --- statusline / tui / bridge --------------------------------------------------------------

func TestStatuslinePrintsTheLabelOrNothing(t *testing.T) {
	h := newHarness(t)
	h.run("statusline")
	if h.out.String() != "◆ work · user@example.com\n" {
		t.Errorf("out %q", h.out.String())
	}
	h.statusline = func() string { return "" }
	h.run("statusline")
	if h.out.Len() != 0 {
		t.Errorf("out %q", h.out.String())
	}
}

func TestTUIFlagsReachTheProgram(t *testing.T) {
	h := newHarness(t)
	var got tuiOptions
	h.runTUI = func(_ context.Context, options tuiOptions) int { got = options; return 0 }
	h.run("tui", "--demo")
	if !got.Demo {
		t.Errorf("options %+v", got)
	}
	h.run("tui", "--render", "--demo-file", "f.json", "--width", "80", "--height", "24")
	if !got.Render || got.DemoFile != "f.json" || got.Width != 80 || got.Height != 24 {
		t.Errorf("options %+v", got)
	}
}

func TestBridgeStaysReachableForTheCompareScript(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.bridge = func(_ context.Context, args []string) int { got = args; return 0 }
	h.run("bridge", "snapshot", "--refresh")
	if !reflect.DeepEqual(got, []string{"snapshot", "--refresh"}) {
		t.Errorf("args %v", got)
	}
}

// --- usage ------------------------------------------------------------------------------------

func TestBareInvocationPrintsHelpAndSignalsUsageError(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != 2 || !strings.Contains(h.out.String(), "usage:") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
	if code := h.run("--help"); code != 0 || !strings.Contains(h.out.String(), "Inspect and use the Claude Code") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestEveryCommandIsReachable(t *testing.T) {
	for _, expected := range []string{"tui", "list", "show", "verify", "use", "window", "refresh", "switch", "models",
		"inspect", "resume", "sessions", "nudge", "reboot", "resets", "codex-account", "codex", "statusline", "clarp"} {
		if lookup(expected) == nil {
			t.Errorf("%s is not wired", expected)
		}
	}
	if lookup("serve") != nil {
		t.Error("serve is not ported (design decision 10)")
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--version"); code != 0 || h.out.String() != "hotseat "+version.Version+"\n" {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"bogus"}, "invalid choice: 'bogus'"},
		{[]string{"show"}, "the following arguments are required: alias"},
		{[]string{"nudge", "id"}, "the following arguments are required: message"},
		{[]string{"list", "extra"}, "unrecognized arguments: extra"},
		{[]string{"list", "--bogus"}, "flag provided but not defined"},
		{[]string{"use"}, "the following arguments are required: alias"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		if code := h.run(tc.argv...); code != 2 || !strings.Contains(h.err.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %q", tc.argv, code, h.err.String())
		}
	}
	h := newHarness(t)
	if code := h.run("list", "-h"); code != 0 || !strings.Contains(h.out.String(), "usage: hotseat list [-h] [--json]") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

// --- --json shapes ------------------------------------------------------------------------------

// Every --json output keeps the Python's keys. This table pins the top-level
// shape of each; the nested shapes are the structs' own tests.
func TestJSONShapes(t *testing.T) {
	setup := func(h *harness) {
		h.verify = func(_ context.Context, _ claude.Backend, alias string) (claude.VerifyResult, error) {
			return claude.VerifyResult{Alias: alias, OK: true, Usage: quota.Summary{}}, nil
		}
		h.launch = func(_ claude.Backend, alias string) (claude.LaunchResult, error) {
			return claude.LaunchResult{Alias: alias, Terminal: "kitty", Started: true, ConfigDir: "/d"}, nil
		}
		h.switchDefault = func(_ claude.Backend, alias string, sessions int) (claude.SwitchResult, error) {
			return claude.SwitchResult{Alias: alias, Switched: true, SessionsAffected: sessions}, nil
		}
		h.refresh = func(_ claude.Backend, alias string, _ bool) (claude.RefreshResult, error) {
			return claude.RefreshResult{Alias: alias, Source: "fresh"}, nil
		}
		h.modelUsage = func(int) *claude.ModelSummary { return modelFixture(1, 1) }
		h.codexOverview = func(context.Context, bool) (*codex.Overview, error) { return codexFixture(), nil }
		h.store.recent = []codex.Session{*sessionFixture(true)}
		h.store.resolved = sessionFixture(false)
		h.launchCommand = func([]string, string) (claude.LaunchCommandResult, error) {
			return claude.LaunchCommandResult{Terminal: "kitty", Started: true}, nil
		}
		h.balances = func(context.Context, []string) ([]codex.Balance, error) {
			return []codex.Balance{{Alias: "work", Reading: codex.Reading{AvailableCount: ptrOf(1)}}}, nil
		}
		h.stopped = func(context.Context, int) ([]work.StoppedItem, error) { return []work.StoppedItem{clarpItem()}, nil }
		h.continueItem = func(_ context.Context, item work.StoppedItem) (work.Continuation, error) {
			return work.Continuation{ID: item.ID, Kind: "clarp", Continued: true}, nil
		}
		h.inspect = func(context.Context, string) (*work.Detail, error) { return detailFixture(), nil }
		h.clarpReport = func(context.Context, string, bool, string) (*clarp.Report, error) { return clarpFixture(), nil }
	}
	cases := []struct {
		argv []string
		keys []string
		// sub picks a nested object to check instead: a key, then optionally an
		// index into the list under it.
		sub     string
		subKeys []string
	}{
		{[]string{"list", "--json"}, []string{"accounts", "capabilities", "codex", "error", "extensions", "generated_at",
			"model_usage", "sessions", "stopped"}, "", nil},
		{[]string{"show", "work", "--json"}, []string{"access_expires_at", "access_hours_left", "alias", "email", "error",
			"is_active", "org", "plan", "plan_label", "rate_limit_tier", "refresh_expires_at", "signin_days_left",
			"signin_due_soon", "usage"}, "", nil},
		{[]string{"verify", "work", "--json"}, []string{"alias", "ok", "usage"}, "", nil},
		{[]string{"window", "work", "--json"}, []string{"alias", "config_dir", "started", "terminal"}, "", nil},
		{[]string{"switch", "work", "--yes", "--json"}, []string{"alias", "sessions_affected", "switched"}, "", nil},
		{[]string{"refresh", "work", "--json"}, []string{"refreshed"}, "refreshed",
			[]string{"alias", "expires_at", "hours_left", "refreshed", "source"}},
		{[]string{"models", "--json"}, []string{"as_of", "days", "models", "stale", "total_tokens"}, "models",
			[]string{"family", "label", "model", "share", "tokens"}},
		{[]string{"codex", "--json"}, []string{"accounts", "quota_age_s", "quota_read", "unsaved_live"}, "accounts",
			[]string{"account_id", "alias", "api_key_present", "email", "is_active", "plan", "provider", "saved",
				"token_expires_at", "token_hours_left", "usage"}},
		{[]string{"nudge", "x", "hi", "--json"}, nil, "", nil}, // dead session: exits 1, no JSON
		{[]string{"reboot", "x", "--json"}, []string{"closed", "started", "terminal", "thread_id"}, "", nil},
		{[]string{"resets", "--json"}, []string{"accounts"}, "accounts",
			[]string{"alias", "available_count", "checked_at", "email", "error"}},
		{[]string{"resume", "--json"}, []string{"default_account", "items"}, "items",
			[]string{"backend", "cause", "cwd", "id", "kind", "last_message", "name", "pending", "readiness", "stopped_at"}},
		{[]string{"resume", "--go", "--json"}, []string{"default_account", "items", "results"}, "results",
			[]string{"continued", "id", "kind"}},
		{[]string{"inspect", "theo", "--json"}, []string{"backend", "branch", "cwd", "id", "kind", "last_activity",
			"last_assistant", "last_user", "model", "name", "queued", "recent", "summary", "turns"}, "", nil},
		{[]string{"clarp", "--json"}, []string{"agents", "by_backend", "claude_backed", "live", "live_by_account", "total"},
			"agents", []string{"account", "backend", "cwd", "live", "persona", "pid", "pinned", "session", "would_use"}},
	}
	for _, tc := range cases {
		h := newHarness(t)
		setup(h)
		code := h.run(tc.argv...)
		if tc.keys == nil {
			if code == 0 {
				t.Errorf("%v: expected failure", tc.argv)
			}
			continue
		}
		if code != 0 {
			t.Errorf("%v: exit %d: %s", tc.argv, code, h.err.String())
			continue
		}
		payload := h.json(t)
		if got := sortedKeys(payload); !reflect.DeepEqual(got, tc.keys) {
			t.Errorf("%v: keys %v, want %v", tc.argv, got, tc.keys)
		}
		if tc.sub == "" {
			continue
		}
		list, ok := payload[tc.sub].([]any)
		if !ok || len(list) == 0 {
			t.Errorf("%v: %s is %v", tc.argv, tc.sub, payload[tc.sub])
			continue
		}
		if got := sortedKeys(list[0].(map[string]any)); !reflect.DeepEqual(got, tc.subKeys) {
			t.Errorf("%v: %s[0] keys %v, want %v", tc.argv, tc.sub, got, tc.subKeys)
		}
	}
}

// The sessions listing is a bare JSON list, so it gets its own check.
func TestSessionsJSONShape(t *testing.T) {
	h := newHarness(t)
	h.store.recent = []codex.Session{*sessionFixture(true)}
	if code := h.run("sessions", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var found []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &found); err != nil {
		t.Fatal(err)
	}
	want := []string{"active", "failed", "holders", "last_at", "last_status", "live", "short", "state", "thread_id", "topic", "turns"}
	if got := sortedKeys(found[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("keys %v", got)
	}
}
