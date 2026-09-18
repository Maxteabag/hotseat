package codex

// Codex account discovery and quota shaping.
//
// Identity comes from JWT claims, so the fixtures build real (unsigned) tokens.
// Nothing here runs the live probe or touches the real ~/.codex.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var (
	future = float64(time.Now().Unix() + 3600)
	past   = float64(time.Now().Unix() - 3600)
)

// idToken builds an unsigned JWT with the claims Codex stores.
func idToken(email, plan, accountID string, exp float64) string {
	claims := map[string]any{"email": email, "exp": exp,
		authClaim: map[string]any{"chatgpt_plan_type": plan, "chatgpt_account_id": accountID}}
	raw, _ := json.Marshal(claims)
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

type accountsFixture struct {
	t        *testing.T
	home     string
	profiles string
	codex    *Codex
}

func newAccountsFixture(t *testing.T) *accountsFixture {
	t.Helper()
	home := t.TempDir()
	profiles := filepath.Join(home, "profiles")
	if err := os.Mkdir(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	return &accountsFixture{t: t, home: home, profiles: profiles, codex: New(home)}
}

func (f *accountsFixture) write(path, email, accountID string, exp float64) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token": idToken(email, "pro", accountID, exp), "account_id": accountID}})
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *accountsFixture) profile(name string) string {
	return filepath.Join(f.profiles, name, "auth.json")
}

func TestClaimsAreDecoded(t *testing.T) {
	out := Claims(idToken("user@example.com", "pro", "acc-1", future))
	if out["email"] != "user@example.com" {
		t.Fatalf("email = %v", out["email"])
	}
	auth := out[authClaim].(map[string]any)
	if auth["chatgpt_plan_type"] != "pro" || auth["chatgpt_account_id"] != "acc-1" {
		t.Fatalf("auth claims = %v", auth)
	}
}

func TestAMalformedTokenIsNotFatal(t *testing.T) {
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c", "a.W10.c"} {
		if out := Claims(bad); len(out) != 0 {
			t.Errorf("Claims(%q) = %v, want empty", bad, out)
		}
	}
}

func TestSavedProfilesAreListed(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.write(f.profile("home"), "home@example.com", "acc-2", future)
	if got := f.codex.SavedProfiles(); !reflect.DeepEqual(got, []string{"home", "work"}) {
		t.Fatalf("SavedProfiles = %v", got)
	}
}

func TestAProfileMatchingTheLiveCredentialIsMarkedActive(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.write(f.codex.LiveAuth(), "work@example.com", "acc-1", future)
	found := f.codex.Accounts()
	if len(found) != 1 {
		t.Fatalf("the same account must not appear twice: %+v", found)
	}
	if !found[0].IsActive || !found[0].Saved {
		t.Fatalf("entry = %+v", found[0])
	}
}

func TestAnUnsavedLiveAccountIsNamedRatherThanHidden(t *testing.T) {
	// Losing it to a profile switch is exactly what happened in practice.
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.write(f.codex.LiveAuth(), "other@example.com", "acc-2", future)
	found := f.codex.Accounts()
	if len(found) != 2 {
		t.Fatalf("len = %d", len(found))
	}
	live := found[0]
	if live.Saved || !live.IsActive || live.Alias != LiveName || live.Email == nil || *live.Email != "other@example.com" {
		t.Fatalf("live = %+v", live)
	}
	if live.Provider != "codex" {
		t.Fatalf("provider = %q", live.Provider)
	}
}

func TestTheLiveAccountIsNotMatchedOnEmailAlone(t *testing.T) {
	// Two accounts can share an address across different organisations.
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "same@example.com", "acc-1", future)
	f.write(f.codex.LiveAuth(), "same@example.com", "acc-2", future)
	if found := f.codex.Accounts(); len(found) != 2 {
		t.Fatalf("len = %d", len(found))
	}
}

func TestAnExpiredStoredTokenIsReportedNotHidden(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", past)
	entry := f.codex.Accounts()[0]
	if entry.TokenHoursLeft == nil || *entry.TokenHoursLeft >= 0 {
		t.Fatalf("token_hours_left = %v", entry.TokenHoursLeft)
	}
	if entry.TokenExpiresAt == nil || *entry.TokenExpiresAt != past {
		t.Fatalf("token_expires_at = %v", entry.TokenExpiresAt)
	}
}

