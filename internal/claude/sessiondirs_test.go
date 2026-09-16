package claude

// Launching a pinned session must actually look like a different account.
//
// The original implementation passed the token in CLAUDE_CODE_OAUTH_TOKEN. That
// authenticates correctly, but the CLI then reports no account identity at all:
// `claude auth status` returns email None and org None. Every launched session
// looked anonymous and identical, which is indistinguishable from "it ignored
// my choice".
//
// Identity is only reported when a session has its own config directory
// holding a credentials file. That directory therefore has to be per account
// and persistent, because a session refreshes its own token and rotation
// supersedes the old one. A throwaway directory would strand the rotated token
// and break the account.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const future = int64(2_000_000_000_000)

func credentialsFixture(token string, expires int64) map[string]any {
	return map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": token, "refreshToken": "r-" + token,
		"expiresAt": expires, "refreshTokenExpiresAt": expires,
		"subscriptionType": "team"}}
}

type sessionFixture struct {
	t            *testing.T
	home, shared string
	root         string
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	home := isolateHome(t)
	shared := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(shared, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(shared, "settings.json"), `{"model":"x"}`)
	writeFile(t, filepath.Join(shared, "CLAUDE.md"), "shared instructions")
	// The default shared root keeps its .claude.json beside itself, in HOME.
	writeJSON(t, IdentityFile(shared), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "default@example.com"},
		"mcpServers":   map[string]any{"keep": map[string]any{}}})

	profile := filepath.Join(shared, "profiles", "work")
	writeJSON(t, filepath.Join(profile, "credentials.json"), credentialsFixture("ACCOUNT-TOKEN", future))
	writeJSON(t, filepath.Join(profile, "oauthAccount.json"), map[string]any{
		"emailAddress": "user@example.com", "organizationName": "Example Org"})
	return &sessionFixture{t: t, home: home, shared: shared, root: filepath.Join(home, ".claude-accounts")}
}

func (f *sessionFixture) ensure(token string, expires int64) string {
	f.t.Helper()
	dir, err := Ensure("work", credentialsFixture(token, expires),
		map[string]any{"emailAddress": "user@example.com", "organizationName": "Example Org"},
		f.shared, f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *sessionFixture) storedToken(dir string) string {
	f.t.Helper()
	stored := readJSONFile(f.t, filepath.Join(dir, ".credentials.json"))
	return getString(getMap(stored, "claudeAiOauth"), "accessToken")
}

// --- the defect --------------------------------------------------------------

func TestSessionDirectoryCarriesTheChosenAccountIdentity(t *testing.T) {
	f := newSessionFixture(t)
	d := f.ensure("ACCOUNT-TOKEN", future)
	identity := getMap(readJSONFile(t, filepath.Join(d, ".claude.json")), "oauthAccount")
	if getString(identity, "emailAddress") != "user@example.com" {
		t.Fatal("the session would otherwise report the default account")
	}
}

func TestSessionDirectoryHasItsOwnCredentials(t *testing.T) {
	// Identity is only reported when credentials come from a file.
	f := newSessionFixture(t)
	d := f.ensure("ACCOUNT-TOKEN", future)
	if got := f.storedToken(d); got != "ACCOUNT-TOKEN" {
		t.Fatalf("token = %q", got)
	}
}

func TestCredentialsArePrivate(t *testing.T) {
	f := newSessionFixture(t)
	d := f.ensure("ACCOUNT-TOKEN", future)
	for _, name := range []string{".credentials.json", ".claude.json"} {
		info, err := os.Stat(filepath.Join(d, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o044 != 0 {
			t.Fatalf("%s must not be readable by other users: %v", name, info.Mode())
		}
	}
	for _, dir := range []string{d, f.root} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want 0700", dir, info.Mode().Perm())
		}
	}
}

func TestDirectoryIsStableAcrossLaunches(t *testing.T) {
	// A throwaway directory would strand the refresh token rotation produces.
	f := newSessionFixture(t)
	if a, b := f.ensure("ACCOUNT-TOKEN", future), f.ensure("ACCOUNT-TOKEN", future); a != b {
		t.Fatalf("%s != %s", a, b)
	}
}

func TestARotatedTokenIsNeverOverwrittenByAnOlderOne(t *testing.T) {
	f := newSessionFixture(t)
	d := f.ensure("NEW", future+10_000)
	// The profile store still holds the pre-rotation copy.
	f.ensure("OLD", future)
	if got := f.storedToken(d); got != "NEW" {
		t.Fatal("overwriting a rotated token locks the account out")
	}
	// An equal expiry is allowed to overwrite: the caller may hold the same
	// credentials with a fresher refresh token.
	f.ensure("SAME", future+10_000)
	if got := f.storedToken(d); got != "SAME" {
		t.Fatalf("token = %q", got)
	}
}

func TestSharedAssetsAreLinkedNotCopied(t *testing.T) {
	f := newSessionFixture(t)
	d := f.ensure("ACCOUNT-TOKEN", future)
	for _, name := range []string{"skills", "settings.json", "CLAUDE.md"} {
		info, err := os.Lstat(filepath.Join(d, name))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s must stay in sync with the shared config (%v)", name, err)
		}
	}
	// Entries the shared config does not have are not linked.
	if _, err := os.Lstat(filepath.Join(d, "hooks")); err == nil {
		t.Fatal("hooks does not exist in the shared config and must not be linked")
	}
}

