package claude

// Refreshing an expired profile through the CLI, without touching anything shared.

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	refreshNow  = 1_800_000_000.0
	refreshHour = 3600.0
)

func refreshCreds(access, refreshToken string, expires float64) map[string]any {
	return map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refreshToken,
		"expiresAt": int64(expires * 1000), "subscriptionType": "max",
		"refreshTokenExpiresAt": int64((refreshNow + 20*86400) * 1000)}}
}

func defaultCreds() map[string]any {
	return refreshCreds("old-access", "old-refresh", refreshNow-refreshHour)
}

type cliCall struct {
	args []string
	dir  string
	env  []string
}

type refreshFixture struct {
	t        *testing.T
	tmp      string
	root     string
	backend  *LinuxBackend
	profile  string
	sessions string
	shared   string
	calls    []cliCall
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	isolateHome(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "claude")
	profile := filepath.Join(root, "profiles", "work")
	writeJSON(t, filepath.Join(profile, "credentials.json"), defaultCreds())
	writeJSON(t, filepath.Join(profile, "oauthAccount.json"), map[string]any{"emailAddress": "dev@example.com"})
	shared := filepath.Join(root, ".credentials.json")
	writeJSON(t, shared, refreshCreds("shared-access", "shared-refresh", refreshNow+5*refreshHour))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	unsetenv(t, "HOTSEAT_NO_REFRESH")
	unsetenv(t, "HOTSEAT_REFRESH_MARGIN_S")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	return &refreshFixture{t: t, tmp: tmp, root: root, backend: NewLinuxBackend(root),
		profile: profile, sessions: filepath.Join(tmp, "sessions"), shared: shared}
}

func (f *refreshFixture) stored() map[string]any {
	f.t.Helper()
	return getMap(readJSONFile(f.t, filepath.Join(f.profile, "credentials.json")), "claudeAiOauth")
}

// fakeRun stands in for the CLI rotating tokens.
func (f *refreshFixture) fakeRun(rotate bool, newAccess string, now float64) RunCLI {
	return func(cmd *exec.Cmd) error {
		f.calls = append(f.calls, cliCall{args: cmd.Args, dir: cmd.Dir, env: cmd.Env})
		configDir, ok := envValue(cmd.Env, "CLAUDE_CONFIG_DIR")
		if !ok {
			f.t.Fatal("CLAUDE_CONFIG_DIR missing from the CLI environment")
		}
		path := filepath.Join(configDir, ".credentials.json")
		seeded, err := ReadJSON(path)
		if err != nil || seeded == nil {
			f.t.Fatalf("seeded credentials missing: %v", err)
		}
		if float64(ExpiresAt(seeded))/1000 >= now {
			f.t.Fatal("copy must be backdated")
		}
		if rotate {
			writeJSON(f.t, path, refreshCreds(newAccess, "new-refresh", now+8*refreshHour))
		}
		return nil
	}
}

func (f *refreshFixture) opts(run RunCLI) RefreshOptions {
	return RefreshOptions{Run: run, Now: refreshNow, SessionRoot: f.sessions}
}

func (f *refreshFixture) refresh(run RunCLI) RefreshResult {
	f.t.Helper()
	out, err := Refresh(f.backend, "work", f.opts(run))
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func expectRefreshError(t *testing.T, err error) *RefreshError {
	t.Helper()
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("expected RefreshError, got %v", err)
	}
	return refreshErr
}

func TestRefreshFreshTokenIsLeftAlone(t *testing.T) {
	f := newRefreshFixture(t)
	writeJSON(t, filepath.Join(f.profile, "credentials.json"), refreshCreds("old-access", "old-refresh", refreshNow+5*refreshHour))
	out := f.refresh(f.fakeRun(true, "new-access", refreshNow))
	if out.Refreshed || out.Source != "fresh" {
		t.Fatalf("got %+v", out)
	}
	if len(f.calls) != 0 {
		t.Fatal("the CLI must not run for a fresh token")
	}
}

func TestRefreshWithinMarginIsRefreshedPreemptively(t *testing.T) {
	f := newRefreshFixture(t)
	writeJSON(t, filepath.Join(f.profile, "credentials.json"), refreshCreds("old-access", "old-refresh", refreshNow+10*60))
	out := f.refresh(f.fakeRun(true, "new-access", refreshNow))
	if !out.Refreshed {
		t.Fatalf("got %+v", out)
	}
}