func TestADamagedProfileIsSkippedRatherThanCrashing(t *testing.T) {
	f := newAccountsFixture(t)
	if err := os.Mkdir(filepath.Join(f.profiles, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.profile("broken"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	found := f.codex.Accounts()
	if len(found) != 1 || found[0].Alias != "work" {
		t.Fatalf("accounts = %+v", found)
	}
}

func TestNoInstallationIsAClearError(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "absent"))
	_, err := c.Overview(context.Background(), true, 0)
	var codexErr *Error
	if !errors.As(err, &codexErr) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if codexErr.Msg != "No Codex installation found at ~/.codex." {
		t.Fatalf("message = %q", codexErr.Msg)
	}
}

func TestAccountRowJSONShape(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.codex.now = func() time.Time { return time.Unix(int64(future)-3600, 0) }
	raw, _ := json.Marshal(f.codex.Accounts()[0])
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	for _, key := range []string{"provider", "alias", "is_active", "saved", "email", "plan", "account_id",
		"token_expires_at", "token_hours_left", "api_key_present", "usage"} {
		if _, ok := row[key]; !ok {
			t.Errorf("row lacks %q: %s", key, raw)
		}
	}
	if row["plan"] != "pro" || row["token_hours_left"] != 1.0 || row["api_key_present"] != false {
		t.Fatalf("row = %s", raw)
	}
}

// Probing spawns one `codex app-server` per saved account, one at a time.
//
// On a refresh timer that is a process per account every cycle, which is far
// too much for a panel. The windows are hourly and weekly, so a slightly stale
// reading is fine and a fresh probe is not.
type cacheFixture struct {
	codex *Codex
	calls int
	now   time.Time
}

func newCacheFixture(t *testing.T) *cacheFixture {
	f := &cacheFixture{codex: New(t.TempDir()), now: time.Unix(1_700_000_000, 0)}
	f.codex.limitsFn = func(context.Context) map[string]Usage {
		f.calls++
		return map[string]Usage{"work": {Usable: true}}
	}
	f.codex.now = func() time.Time {
		f.now = f.now.Add(time.Millisecond)
		return f.now
	}
	return f
}

func (f *cacheFixture) backdate(age time.Duration) {
	f.codex.cache = &limitsCache{fetchedAt: f.now.Add(-age), readings: map[string]Usage{"work": {Usable: true}}}
}

func TestRepeatedBackgroundReadsProbeOnce(t *testing.T) {
	f := newCacheFixture(t)
	for range 5 {
		f.codex.CachedLimits(context.Background(), LimitsTTL)
	}
	if f.calls != 1 {
		t.Fatalf("a refresh timer must not re-probe: %d calls", f.calls)
	}
}

func TestAnExpiredEntryIsRefreshed(t *testing.T) {
	f := newCacheFixture(t)
	f.codex.CachedLimits(context.Background(), LimitsTTL)
	f.backdate(3600 * time.Second)
	f.codex.CachedLimits(context.Background(), LimitsTTL)
	if f.calls != 2 {
		t.Fatalf("calls = %d", f.calls)
	}
}

func TestAZeroMaxAgeAlwaysProbes(t *testing.T) {
	// The command line asks for current numbers, not a cached panel.
	f := newCacheFixture(t)
	f.codex.CachedLimits(context.Background(), 0)
	f.codex.CachedLimits(context.Background(), 0)
	if f.calls != 2 {
		t.Fatalf("calls = %d", f.calls)
	}
}

func TestTheAgeOfTheReturnedReadingIsReported(t *testing.T) {
	f := newCacheFixture(t)
	_, age, known := f.codex.CachedLimits(context.Background(), LimitsTTL)
	if !known || age != 0 {
		t.Fatalf("age = %v known = %v", age, known)
	}
	f.backdate(300 * time.Second)
	_, age, known = f.codex.CachedLimits(context.Background(), LimitsTTL)
	if !known || age <= 299 {
		t.Fatalf("age = %v known = %v", age, known)
	}
}

func TestAFailedProbeIsNotCached(t *testing.T) {
	// Caching a failure would hide a recovery for the rest of the window.
	f := newCacheFixture(t)
	f.codex.limitsFn = func(context.Context) map[string]Usage { return map[string]Usage{} }
	found, _, known := f.codex.CachedLimits(context.Background(), LimitsTTL)
	if len(found) != 0 || known {
		t.Fatalf("found = %v known = %v", found, known)
	}
	if f.codex.cache != nil {
		t.Fatal("a failure was cached")
	}
}

func TestAFailedProbeFallsBackToTheLastGoodReading(t *testing.T) {
	f := newCacheFixture(t)
	f.codex.CachedLimits(context.Background(), LimitsTTL)
	f.backdate(3600 * time.Second)
	f.codex.limitsFn = func(context.Context) map[string]Usage { return map[string]Usage{} }
	found, age, known := f.codex.CachedLimits(context.Background(), LimitsTTL)
	if !reflect.DeepEqual(found, map[string]Usage{"work": {Usable: true}}) {
		t.Fatalf("found = %v", found)
	}
	if !known || age <= 3599 {
		t.Fatalf("its age must be reported, not hidden: %v", age)
	}
}

func fl(v float64) *float64 { return &v }
func str(v string) *string  { return &v }

var limitsRows = []Row{{
	Name: "work", Email: "work@example.com", Plan: str("pro"),
	Usable: false, Blocked: true, ResetsAt: fl(1789417352),
	SoonestWindow: "codex/primary weekly",
	Windows: []Window{
		{Limit: "codex", Tier: "primary", Label: "weekly", UsedPercent: fl(100), ResetsAt: fl(1789417352)},
		{Limit: "codex", Tier: "primary", Label: "5-hour", UsedPercent: fl(12), ResetsAt: fl(1789400000)}},
}}

func TestTheWorstWindowIsWhatConstrainsTheAccount(t *testing.T) {
	// An account carries several windows at once; the highest one binds.
	out := shapeLimits(limitsRows)
	if out["work"].WorstUsed == nil || *out["work"].WorstUsed != 1.0 {
		t.Fatalf("worst_used = %v", out["work"].WorstUsed)
	}
	if len(out["work"].Windows) != 2 {
		t.Fatalf("windows = %v", out["work"].Windows)
	}
	if out["work"].Windows[0].Label != "codex primary weekly" || out["work"].Windows[1].Used != 0.12 {
		t.Fatalf("windows = %+v", out["work"].Windows)
	}
	if out["work"].Reset == nil || *out["work"].Reset != 1789417352 || *out["work"].ResetWindow != "codex/primary weekly" {
		t.Fatalf("reset = %v window = %v", out["work"].Reset, out["work"].ResetWindow)
	}
}

func TestBlockedStateIsCarried(t *testing.T) {
	out := shapeLimits(limitsRows)
	if out["work"].Usable || !out["work"].Blocked {
		t.Fatalf("usage = %+v", out["work"])
	}
}

func TestAFailedProbeYieldsNoQuotaRatherThanAnError(t *testing.T) {
	// Quota is an enrichment; losing it must not hide the accounts.
	c := New(t.TempDir())
	c.Which = func(string) (string, error) { return "", errors.New("not found") }
	c.Spawn = func(context.Context, []string, []string) (Process, error) {
		t.Fatal("app-server must not be spawned without codex on PATH")
		return nil, nil
	}
	if out := c.Limits(context.Background()); len(out) != 0 {
		t.Fatalf("limits = %v", out)
	}
}

func TestAnErrorRowIsCarriedAsUsageError(t *testing.T) {
	out := shapeLimits([]Row{{Name: "work", Email: "w@example.com", Error: str("codex app-server exited")}})
	usage := out["work"]
	if usage.Error == nil || *usage.Error != "codex app-server exited" || usage.Usable || usage.Blocked {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.WorstUsed != nil || usage.Reset != nil || usage.ResetWindow != nil || len(usage.Windows) != 0 {
		t.Fatalf("usage = %+v", usage)
	}
	raw, _ := json.Marshal(usage)
	if string(raw) != `{"usable":false,"blocked":false,"error":"codex app-server exited","worst_used":null,"reset":null,"reset_window":null,"windows":[]}` {
		t.Fatalf("json = %s", raw)
	}
}

func TestRowsWithoutANameAreIgnored(t *testing.T) {
	if out := shapeLimits([]Row{{Usable: true}}); len(out) != 0 {
		t.Fatalf("limits = %v", out)
	}
}

func TestMissingPercentageIsNotZeroUsage(t *testing.T) {
	rows := []Row{{Name: "work", Windows: []Window{
		{Limit: "codex", Tier: "primary", Label: "weekly", UsedPercent: nil},
		{Limit: "spark", Label: "weekly", UsedPercent: fl(25)}}}}
	result := shapeLimits(rows)
	if len(result["work"].Windows) != 1 {
		t.Fatalf("windows = %+v", result["work"].Windows)
	}
	if result["work"].Windows[0].Used != .25 {
		t.Fatalf("used = %v", result["work"].Windows[0].Used)
	}
	// The label keeps the Python f-string shape: a missing tier leaves two spaces.
	if result["work"].Windows[0].Label != "spark  weekly" {
		t.Fatalf("label = %q", result["work"].Windows[0].Label)
	}
}

func TestOverviewAttachesUsageAndReportsTheUnsavedLive(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.write(f.codex.LiveAuth(), "other@example.com", "acc-2", future)
	f.codex.limitsFn = func(context.Context) map[string]Usage {
		return map[string]Usage{"work": {Usable: true, Windows: []UsageWindow{}}, LiveName: {Blocked: true, Windows: []UsageWindow{}}}
	}
	overview, err := f.codex.Overview(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if overview.UnsavedLive == nil || *overview.UnsavedLive != "other@example.com" {
		t.Fatalf("unsaved_live = %v", overview.UnsavedLive)
	}
	if !overview.QuotaRead || overview.QuotaAgeS == nil || *overview.QuotaAgeS != 0 {
		t.Fatalf("overview = %+v", overview)
	}
	if overview.Accounts[0].Usage == nil || !overview.Accounts[0].Usage.Blocked {
		t.Fatalf("live usage = %+v", overview.Accounts[0].Usage)
	}
	if overview.Accounts[1].Usage == nil || !overview.Accounts[1].Usage.Usable {
		t.Fatalf("work usage = %+v", overview.Accounts[1].Usage)
	}
	raw, _ := json.Marshal(overview)
	var shape map[string]any
	_ = json.Unmarshal(raw, &shape)
	for _, key := range []string{"accounts", "unsaved_live", "quota_read", "quota_age_s"} {
		if _, ok := shape[key]; !ok {
			t.Errorf("overview lacks %q", key)
		}
	}
}

func TestOverviewWithoutLimitsHasNoQuota(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.codex.limitsFn = func(context.Context) map[string]Usage {
		t.Fatal("limits must not be probed")
		return nil
	}
	overview, err := f.codex.Overview(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if overview.QuotaRead || overview.QuotaAgeS != nil || overview.Accounts[0].Usage != nil || overview.UnsavedLive != nil {
		t.Fatalf("overview = %+v", overview)
	}
}

func TestOverviewUsesTheCacheWhenAMaxAgeIsGiven(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	calls := 0
	f.codex.limitsFn = func(context.Context) map[string]Usage {
		calls++
		return map[string]Usage{"work": {Usable: true}}
	}
	for range 3 {
		if _, err := f.codex.Overview(context.Background(), true, LimitsTTL); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestTheActiveProfileIsReportedFromTheLiveCredential(t *testing.T) {
	// Codex refreshes auth.json in place, so the saved copy of the account in use
	// lags behind. Reporting the stale copy says "expired" about a live login.
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", past)
	f.write(f.codex.LiveAuth(), "work@example.com", "acc-1", future)
	accounts := f.codex.Accounts()
	if len(accounts) != 1 || !accounts[0].IsActive {
		t.Fatalf("accounts = %+v", accounts)
	}
	if got := *accounts[0].TokenExpiresAt; got != future {
		t.Fatalf("TokenExpiresAt = %v, want the live %v", got, future)
	}
}

func TestAnInactiveProfileKeepsItsOwnExpiry(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("other"), "other@example.com", "acc-2", past)
	f.write(f.codex.LiveAuth(), "work@example.com", "acc-1", future)
	for _, account := range f.codex.Accounts() {
		if account.Alias != "other" {
			continue
		}
		if got := *account.TokenExpiresAt; got != past {
			t.Fatalf("TokenExpiresAt = %v, want its own %v", got, past)
		}
		return
	}
	t.Fatal("profile 'other' missing")
}

func TestTheActiveProfileTakesTheLiveQuotaReading(t *testing.T) {
	f := newAccountsFixture(t)
	f.write(f.profile("work"), "work@example.com", "acc-1", future)
	f.write(f.codex.LiveAuth(), "work@example.com", "acc-1", future)
	revoked, quarter := "refresh token was revoked", 0.25
	f.codex.limitsFn = func(context.Context) map[string]Usage {
		return map[string]Usage{
			"work":   {Error: &revoked},
			LiveName: {Usable: true, WorstUsed: &quarter},
		}
	}
	overview, err := f.codex.Overview(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	usage := overview.Accounts[0].Usage
	if usage == nil || usage.Error != nil || !usage.Usable {
		t.Fatalf("usage = %+v, want the live reading", usage)
	}
}
