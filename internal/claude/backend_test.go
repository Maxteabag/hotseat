package claude

// Account discovery. No real credentials are involved anywhere in these tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	orgA = "00000000-0000-4000-8000-00000000000a"
	orgB = "00000000-0000-4000-8000-00000000000b"
)

func oauthFixture(token, plan string) map[string]any {
	return map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": token, "subscriptionType": plan,
		"expiresAt":             1800000000000,
		"refreshTokenExpiresAt": 1802000000000,
	}}
}

type linuxFixture struct {
	t       *testing.T
	root    string
	backend *LinuxBackend
}

func newLinuxFixture(t *testing.T) *linuxFixture {
	t.Helper()
	isolateHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &linuxFixture{t: t, root: root, backend: NewLinuxBackend(root)}
}

func (f *linuxFixture) profile(alias, email, orgUUID, token string) string {
	d := filepath.Join(f.root, "profiles", alias)
	writeJSON(f.t, filepath.Join(d, "credentials.json"), oauthFixture(token, "team"))
	writeJSON(f.t, filepath.Join(d, "meta.json"), map[string]any{
		"alias": alias, "email": email, "org": "Example Org", "orgUuid": orgUUID})
	return d
}

func (f *linuxFixture) live(token string) {
	writeJSON(f.t, filepath.Join(f.root, ".credentials.json"), oauthFixture(token, "team"))
}

// identity stands in for mock.patch.object(LinuxBackend, "_identity"): a custom
// root reads its identity from <root>/.claude.json.
func (f *linuxFixture) identity(identity map[string]any) {
	writeJSON(f.t, filepath.Join(f.root, ".claude.json"), map[string]any{"oauthAccount": identity})
}

func (f *linuxFixture) accounts() []Account {
	f.t.Helper()
	accounts, err := f.backend.Accounts()
	if err != nil {
		f.t.Fatal(err)
	}
	return accounts
}

func TestLinuxNoCredentialsYieldsNothing(t *testing.T) {
	f := newLinuxFixture(t)
	if got := f.accounts(); len(got) != 0 {
		t.Fatalf("expected no accounts, got %v", got)
	}
}

func TestLinuxProfilesAreListed(t *testing.T) {
	f := newLinuxFixture(t)
	f.profile("work", "user@example.com", orgA, "tok")
	f.profile("personal", "user@example.com", orgB, "tok")
	got := aliases(f.accounts())
	sort.Strings(got)
	if !equalStrings(got, []string{"personal", "work"}) {
		t.Fatalf("aliases = %v", got)
	}
}

func TestLinuxSameEmailDifferentOrgStaysTwoAccounts(t *testing.T) {
	// Collapsing these is how one organisation's token overwrites another.
	f := newLinuxFixture(t)
	f.profile("work", "user@example.com", orgA, "tok")
	f.profile("personal", "user@example.com", orgB, "tok")
	f.live("live-tok")
	f.identity(map[string]any{"emailAddress": "user@example.com", "organizationUuid": orgA})
	accounts := f.accounts()
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %v", accounts)
	}
	if got := activeAliases(accounts); !equalStrings(got, []string{"work"}) {
		t.Fatalf("only the matching organisation is active; got %v", got)
	}
}

func TestLinuxTrackerBreaksTiesBetweenDuplicateProfiles(t *testing.T) {
	f := newLinuxFixture(t)
	f.profile("work", "user@example.com", orgA, "tok")
	f.profile("work-copy", "user@example.com", orgA, "tok")
	writeFile(t, filepath.Join(f.root, "profiles", ".current_profile"), "work-copy")
	f.live("live-tok")
	f.identity(map[string]any{"emailAddress": "user@example.com", "organizationUuid": orgA})
	if got := activeAliases(f.accounts()); !equalStrings(got, []string{"work-copy"}) {
		t.Fatalf("active = %v", got)
	}
}

func TestLinuxSignedInAccountAppearsWithoutAnyProfiles(t *testing.T) {
	f := newLinuxFixture(t)
	f.live("live-tok")
	f.identity(map[string]any{"emailAddress": "user@example.com", "organizationName": "Example Org"})
	accounts := f.accounts()
	if len(accounts) != 1 || !accounts[0].IsActive {
		t.Fatalf("accounts = %v", accounts)
	}
	if accounts[0].Alias != SignedInAlias || accounts[0].Org != "Example Org" || accounts[0].Token != "live-tok" {
		t.Fatalf("account = %+v", accounts[0])
	}
}

func TestLinuxPublicViewNeverCarriesAToken(t *testing.T) {
	f := newLinuxFixture(t)
	f.profile("work", "user@example.com", orgA, "secret-value")
	var views []PublicAccount
	for _, a := range f.accounts() {
		views = append(views, a.Public())
	}
	published, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(published), "secret-value") || strings.Contains(string(published), "token") {
		t.Fatalf("published view leaks: %s", published)
	}
	if strings.Contains(f.accounts()[0].String(), "secret-value") {
		t.Fatal("String() must not include the token")
	}
}