func TestRefreshExpiredTokenRotatesThroughCLI(t *testing.T) {
	f := newRefreshFixture(t)
	out := f.refresh(f.fakeRun(true, "new-access", refreshNow))
	if !out.Refreshed || out.Source != "cli" {
		t.Fatalf("got %+v", out)
	}
	if math.Abs(out.HoursLeft-8.0) > 0.05 {
		t.Fatalf("hours_left = %v", out.HoursLeft)
	}
	stored := f.stored()
	if getString(stored, "accessToken") != "new-access" || getString(stored, "refreshToken") != "new-refresh" {
		t.Fatalf("stored = %v", stored)
	}
	info, err := os.Stat(filepath.Join(f.profile, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	// A dated copy of the old file, and a rescue copy of the rotated tokens.
	entries, err := os.ReadDir(filepath.Join(f.profile, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, strings.SplitN(entry.Name(), ".", 2)[0])
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"credentials", "rotated"}) {
		t.Fatalf("backups = %v", names)
	}
	// The shared credential file is exactly as it was.
	if getString(getMap(readJSONFile(t, f.shared), "claudeAiOauth"), "accessToken") != "shared-access" {
		t.Fatal("the shared credentials were touched")
	}
}

func TestRefreshCLIRunsIsolatedWithCleanEnvironment(t *testing.T) {
	f := newRefreshFixture(t)
	f.refresh(f.fakeRun(true, "new-access", refreshNow))
	call := f.calls[0]
	if _, leaked := envValue(call.env, "ANTHROPIC_API_KEY"); leaked {
		t.Fatal("ANTHROPIC_API_KEY reached the CLI")
	}
	configDir, _ := envValue(call.env, "CLAUDE_CONFIG_DIR")
	if configDir == f.root {
		t.Fatal("the CLI must not run against the real config directory")
	}
	if call.dir != configDir {
		t.Fatalf("cwd %q != CLAUDE_CONFIG_DIR %q", call.dir, configDir)
	}
	if !equalStrings(call.args, RefreshCommand("claude")) {
		t.Fatalf("argv = %v", call.args)
	}
	for i, arg := range call.args {
		if arg == "--max-budget-usd" && call.args[i+1] != "0.02" {
			t.Fatalf("budget = %q", call.args[i+1])
		}
	}
	if _, err := os.Stat(call.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("throwaway config dir is removed")
	}
	if !strings.Contains(filepath.Base(call.dir), "hotseat-refresh-") {
		t.Fatalf("temp dir %q lacks the hotseat-refresh- prefix", call.dir)
	}
}

func TestRefreshSeedsIdentityIntoTheThrowawayDirectory(t *testing.T) {
	f := newRefreshFixture(t)
	var seen map[string]any
	run := func(cmd *exec.Cmd) error {
		seen = readJSONFile(t, filepath.Join(cmd.Dir, ".claude.json"))
		return nil
	}
	if _, err := Refresh(f.backend, "work", f.opts(run)); err != nil {
		t.Fatal(err)
	}
	if getString(getMap(seen, "oauthAccount"), "emailAddress") != "dev@example.com" {
		t.Fatalf(".claude.json = %v", seen)
	}
}

func TestRefreshCLIDecliningToRotateIsReportedNotFaked(t *testing.T) {
	f := newRefreshFixture(t)
	out := f.refresh(f.fakeRun(false, "new-access", refreshNow))
	if out.Refreshed || out.Source != "unchanged" {
		t.Fatalf("got %+v", out)
	}
	if getString(f.stored(), "accessToken") != "old-access" {
		t.Fatal("stored credentials changed")
	}
	if _, err := os.Stat(filepath.Join(f.profile, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no backup should be written when nothing changed")
	}
}

func TestRefreshCLIFailureLeavesProfileUnchanged(t *testing.T) {
	f := newRefreshFixture(t)
	broken := func(*exec.Cmd) error { return errors.New("claude: not found") }
	_, err := Refresh(f.backend, "work", f.opts(broken))
	refreshErr := expectRefreshError(t, err)
	if !strings.Contains(refreshErr.Msg, "could not run claude") {
		t.Fatalf("message = %q", refreshErr.Msg)
	}
	if getString(f.stored(), "accessToken") != "old-access" {
		t.Fatal("stored credentials changed")
	}
}

func TestRefreshNonZeroExitIsNotAFailureByItself(t *testing.T) {
	f := newRefreshFixture(t)
	run := func(cmd *exec.Cmd) error {
		// check=False: the CLI's exit status is irrelevant, only the file counts.
		return exec.Command("false").Run()
	}
	out := f.refresh(run)
	if out.Source != "unchanged" {
		t.Fatalf("got %+v", out)
	}
}

func TestRefreshPinnedSessionDirectorySuppliesNewerTokenWithoutCLI(t *testing.T) {
	f := newRefreshFixture(t)
	writeJSON(t, filepath.Join(f.sessions, "work", ".credentials.json"),
		refreshCreds("session-access", "session-refresh", refreshNow+6*refreshHour))
	out := f.refresh(f.fakeRun(true, "new-access", refreshNow))
	if !out.Refreshed || out.Source != "session" {
		t.Fatalf("got %+v", out)
	}
	if getString(f.stored(), "accessToken") != "session-access" {
		t.Fatal("the pinned token was not stored")
	}
	if len(f.calls) != 0 {
		t.Fatal("the CLI must not run when a session already rotated the token")
	}
}

func TestRefreshRotationIsPropagatedToStalePinnedDirectory(t *testing.T) {
	f := newRefreshFixture(t)
	pinned := filepath.Join(f.sessions, "work", ".credentials.json")
	writeJSON(t, pinned, refreshCreds("stale", "stale", refreshNow-2*refreshHour))
	f.refresh(f.fakeRun(true, "new-access", refreshNow))
	if getString(getMap(readJSONFile(t, pinned), "claudeAiOauth"), "accessToken") != "new-access" {
		t.Fatal("the pinned directory lags behind the profile")
	}
}

func TestRefreshUnknownProfileAndSignedInAlias(t *testing.T) {
	f := newRefreshFixture(t)
	_, err := Refresh(f.backend, "nobody", RefreshOptions{Run: f.fakeRun(true, "x", refreshNow), Now: refreshNow})
	if expectRefreshError(t, err).Msg != "no saved profile named 'nobody'" {
		t.Fatalf("message = %q", err)
	}
	_, err = Refresh(f.backend, SignedInAlias, RefreshOptions{Run: f.fakeRun(true, "x", refreshNow), Now: refreshNow})
	if !strings.Contains(expectRefreshError(t, err).Msg, "not a saved profile") {
		t.Fatalf("message = %q", err)
	}
}

func TestRefreshKeychainBackendIsRefused(t *testing.T) {
	f := newRefreshFixture(t)
	_, err := Refresh(&DarwinBackend{}, "work", RefreshOptions{Run: f.fakeRun(true, "x", refreshNow), Now: refreshNow})
	if !strings.Contains(expectRefreshError(t, err).Msg, "Keychain") {
		t.Fatalf("message = %q", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("nothing should run")
	}
}

func TestRefreshLocksTheProfile(t *testing.T) {
	f := newRefreshFixture(t)
	f.refresh(f.fakeRun(false, "", refreshNow))
	if _, err := os.Stat(filepath.Join(f.profile, ".lock")); err != nil {
		t.Fatalf("lock file: %v", err)
	}
}

func TestRefreshRealSubprocessPathWithFakeExecutable(t *testing.T) {
	f := newRefreshFixture(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	fake := filepath.Join(f.tmp, "bin", "claude") // not f.tmp/claude: that is the config root
	script := `#!/bin/sh
p="$CLAUDE_CONFIG_DIR/.credentials.json"
[ -f "$p" ] || exit 2
[ -z "$ANTHROPIC_API_KEY" ] || exit 3
exp=$(( ( $(date +%s) + 28800 ) * 1000 ))
printf '{"claudeAiOauth":{"accessToken":"exec-access","refreshToken":"exec-refresh","expiresAt":%s,"subscriptionType":"max"}}' "$exp" > "$p"
printf '{"result":"OK"}\n'
`
	writeFile(t, fake, script)
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	// Real clock here (the fake CLI stamps date +%s), so force past the freshness check.
	out, err := Refresh(f.backend, "work", RefreshOptions{Force: true, Claude: fake, SessionRoot: f.sessions})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refreshed || out.Source != "cli" {
		t.Fatalf("got %+v", out)
	}
	if getString(f.stored(), "accessToken") != "exec-access" {
		t.Fatalf("stored = %v", f.stored())
	}
}

func TestRefreshBackupsArePruned(t *testing.T) {
	f := newRefreshFixture(t)
	for i := 0; i < BackupKeep+3; i++ {
		if _, err := backup(f.profile, "credentials", nil, refreshNow+float64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := backup(f.profile, "rotated", defaultCreds(), refreshNow+float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, prefix := range []string{"credentials", "rotated"} {
		kept, err := filepath.Glob(filepath.Join(f.profile, "backups", prefix+".*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(kept) != BackupKeep {
			t.Fatalf("%s: kept %d, want %d", prefix, len(kept), BackupKeep)
		}
		sort.Strings(kept)
		if !strings.HasSuffix(kept[len(kept)-1], prefix+".1800000012.json") {
			t.Fatalf("%s: the newest copy was pruned: %v", prefix, kept)
		}
	}
}

func TestRefreshMarginFromEnvironment(t *testing.T) {
	unsetenv(t, "HOTSEAT_REFRESH_MARGIN_S")
	if RefreshMarginS() != 1800 {
		t.Fatalf("default margin = %v", RefreshMarginS())
	}
	t.Setenv("HOTSEAT_REFRESH_MARGIN_S", "600")
	if RefreshMarginS() != 600 {
		t.Fatalf("margin = %v", RefreshMarginS())
	}
	t.Setenv("HOTSEAT_REFRESH_MARGIN_S", "nonsense")
	if RefreshMarginS() != 1800 {
		t.Fatalf("margin = %v", RefreshMarginS())
	}
}

func TestRefreshResultJSONKeys(t *testing.T) {
	raw, err := json.Marshal(RefreshResult{Alias: "work", Source: "fresh", ExpiresAt: 1, HoursLeft: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alias", "refreshed", "source", "expires_at", "hours_left"} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("missing %q in %s", key, raw)
		}
	}
}

// --- automatic use ------------------------------------------------------------

func autoAccount(expires *int64) *Account {
	return &Account{Alias: "work", Token: "old-access", AccessExpiresAt: expires}
}

func msPtr(seconds float64) *int64 {
	v := int64(seconds * 1000)
	return &v
}

func TestAutoRefreshesAndUpdatesAccountInPlace(t *testing.T) {
	f := newRefreshFixture(t)
	account := autoAccount(msPtr(refreshNow - refreshHour))
	token, err := Auto(f.backend, account, f.opts(f.fakeRun(true, "new-access", refreshNow)))
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" || account.Token != "new-access" {
		t.Fatalf("token = %q, account = %q", token, account.Token)
	}
	if account.AccessExpiresAt == nil || float64(*account.AccessExpiresAt)/1000 <= refreshNow {
		t.Fatalf("access_expires_at = %v", account.AccessExpiresAt)
	}
	if account.RefreshExpiresAt == nil {
		t.Fatal("refresh_expires_at should be taken from the rotated credentials")
	}
}

func TestAutoFailureIsRememberedAndNotRetriedWithinCooldown(t *testing.T) {
	f := newRefreshFixture(t)
	broken := func(*exec.Cmd) error { return errors.New("boom") }
	_, err := Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), f.opts(broken))
	expectRefreshError(t, err)

	// Second attempt inside the cooldown does not call the CLI at all.
	opts := f.opts(f.fakeRun(true, "new-access", refreshNow))
	opts.Now = refreshNow + 60
	_, err = Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), opts)
	msg := expectRefreshError(t, err).Msg
	if !strings.Contains(msg, "not retried") || !strings.Contains(msg, "boom") {
		t.Fatalf("message = %q", msg)
	}
	if !strings.Contains(msg, "(not retried for 59 min)") {
		t.Fatalf("message = %q", msg)
	}
	if len(f.calls) != 0 {
		t.Fatal("the CLI ran inside the cooldown")
	}

	// After the cooldown it is tried again and succeeds, clearing the memo.
	later := refreshNow + RetryCooldownS + 1
	opts = f.opts(f.fakeRun(true, "new-access", later))
	opts.Now = later
	token, err := Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), opts)
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" {
		t.Fatalf("token = %q", token)
	}
	if memo := ReadMemo(); len(memo) != 0 {
		t.Fatalf("memo = %v", memo)
	}
	if !strings.HasPrefix(MemoPath(), filepath.Join(f.tmp, "cache", "hotseat")) {
		t.Fatalf("memo path = %q", MemoPath())
	}
}