func TestExistingProjectConfigurationIsCarriedOver(t *testing.T) {
	f := newSessionFixture(t)
	d := f.ensure("ACCOUNT-TOKEN", future)
	config := readJSONFile(t, filepath.Join(d, ".claude.json"))
	if _, ok := getMap(config, "mcpServers")["keep"]; !ok {
		t.Fatal("a pinned session should start with the same servers configured")
	}
}

func TestSharedCredentialsAreNeverTouched(t *testing.T) {
	f := newSessionFixture(t)
	live := filepath.Join(f.shared, ".credentials.json")
	writeJSON(t, live, credentialsFixture("DEFAULT-ACCOUNT", future))
	f.ensure("ACCOUNT-TOKEN", future)
	if got := getString(getMap(readJSONFile(t, live), "claudeAiOauth"), "accessToken"); got != "DEFAULT-ACCOUNT" {
		t.Fatal("launching must not disturb other sessions")
	}
}

func TestEnsureDefaultsToTheAccountsRoot(t *testing.T) {
	f := newSessionFixture(t)
	d, err := Ensure("work", credentialsFixture("ACCOUNT-TOKEN", future), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d != filepath.Join(f.home, ".claude-accounts", "work") {
		t.Fatalf("dir = %s", d)
	}
	// No identity given: the shared identity is carried over untouched.
	identity := getMap(readJSONFile(t, filepath.Join(d, ".claude.json")), "oauthAccount")
	if getString(identity, "emailAddress") != "default@example.com" {
		t.Fatalf("identity = %v", identity)
	}
}

// --- pinned identity guard (tests/test_pinned_identity_guard.py) -------------

func TestWrongIdentityIsRejectedBeforeAnyOverwrite(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	target := filepath.Join(root, "work")
	oldConfig := `{"oauthAccount": {"emailAddress": "wrong@example.com", "organizationUuid": "wrong"}}`
	oldCreds := `{"claudeAiOauth": {"accessToken": "dummy", "expiresAt": 9999999999999}}`
	writeFile(t, filepath.Join(target, ".claude.json"), oldConfig)
	writeFile(t, filepath.Join(target, ".credentials.json"), oldCreds)
	_, err := Ensure("work",
		map[string]any{"claudeAiOauth": map[string]any{"accessToken": "new", "expiresAt": 2}},
		map[string]any{"emailAddress": "right@example.com", "organizationUuid": "right"},
		filepath.Join(root, "shared"), root)
	if !errors.Is(err, ErrPinnedIdentity) {
		t.Fatalf("err = %v", err)
	}
	if readText(t, filepath.Join(target, ".claude.json")) != oldConfig {
		t.Fatal(".claude.json was modified")
	}
	if readText(t, filepath.Join(target, ".credentials.json")) != oldCreds {
		t.Fatal(".credentials.json was modified")
	}
}

// --- regressions (tests/test_regressions.py, CredentialRegressionTest) -------

type regressionFixture struct {
	t        *testing.T
	home     string
	root     string
	profile  string
	identity map[string]any
	live     map[string]any
	backend  *LinuxBackend
}

func newRegressionFixture(t *testing.T) *regressionFixture {
	t.Helper()
	home := isolateHome(t)
	root := filepath.Join(home, ".claude")
	profile := filepath.Join(root, "profiles", "work")
	identity := map[string]any{"emailAddress": "fixture@example.invalid", "organizationUuid": "org"}
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": identity, "mcpServers": map[string]any{"fixture": map[string]any{}}})
	live := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "NEW", "refreshToken": "NEW-R", "expiresAt": 2000}}
	writeJSON(t, filepath.Join(root, ".credentials.json"), live)
	writeJSON(t, filepath.Join(profile, "credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "OLD", "refreshToken": "OLD-R", "expiresAt": 1000}})
	writeJSON(t, filepath.Join(profile, "meta.json"), map[string]any{
		"email": identity["emailAddress"], "orgUuid": "org"})
	writeJSON(t, filepath.Join(profile, "oauthAccount.json"), identity)
	return &regressionFixture{t: t, home: home, root: root, profile: profile,
		identity: identity, live: live, backend: NewLinuxBackend(root)}
}

// prepare is the sessiondirs half of actions.prepare: raw credentials from the
// backend, then Ensure with the defaults.
func (f *regressionFixture) prepare(alias string) string {
	f.t.Helper()
	credentials, identity, err := f.backend.RawCredentials(alias)
	if err != nil {
		f.t.Fatal(err)
	}
	if credentials == nil {
		f.t.Fatalf("no credentials for %q", alias)
	}
	target, err := Ensure(alias, credentials, identity, "", "")
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

func normalise(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLaunchUsesCompleteLiveCredentialsForActiveProfile(t *testing.T) {
	f := newRegressionFixture(t)
	target := f.prepare("work")
	got := normalise(t, readJSONFile(t, filepath.Join(target, ".credentials.json")))
	if !reflect.DeepEqual(got, normalise(t, f.live)) {
		t.Fatalf("stored = %v", got)
	}
	accounts, err := f.backend.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Token != "NEW" {
		t.Fatalf("token = %q", accounts[0].Token)
	}
}

func TestUnsavedLiveAccountKeepsRefreshToken(t *testing.T) {
	f := newRegressionFixture(t)
	if err := os.Remove(filepath.Join(f.profile, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	target := f.prepare(SignedInAlias)
	got := normalise(t, readJSONFile(t, filepath.Join(target, ".credentials.json")))
	if !reflect.DeepEqual(got, normalise(t, f.live)) {
		t.Fatalf("stored = %v", got)
	}
}

func TestGlobalSettingsCarryOverAndExistingPinnedEditsSurvive(t *testing.T) {
	f := newRegressionFixture(t)
	target := f.prepare("work")
	config := filepath.Join(target, ".claude.json")
	data := readJSONFile(t, config)
	if !reflect.DeepEqual(normalise(t, data["mcpServers"]), normalise(t, map[string]any{"fixture": map[string]any{}})) {
		t.Fatalf("mcpServers = %v", data["mcpServers"])
	}
	getMap(data, "mcpServers")["local"] = map[string]any{}
	writeJSON(t, config, data)
	f.prepare("work")
	if _, ok := getMap(readJSONFile(t, config), "mcpServers")["local"]; !ok {
		t.Fatal("edits made in the pinned directory must survive a relaunch")
	}
}

func TestCustomConfigIdentityIsReadFromCustomDirectory(t *testing.T) {
	f := newRegressionFixture(t)
	custom := filepath.Join(f.home, "custom")
	writeJSON(t, filepath.Join(custom, ".claude.json"), map[string]any{"oauthAccount": f.identity})
	identity, err := NewLinuxBackend(custom).Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalise(t, identity), normalise(t, f.identity)) {
		t.Fatalf("identity = %v", identity)
	}
}

func TestRawCredentialsForUnknownAliasIsNil(t *testing.T) {
	f := newRegressionFixture(t)
	creds, identity, err := f.backend.RawCredentials("nobody")
	if creds != nil || identity != nil || err != nil {
		t.Fatalf("got %v %v %v", creds, identity, err)
	}
}

// --- helpers -----------------------------------------------------------------

func TestWritePrivateReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := WritePrivate(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if readText(t, path) != "two" {
		t.Fatal("content not replaced")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestExpiresAtTolerance(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want int64
	}{
		{nil, 0},
		{map[string]any{}, 0},
		{map[string]any{"claudeAiOauth": "junk"}, 0},
		{map[string]any{"claudeAiOauth": map[string]any{"expiresAt": nil}}, 0},
		{map[string]any{"claudeAiOauth": map[string]any{"expiresAt": json.Number("1800000000000")}}, 1800000000000},
		{map[string]any{"claudeAiOauth": map[string]any{"expiresAt": 42}}, 42},
		{map[string]any{"claudeAiOauth": map[string]any{"expiresAt": "17"}}, 17},
		{map[string]any{"claudeAiOauth": map[string]any{"expiresAt": "nope"}}, 0},
	}
	for _, tc := range cases {
		if got := ExpiresAt(tc.in); got != tc.want {
			t.Errorf("ExpiresAt(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStripBlockedEnv(t *testing.T) {
	in := []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=abc", "ANTHROPIC_API_KEY=sk-nope", "PATH=/usr/bin", "HOME=/h"}
	got := StripBlockedEnv(in)
	if !equalStrings(got, []string{"PATH=/usr/bin", "HOME=/h"}) {
		t.Fatalf("got %v", got)
	}
	if len(in) != 5 {
		t.Fatal("input mutated")
	}
	if !IsBlockedEnv("CLAUDE_EFFORT") || IsBlockedEnv("CLAUDE_CONFIG_DIR") {
		t.Fatal("blocked set is wrong")
	}
}

func TestACustomSharedRootKeepsItsConfigInside(t *testing.T) {
	isolateHome(t)
	shared := filepath.Join(t.TempDir(), "custom")
	writeJSON(t, filepath.Join(shared, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{"custom": map[string]any{}}})
	d, err := Ensure("work", credentialsFixture("T", future), nil, shared, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := getMap(readJSONFile(t, filepath.Join(d, ".claude.json")), "mcpServers")["custom"]; !ok {
		t.Fatal("a custom CLAUDE_CONFIG_DIR keeps .claude.json inside itself")
	}
}