func TestLinuxPublicViewShape(t *testing.T) {
	expires := int64(1800000000000)
	a := Account{Alias: "work", Email: "user@example.com", Plan: "team", IsActive: true,
		AccessExpiresAt: &expires, Token: "secret"}
	raw, err := json.Marshal(a.Public())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"alias": "work", "email": "user@example.com", "org": nil, "plan": "team",
		"plan_label": "Team", "rate_limit_tier": nil, "is_active": true,
		"access_expires_at": float64(1800000000000), "refresh_expires_at": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public = %v\nwant %v", got, want)
	}
}

func TestLinuxCapabilities(t *testing.T) {
	caps := NewLinuxBackend(t.TempDir()).Capabilities()
	if caps != (Capabilities{Backend: "file", Profiles: true, Switch: true}) {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestLinuxDamagedCredentialsAreABackendError(t *testing.T) {
	f := newLinuxFixture(t)
	writeFile(t, filepath.Join(f.root, ".credentials.json"), "{not json")
	_, err := f.backend.Accounts()
	var be *BackendError
	if !errors.As(err, &be) || !strings.HasPrefix(be.Error(), "could not read .credentials.json:") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadJSONMissingFileIsNil(t *testing.T) {
	data, err := ReadJSON(filepath.Join(t.TempDir(), "absent.json"))
	if data != nil || err != nil {
		t.Fatalf("got %v, %v", data, err)
	}
}

// --- subscription tiers (tests/test_subscription_tiers.py) ------------------

func TestMaxFiveAndTwentyAreDistinct(t *testing.T) {
	for _, tc := range []struct{ tier, label string }{
		{"default_claude_max_5x", "Max 5×"}, {"default_claude_max_20x", "Max 20×"},
	} {
		a := Account{Alias: "x", Plan: "max", RateLimitTier: tc.tier}
		if a.PlanLabel() != tc.label || a.Public().PlanLabel != tc.label {
			t.Errorf("%s: label %q", tc.tier, a.PlanLabel())
		}
		raw, _ := json.Marshal(a.Public())
		if strings.Contains(string(raw), "token") {
			t.Errorf("public view carries a token: %s", raw)
		}
	}
}

func TestTeamSeatKeepsItsPlanName(t *testing.T) {
	a := Account{Alias: "x", Plan: "team", RateLimitTier: "default_claude_max_5x"}
	if a.PlanLabel() != "Team 5×" {
		t.Fatalf("label = %q", a.PlanLabel())
	}
}

func TestUnknownTierDoesNotInventMultiplier(t *testing.T) {
	if got := (Account{Alias: "x", Plan: "max"}).PlanLabel(); got != "Max" {
		t.Fatalf("label = %q", got)
	}
	if got := (Account{Alias: "x"}).PlanLabel(); got != "Unknown plan" {
		t.Fatalf("label = %q", got)
	}
	if got := (Account{Alias: "x", Plan: "enterprise"}).PlanLabel(); got != "enterprise" {
		t.Fatalf("label = %q", got)
	}
}

func TestActiveMetadataOverridesOldProfileTier(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	creds := func(tier string) map[string]any {
		return map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "dummy", "subscriptionType": "max", "rateLimitTier": tier}}
	}
	writeJSON(t, filepath.Join(root, "profiles", "work", "credentials.json"), creds("default_claude_max_5x"))
	writeJSON(t, filepath.Join(root, "profiles", "work", "meta.json"),
		map[string]any{"email": "test@example.com", "orgUuid": "org"})
	writeJSON(t, filepath.Join(root, ".credentials.json"), creds("default_claude_max_20x"))
	writeJSON(t, filepath.Join(root, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "test@example.com", "organizationUuid": "org"}})
	accounts, err := NewLinuxBackend(root).Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].IsActive || accounts[0].PlanLabel() != "Max 20×" {
		t.Fatalf("accounts = %v", accounts)
	}
	if accounts[0].AccessExpiresAt != nil {
		t.Fatal("live credentials without expiresAt must not inherit the profile's")
	}
}

// --- darwin ------------------------------------------------------------------

func TestDarwinDefaultServiceNameMatchesTheCLI(t *testing.T) {
	isolateHome(t)
	if got := KeychainService(); got != "Claude Code-credentials" {
		t.Fatalf("service = %q", got)
	}
}

func TestDarwinCustomConfigDirScopesTheServiceName(t *testing.T) {
	isolateHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/elsewhere")
	service := KeychainService()
	if !strings.HasPrefix(service, "Claude Code-credentials-") {
		t.Fatalf("service = %q", service)
	}
	if digest := service[strings.LastIndex(service, "-")+1:]; len(digest) != 8 {
		t.Fatalf("digest = %q", digest)
	}
	// sha256("/tmp/elsewhere")[:8], as the CLI computes it.
	if !strings.HasSuffix(service, sha8("/tmp/elsewhere")) {
		t.Fatalf("service = %q", service)
	}
}