func TestAutoMemoShape(t *testing.T) {
	f := newRefreshFixture(t)
	broken := func(*exec.Cmd) error { return errors.New("boom") }
	_, _ = Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), f.opts(broken))
	memo := ReadMemo()
	entry := getMap(memo, "work")
	if entry == nil {
		t.Fatalf("memo = %v", memo)
	}
	at, _ := asFloat64(entry["at"])
	if at != refreshNow || !strings.Contains(getString(entry, "error"), "boom") {
		t.Fatalf("entry = %v", entry)
	}
}

func TestAutoDisabledByEnvironment(t *testing.T) {
	f := newRefreshFixture(t)
	t.Setenv("HOTSEAT_NO_REFRESH", "1")
	_, err := Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), f.opts(f.fakeRun(true, "new-access", refreshNow)))
	if expectRefreshError(t, err).Msg != "automatic refresh disabled (HOTSEAT_NO_REFRESH)" {
		t.Fatalf("message = %q", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("the CLI ran while disabled")
	}
}

func TestAutoUnrotatedTokenIsAnError(t *testing.T) {
	f := newRefreshFixture(t)
	_, err := Auto(f.backend, autoAccount(msPtr(refreshNow-refreshHour)), f.opts(f.fakeRun(false, "", refreshNow)))
	if expectRefreshError(t, err).Msg != "the CLI did not rotate the token" {
		t.Fatalf("message = %q", err)
	}
}

func TestExpiredHelper(t *testing.T) {
	if !Expired(*autoAccount(msPtr(refreshNow - refreshHour)), refreshNow, 0) {
		t.Fatal("an hour-old token is expired")
	}
	if Expired(*autoAccount(msPtr(refreshNow + refreshHour)), refreshNow, 0) {
		t.Fatal("a token with an hour left is not expired")
	}
	if Expired(*autoAccount(nil), refreshNow, 0) {
		t.Fatal("an unknown expiry is not expired")
	}
	if !Expired(*autoAccount(msPtr(refreshNow + 10*60)), refreshNow, RefreshMarginS()) {
		t.Fatal("ten minutes left is inside the refresh margin")
	}
}

func TestPinnedCredentialsShapes(t *testing.T) {
	root := t.TempDir()
	if path, data := pinnedCredentials("work", root); path != "" || data != nil {
		t.Fatalf("absent: %q %v", path, data)
	}
	target := filepath.Join(root, "work", ".credentials.json")
	writeFile(t, target, "not json")
	if path, data := pinnedCredentials("work", root); path != target || data != nil {
		t.Fatalf("damaged: %q %v", path, data)
	}
	writeJSON(t, target, defaultCreds())
	path, data := pinnedCredentials("work", root)
	if path != target || !reflect.DeepEqual(normalise(t, data), normalise(t, defaultCreds())) {
		t.Fatalf("valid: %q %v", path, data)
	}
}

func TestRefreshDoesNotWaitForAChildTheCLILeavesBehind(t *testing.T) {
	// A background process that inherits the CLI's stdout would hold a pipe
	// open long after the CLI exited. The refresh must not read the output
	// through a pipe at all, so it returns as soon as the CLI does.
	f := newRefreshFixture(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	fake := filepath.Join(f.tmp, "bin", "claude")
	script := `#!/bin/sh
p="$CLAUDE_CONFIG_DIR/.credentials.json"
exp=$(( ( $(date +%s) + 28800 ) * 1000 ))
printf '{"claudeAiOauth":{"accessToken":"exec-access","refreshToken":"exec-refresh","expiresAt":%s,"subscriptionType":"max"}}' "$exp" > "$p"
( sleep 6 & )
printf '{"result":"OK"}\n'
`
	writeFile(t, fake, script)
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	out, err := Refresh(f.backend, "work", RefreshOptions{Force: true, Claude: fake, SessionRoot: f.sessions})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("refresh waited %s for the CLI's orphaned child", elapsed)
	}
	if !out.Refreshed || getString(f.stored(), "accessToken") != "exec-access" {
		t.Fatalf("got %+v stored %v", out, f.stored())
	}
}