func TestDarwinSecureStorageDirOverridesConfigDir(t *testing.T) {
	isolateHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/elsewhere")
	// Present but empty: unscoped, even though CLAUDE_CONFIG_DIR is set.
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	if got := KeychainService(); got != "Claude Code-credentials" {
		t.Fatalf("empty secure-storage dir should be unscoped, got %q", got)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/tmp/secure")
	if got := KeychainService(); got != "Claude Code-credentials-"+sha8("/tmp/secure") {
		t.Fatalf("service = %q", got)
	}
}

func TestDarwinUnusualUsernamesFallBack(t *testing.T) {
	t.Setenv("USER", "bad name/with slash")
	if got := KeychainAccount(); got != "claude-code-user" {
		t.Fatalf("account = %q", got)
	}
	t.Setenv("USER", "peter.adams-1")
	if got := KeychainAccount(); got != "peter.adams-1" {
		t.Fatalf("account = %q", got)
	}
}

func TestDarwinBackendDeclaresItselfReadOnly(t *testing.T) {
	caps := (&DarwinBackend{}).Capabilities()
	if caps.Profiles {
		t.Fatal("no profiles on macOS")
	}
	if caps.Switch {
		t.Fatal("writing Keychain items is out of scope")
	}
	if caps.Backend != "keychain" {
		t.Fatalf("backend = %q", caps.Backend)
	}
}

type fakeExit struct{ code int }

func (e fakeExit) Error() string { return "exit status" }
func (e fakeExit) ExitCode() int { return e.code }

func TestDarwinReadsTheKeychainItem(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("USER", "tester")
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{"oauthAccount": map[string]any{
		"emailAddress": "user@example.com", "organizationName": "Example Org", "organizationUuid": orgA}})
	var argv []string
	b := &DarwinBackend{Run: func(cmd *exec.Cmd) ([]byte, error) {
		argv = cmd.Args
		raw, _ := json.Marshal(oauthFixture("kc-token", "max"))
		return append(raw, '\n'), nil
	}}
	accounts, err := b.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"security", "find-generic-password", "-a", "tester", "-w", "-s", "Claude Code-credentials"}
	if !equalStrings(argv, want) {
		t.Fatalf("argv = %v", argv)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	a := accounts[0]
	if a.Alias != SignedInAlias || !a.IsActive || a.Token != "kc-token" || a.Plan != "max" ||
		a.Email != "user@example.com" || a.Org != "Example Org" || a.OrgUUID != orgA {
		t.Fatalf("account = %+v", a)
	}
}

func TestDarwinFallsBackToTheCredentialsFile(t *testing.T) {
	home := isolateHome(t)
	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), oauthFixture("file-token", "pro"))
	b := &DarwinBackend{Run: func(*exec.Cmd) ([]byte, error) { return nil, fakeExit{44} }}
	accounts, err := b.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Token != "file-token" || accounts[0].Email != "" {
		t.Fatalf("accounts = %v", accounts)
	}
}

func TestDarwinNothingSignedInIsABackendError(t *testing.T) {
	isolateHome(t)
	b := &DarwinBackend{Run: func(*exec.Cmd) ([]byte, error) { return nil, fakeExit{44} }}
	_, err := b.Accounts()
	var be *BackendError
	if !errors.As(err, &be) || !strings.HasPrefix(err.Error(), "No Claude credentials found") {
		t.Fatalf("err = %v", err)
	}
}

func TestDarwinInvalidKeychainJSONIsABackendError(t *testing.T) {
	isolateHome(t)
	b := &DarwinBackend{Run: func(*exec.Cmd) ([]byte, error) { return []byte("not json"), nil }}
	_, err := b.Accounts()
	if err == nil || err.Error() != "the Keychain entry was not valid JSON" {
		t.Fatalf("err = %v", err)
	}
}

func TestDarwinSecurityFailureIsABackendError(t *testing.T) {
	isolateHome(t)
	b := &DarwinBackend{Run: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("boom") }}
	_, err := b.Accounts()
	var be *BackendError
	if !errors.As(err, &be) || err.Error() != "could not query the Keychain: boom" {
		t.Fatalf("err = %v", err)
	}
}

// --- selection -------------------------------------------------------------

func TestPlatformSelection(t *testing.T) {
	isolateHome(t)
	if _, ok := ForPlatform("darwin").(*DarwinBackend); !ok {
		t.Fatal("darwin should select the Keychain backend")
	}
	if _, ok := ForPlatform("linux").(*LinuxBackend); !ok {
		t.Fatal("linux should select the file backend")
	}
	if _, ok := ForPlatform("linux").(ProfileStore); !ok {
		t.Fatal("the file backend exposes its profile store")
	}
	if ForPlatform("") == nil {
		t.Fatal("the running platform must select something")
	}
}

func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
