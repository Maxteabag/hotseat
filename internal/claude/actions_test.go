package claude

// Launching, switching and detecting terminals: the action layer's guards.
//
// Picking the first terminal on PATH opens one the user may never use. That
// happened in practice: kitty was installed, ghostty was the real terminal, and
// the window appeared somewhere the user was not looking.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeBackend exposes one account and nothing else, like a backend that only
// knows a token. It deliberately does not implement ProfileStore.
type fakeBackend struct {
	account Account
}

func newFakeBackend(token string) *fakeBackend {
	return &fakeBackend{account: Account{Alias: "work", Email: "user@example.com",
		Org: "Example Org", OrgUUID: "org-1", Token: token}}
}

func (b *fakeBackend) Name() string                 { return "fake" }
func (b *fakeBackend) HasProfiles() bool            { return true }
func (b *fakeBackend) CanSwitch() bool              { return true }
func (b *fakeBackend) Accounts() ([]Account, error) { return []Account{b.account}, nil }
func (b *fakeBackend) Capabilities() Capabilities {
	return Capabilities{Backend: "fake", Profiles: true, Switch: true}
}

type spawnCall struct {
	argv []string
	env  []string
}

type recorder struct {
	calls []spawnCall
	err   error
}

func (r *recorder) spawn(argv []string, env []string) error {
	r.calls = append(r.calls, spawnCall{argv: argv, env: env})
	return r.err
}

func expectActionError(t *testing.T, err error) *ActionError {
	t.Helper()
	var actionErr *ActionError
	if !errors.As(err, &actionErr) {
		t.Fatalf("expected ActionError, got %v", err)
	}
	return actionErr
}

// --- terminal detection ------------------------------------------------------

type detectionFixture struct {
	installed map[string]bool
	running   string
	env       map[string]string
	procRoot  string
}

func newDetectionFixture(t *testing.T) *detectionFixture {
	t.Helper()
	// An empty procfs: no ancestors can be found, like the Python patching
	// _terminal_from_ancestors to None.
	return &detectionFixture{installed: map[string]bool{}, env: map[string]string{}, procRoot: t.TempDir()}
}

func (f *detectionFixture) detector() TerminalDetector {
	return TerminalDetector{
		LookPath: func(name string) (string, error) {
			if f.installed[name] {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		Getenv:   func(name string) string { return f.env[name] },
		ProcRoot: f.procRoot,
		PID:      100,
		Run:      func(*exec.Cmd) ([]byte, error) { return []byte(f.running), nil },
	}
}

func TestDetectTerminalExplicitPreferenceWins(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true, "ghostty": true}
	f.env["HOTSEAT_TERMINAL"] = "ghostty"
	if got := f.detector().Detect(); got != "ghostty" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTerminalTERMINALIsHonouredWhenInstalled(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true, "ghostty": true}
	f.env["TERMINAL"] = "kitty"
	if got := f.detector().Detect(); got != "kitty" {
		t.Fatalf("got %q", got)
	}
	f.env["TERMINAL"] = "not-installed"
	if got := f.detector().Detect(); got != "ghostty" {
		t.Fatalf("an uninstalled preference must be ignored, got %q", got)
	}
}

func TestDetectTerminalXDGLauncherIsPreferredOverGuessing(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true, XDGLauncher: true}
	if got := f.detector().Detect(); got != XDGLauncher {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTerminalRunningBeatsMerelyInstalled(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true, "ghostty": true}
	f.running = "bash\nghostty\nfirefox\n"
	if got := f.detector().Detect(); got != "ghostty" {
		t.Fatalf("what is running is what the user is looking at; got %q", got)
	}
}

func TestDetectTerminalFallsBackToPathOnlyWhenNothingElseIsKnown(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"konsole": true}
	if got := f.detector().Detect(); got != "konsole" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTerminalNoneAtAllIsReported(t *testing.T) {
	f := newDetectionFixture(t)
	if got := f.detector().Detect(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTerminalPsFailureIsNotFatal(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"xterm": true}
	d := f.detector()
	d.Run = func(*exec.Cmd) ([]byte, error) { return nil, errors.New("no ps") }
	if got := d.Detect(); got != "xterm" {
		t.Fatalf("got %q", got)
	}
}

// writeProc lays out /proc/<pid>/comm and /proc/<pid>/status for a fake process.
func writeProc(t *testing.T, root string, pid, ppid int, comm string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	writeFile(t, filepath.Join(dir, "comm"), comm+"\n")
	writeFile(t, filepath.Join(dir, "status"), "Name:\t"+comm+"\nState:\tS (sleeping)\nPPid:\t"+strconv.Itoa(ppid)+"\n")
}

func TestDetectTerminalWalksAncestors(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true, "ghostty": true}
	writeProc(t, f.procRoot, 100, 50, "hotseat")
	writeProc(t, f.procRoot, 50, 20, "bash")
	writeProc(t, f.procRoot, 20, 1, "ghostty")
	if got := f.detector().Detect(); got != "ghostty" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectTerminalAncestorMustBeInstalled(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"kitty": true}
	writeProc(t, f.procRoot, 100, 20, "bash")
	writeProc(t, f.procRoot, 20, 1, "ghostty")
	if got := f.detector().Detect(); got != "kitty" {
		t.Fatalf("an ancestor that is not on PATH cannot be launched; got %q", got)
	}
}

func TestDetectTerminalAncestorWalkStopsAtInitAndAtTwelveLevels(t *testing.T) {
	f := newDetectionFixture(t)
	f.installed = map[string]bool{"foot": true}
	// Thirteen shells deep, then the terminal: one level too far.
	pid := 100
	for i := 0; i < 13; i++ {
		writeProc(t, f.procRoot, pid, pid+1, "sh")
		pid++
	}
	writeProc(t, f.procRoot, pid, 1, "foot")
	if got := f.detector().fromAncestors(); got != "" {
		t.Fatalf("got %q", got)
	}
	// Directly above init: nothing found, and the walk stops rather than reading pid 1.
	f2 := newDetectionFixture(t)
	writeProc(t, f2.procRoot, 100, 1, "sh")
	writeProc(t, f2.procRoot, 1, 0, "ghostty")
	f2.installed = map[string]bool{"ghostty": true}
	if got := f2.detector().fromAncestors(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEachTerminalGetsItsOwnCommandConvention(t *testing.T) {
	cases := map[string]string{"ghostty": "-e", "kitty": "-e", "gnome-terminal": "--",
		"xfce4-terminal": "-x", XDGLauncher: "--"}
	for terminal, want := range cases {
		if got := TerminalFlag(terminal); got != want {
			t.Errorf("%s: got %q want %q", terminal, got, want)
		}
	}
}

func TestAnAbsolutePathStillResolvesItsConvention(t *testing.T) {
	if got := TerminalFlag("/usr/bin/gnome-terminal"); got != "--" {
		t.Fatalf("got %q", got)
	}
}

func TestTerminalsListAndOrder(t *testing.T) {
	want := []string{"ghostty", "kitty", "alacritty", "wezterm", "foot",
		"gnome-terminal", "konsole", "xfce4-terminal", "xterm"}
	if !equalStrings(Terminals, want) {
		t.Fatalf("Terminals = %v", Terminals)
	}
}

// --- launch --------------------------------------------------------------------

type launchFixture struct {
	t    *testing.T
	home string
	rec  *recorder
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	home := isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	unsetenv(t, "CLAUDE_CODE_OAUTH_TOKEN")
	return &launchFixture{t: t, home: home, rec: &recorder{}}
}

func (f *launchFixture) launch(backend Backend) (LaunchResult, error) {
	f.t.Helper()
	return Launch(backend, "work", f.rec.spawn, "kitty")
}

func (f *launchFixture) mustLaunch(backend Backend) LaunchResult {
	f.t.Helper()
	out, err := f.launch(backend)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestLaunchSessionIsPointedAtItsOwnConfigDirectory(t *testing.T) {
	f := newLaunchFixture(t)
	out := f.mustLaunch(newFakeBackend("ACCOUNT-TOKEN"))
	want := filepath.Join(f.home, ".claude-accounts", "work")
	if out.ConfigDir != want {
		t.Fatalf("config_dir = %q", out.ConfigDir)
	}
	got, _ := envValue(f.rec.calls[0].env, "CLAUDE_CONFIG_DIR")
	if got != want {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", got)
	}
	if !out.Started || out.Terminal != "kitty" || out.Alias != "work" {
		t.Fatalf("result = %+v", out)
	}
}

func TestLaunchEnvironmentTokenIsNotUsed(t *testing.T) {
	// It authenticates but hides the account, which is the bug being fixed.
	f := newLaunchFixture(t)
	f.mustLaunch(newFakeBackend("ACCOUNT-TOKEN"))
	if _, present := envValue(f.rec.calls[0].env, "CLAUDE_CODE_OAUTH_TOKEN"); present {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN must not be set")
	}
}

func TestLaunchTokenNeverReachesTheCommandLine(t *testing.T) {
	f := newLaunchFixture(t)
	f.mustLaunch(newFakeBackend("ACCOUNT-TOKEN"))
	if strings.Contains(strings.Join(f.rec.calls[0].argv, " "), "ACCOUNT-TOKEN") {
		t.Fatal("token on the command line")
	}
}

func TestLaunchParentSessionVariablesAreStripped(t *testing.T) {
	f := newLaunchFixture(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abc")
	t.Setenv("ANTHROPIC_API_KEY", "sk-nope")
	f.mustLaunch(newFakeBackend("ACCOUNT-TOKEN"))
	for _, name := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "ANTHROPIC_API_KEY"} {
		if _, present := envValue(f.rec.calls[0].env, name); present {
			t.Fatalf("%s must not reach the new session", name)
		}
	}
	if _, present := envValue(f.rec.calls[0].env, "HOME"); !present {
		t.Fatal("the rest of the environment must carry over")
	}
}

func TestLaunchAccountWithoutATokenIsRefused(t *testing.T) {
	f := newLaunchFixture(t)
	_, err := f.launch(newFakeBackend(""))
	if expectActionError(t, err).Msg != "work has no stored token" {
		t.Fatalf("message = %q", err)
	}
	if len(f.rec.calls) != 0 {
		t.Fatal("nothing should be spawned")
	}
}

func TestLaunchUnknownAccount(t *testing.T) {
	f := newLaunchFixture(t)
	_, err := Launch(newFakeBackend("x"), "nobody", f.rec.spawn, "kitty")
	if expectActionError(t, err).Msg != "No account named 'nobody'" {
		t.Fatalf("message = %q", err)
	}
}

func TestLaunchWritesAScriptThatPinsTheDirectory(t *testing.T) {
	f := newLaunchFixture(t)
	out := f.mustLaunch(newFakeBackend("ACCOUNT-TOKEN"))
	argv := f.rec.calls[0].argv
	if len(argv) != 3 || argv[0] != "kitty" || argv[1] != "-e" {
		t.Fatalf("argv = %v", argv)
	}
	script := argv[2]
	if filepath.Base(script) != "run.sh" || !strings.HasPrefix(filepath.Base(filepath.Dir(script)), "hotseat-launch-") {
		t.Fatalf("script = %q", script)
	}
	if filepath.Dir(filepath.Dir(script)) != os.TempDir() {
		t.Fatalf("launcher must live in the temp dir, got %q", script)
	}
	for _, path := range []string{script, filepath.Dir(script)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	body := readText(t, script)
	want := "#!/bin/sh\n" +
		"# Starts a Claude session pinned to one account. Generated by hotseat.\n" +
		"unset " + strings.Join(BlockedEnv, " ") + "\n" +
		`PATH="${PATH:-/usr/bin:/bin}:/usr/bin:/bin"` + "\n" +
		`CLAUDE_CONFIG_DIR="` + out.ConfigDir + `"` + "\n" +
		"export CLAUDE_CONFIG_DIR\n" +
		`exec claude "$@"` + "\n"
	if body != want {
		t.Fatalf("script:\n%s", body)
	}
	if strings.Contains(body, "ACCOUNT-TOKEN") {
		t.Fatal("the launcher holds no secret")
	}
}

func TestLaunchFailedSpawnRemovesTheLauncher(t *testing.T) {
	f := newLaunchFixture(t)
	f.rec.err = errors.New("exec format error")
	_, err := f.launch(newFakeBackend("ACCOUNT-TOKEN"))
	if expectActionError(t, err).Msg != "Could not start kitty: exec format error" {
		t.Fatalf("message = %q", err)
	}
	dir := filepath.Dir(f.rec.calls[0].argv[2])
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the launcher directory should be gone")
	}
}

func TestLaunchWithoutATerminalIsRefused(t *testing.T) {
	f := newLaunchFixture(t)
	t.Setenv("HOTSEAT_TERMINAL", "definitely-not-a-terminal")
	t.Setenv("TERMINAL", "definitely-not-a-terminal")
	t.Setenv("PATH", t.TempDir())
	_, err := Launch(newFakeBackend("ACCOUNT-TOKEN"), "work", f.rec.spawn, "")
	if !strings.Contains(expectActionError(t, err).Msg, "hotseat use <alias>") {
		t.Fatalf("message = %q", err)
	}
}

func TestLaunchUsesTheProfileStoreCredentialsWhenAvailable(t *testing.T) {
	f := newLaunchFixture(t)
	r := newRegressionFixture(t) // its own HOME; re-point the temp dir
	t.Setenv("TMPDIR", t.TempDir())
	out, err := Launch(r.backend, "work", f.rec.spawn, "kitty")
	if err != nil {
		t.Fatal(err)
	}
	got := normalise(t, readJSONFile(t, filepath.Join(out.ConfigDir, ".credentials.json")))
	if !reflect.DeepEqual(got, normalise(t, r.live)) {
		t.Fatalf("stored = %v", got)
	}
}

func TestSweepOldLaunchers(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	stale := filepath.Join(tmp, "hotseat-launch-stale")
	fresh := filepath.Join(tmp, "hotseat-launch-fresh")
	other := filepath.Join(tmp, "other-stale")
	file := filepath.Join(tmp, "hotseat-launch-file")
	for _, dir := range []string{stale, fresh, other} {
		writeFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n")
	}
	writeFile(t, file, "not a directory")
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stale, other, file} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepOldLaunchers(time.Hour)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stale launcher should be removed")
	}
	for _, path := range []string{fresh, other, file} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s should be left alone: %v", filepath.Base(path), err)
		}
	}
}

// --- prepare / exec ------------------------------------------------------------

func TestPrepareUsesCompleteLiveCredentialsForActiveProfile(t *testing.T) {
	f := newRegressionFixture(t)
	target, env, err := Prepare(f.backend, "work")
	if err != nil {
		t.Fatal(err)
	}
	got := normalise(t, readJSONFile(t, filepath.Join(target, ".credentials.json")))
	if !reflect.DeepEqual(got, normalise(t, f.live)) {
		t.Fatalf("stored = %v", got)
	}
	if configDir, _ := envValue(env, "CLAUDE_CONFIG_DIR"); configDir != target {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", configDir)
	}
}

func TestPrepareUnsavedLiveAccountKeepsRefreshToken(t *testing.T) {
	f := newRegressionFixture(t)
	if err := os.Remove(filepath.Join(f.profile, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	target, _, err := Prepare(f.backend, SignedInAlias)
	if err != nil {
		t.Fatal(err)
	}
	got := normalise(t, readJSONFile(t, filepath.Join(target, ".credentials.json")))
	if !reflect.DeepEqual(got, normalise(t, f.live)) {
		t.Fatalf("stored = %v", got)
	}
}

func TestPrepareGlobalSettingsCarryOverAndPinnedEditsSurvive(t *testing.T) {
	f := newRegressionFixture(t)
	target, _, err := Prepare(f.backend, "work")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(target, ".claude.json")
	data := readJSONFile(t, config)
	if !reflect.DeepEqual(normalise(t, data["mcpServers"]), normalise(t, map[string]any{"fixture": map[string]any{}})) {
		t.Fatalf("mcpServers = %v", data["mcpServers"])
	}
	getMap(data, "mcpServers")["local"] = map[string]any{}
	writeJSON(t, config, data)
	if _, _, err := Prepare(f.backend, "work"); err != nil {
		t.Fatal(err)
	}
	if _, ok := getMap(readJSONFile(t, config), "mcpServers")["local"]; !ok {
		t.Fatal("edits made in the pinned directory must survive a relaunch")
	}
}

func TestPrepareFallsBackToTheAccountFields(t *testing.T) {
	home := isolateHome(t)
	expires := int64(future)
	backend := newFakeBackend("ACCOUNT-TOKEN")
	backend.account.Plan = "team"
	backend.account.AccessExpiresAt = &expires
	target, _, err := Prepare(backend, "work")
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(home, ".claude-accounts", "work") {
		t.Fatalf("target = %q", target)
	}
	oauth := getMap(readJSONFile(t, filepath.Join(target, ".credentials.json")), "claudeAiOauth")
	if getString(oauth, "accessToken") != "ACCOUNT-TOKEN" || getString(oauth, "subscriptionType") != "team" {
		t.Fatalf("oauth = %v", oauth)
	}
	if ExpiresAt(map[string]any{"claudeAiOauth": oauth}) != expires {
		t.Fatalf("expiresAt = %v", oauth["expiresAt"])
	}
	if _, present := oauth["refreshTokenExpiresAt"]; !present {
		t.Fatal("an unknown refresh expiry is still emitted, as null")
	}
	identity := getMap(readJSONFile(t, filepath.Join(target, ".claude.json")), "oauthAccount")
	if getString(identity, "emailAddress") != "user@example.com" || getString(identity, "organizationUuid") != "org-1" {
		t.Fatalf("identity = %v", identity)
	}
}

func TestExecSessionReplacesTheProcessWithAPinnedSession(t *testing.T) {
	home := isolateHome(t)
	var gotPath string
	var gotArgv, gotEnv []string
	execute := func(path string, argv []string, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	if err := ExecSession(newFakeBackend("ACCOUNT-TOKEN"), "work", []string{"--model", "haiku"}, execute); err != nil {
		t.Fatal(err)
	}
	if gotPath != "claude" || !equalStrings(gotArgv, []string{"claude", "--model", "haiku"}) {
		t.Fatalf("exec %q %v", gotPath, gotArgv)
	}
	if configDir, _ := envValue(gotEnv, "CLAUDE_CONFIG_DIR"); configDir != filepath.Join(home, ".claude-accounts", "work") {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", configDir)
	}
}

func TestExecSessionFailuresAreActionErrors(t *testing.T) {
	isolateHome(t)
	err := ExecSession(newFakeBackend(""), "work", nil, func(string, []string, []string) error { return nil })
	if expectActionError(t, err).Msg != "work has no stored token" {
		t.Fatalf("message = %q", err)
	}
	err = ExecSession(newFakeBackend("x"), "work", nil, func(string, []string, []string) error {
		return errors.New("exec: not found")
	})
	if !strings.Contains(expectActionError(t, err).Msg, "could not start claude") {
		t.Fatalf("message = %q", err)
	}
}

// --- verify ----------------------------------------------------------------------

type fakeProber struct {
	headers map[string]string
	err     error
	token   string
}

func (p *fakeProber) Probe(_ context.Context, token string) (map[string]string, error) {
	p.token = token
	return p.headers, p.err
}

func (p *fakeProber) Summarise(headers map[string]string) any {
	return map[string]any{"status": headers["anthropic-ratelimit-unified-status"]}
}

func TestVerifySuccess(t *testing.T) {
	prober := &fakeProber{headers: map[string]string{"anthropic-ratelimit-unified-status": "allowed"}}
	out, err := Verify(context.Background(), newFakeBackend("ACCOUNT-TOKEN"), "work", prober)
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Alias != "work" || prober.token != "ACCOUNT-TOKEN" {
		t.Fatalf("out = %+v, token = %q", out, prober.token)
	}
	if !reflect.DeepEqual(out.Usage, map[string]any{"status": "allowed"}) {
		t.Fatalf("usage = %v", out.Usage)
	}
}

func TestVerifyFailures(t *testing.T) {
	_, err := Verify(context.Background(), newFakeBackend("ACCOUNT-TOKEN"), "work", &fakeProber{err: errors.New("the account rejected this token")})
	if expectActionError(t, err).Msg != "work did not answer: the account rejected this token" {
		t.Fatalf("message = %q", err)
	}
	_, err = Verify(context.Background(), newFakeBackend(""), "work", &fakeProber{})
	if expectActionError(t, err).Msg != "work has no stored token" {
		t.Fatalf("message = %q", err)
	}
	_, err = Verify(context.Background(), newFakeBackend("x"), "nope", &fakeProber{})
	if expectActionError(t, err).Msg != "No account named 'nope'" {
		t.Fatalf("message = %q", err)
	}
}

// --- switch ----------------------------------------------------------------------

type switchFixture struct {
	t       *testing.T
	home    string
	root    string
	backend *LinuxBackend
}

func newSwitchFixture(t *testing.T) *switchFixture {
	t.Helper()
	home := isolateHome(t)
	root := filepath.Join(home, ".claude")
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "work@example.com", "organizationUuid": "org-work"},
		"mcpServers":   map[string]any{"keep": map[string]any{}}})
	writeJSON(t, filepath.Join(root, ".credentials.json"), credentialsFixture("LIVE", future+5))
	work := filepath.Join(root, "profiles", "work")
	writeJSON(t, filepath.Join(work, "credentials.json"), credentialsFixture("WORK-OLD", future))
	writeJSON(t, filepath.Join(work, "meta.json"), map[string]any{"email": "work@example.com", "orgUuid": "org-work"})
	writeJSON(t, filepath.Join(work, "oauthAccount.json"), map[string]any{"emailAddress": "work@example.com", "organizationUuid": "org-work"})
	other := filepath.Join(root, "profiles", "other")
	writeJSON(t, filepath.Join(other, "credentials.json"), credentialsFixture("OTHER", future))
	writeJSON(t, filepath.Join(other, "meta.json"), map[string]any{"email": "other@example.com", "orgUuid": "org-other"})
	writeJSON(t, filepath.Join(other, "oauthAccount.json"), map[string]any{"emailAddress": "other@example.com", "organizationUuid": "org-other"})
	return &switchFixture{t: t, home: home, root: root, backend: NewLinuxBackend(root)}
}

func (f *switchFixture) token(path string) string {
	f.t.Helper()
	return getString(getMap(readJSONFile(f.t, path), "claudeAiOauth"), "accessToken")
}

func TestSwitchRefusesWithoutAcknowledgement(t *testing.T) {
	f := newSwitchFixture(t)
	_, err := Switch(f.backend, "other", 4, false)
	if expectActionError(t, err).Msg != "Switching retargets 4 running session(s). Confirm before proceeding." {
		t.Fatalf("message = %q", err)
	}
	if f.token(filepath.Join(f.root, ".credentials.json")) != "LIVE" {
		t.Fatal("a refused switch must change nothing")
	}
	// The gate comes before the platform check, so the message is the same everywhere.
	_, err = Switch(&DarwinBackend{}, "other", 0, false)
	if !strings.Contains(expectActionError(t, err).Msg, "Confirm before proceeding") {
		t.Fatalf("message = %q", err)
	}
}

func TestSwitchIsNotSupportedOnKeychainBackends(t *testing.T) {
	_, err := Switch(&DarwinBackend{}, "other", 0, true)
	if expectActionError(t, err).Msg != "Switching the default account is not supported on this platform" {
		t.Fatalf("message = %q", err)
	}
}

func TestSwitchUnknownProfile(t *testing.T) {
	f := newSwitchFixture(t)
	_, err := Switch(f.backend, "nobody", 0, true)
	if expectActionError(t, err).Msg != "No saved profile named 'nobody'" {
		t.Fatalf("message = %q", err)
	}
}

func TestSwitchRetargetsTheLiveFilesAndSnapshotsTheOutgoingAccount(t *testing.T) {
	f := newSwitchFixture(t)
	out, err := Switch(f.backend, "other", 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Switched || out.Alias != "other" || out.SessionsAffected != 2 {
		t.Fatalf("result = %+v", out)
	}
	live := filepath.Join(f.root, ".credentials.json")
	if f.token(live) != "OTHER" {
		t.Fatalf("live token = %q", f.token(live))
	}
	info, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("live mode = %o", info.Mode().Perm())
	}
	// The outgoing live token was saved into the profile holding that same account.
	if got := f.token(filepath.Join(f.root, "profiles", "work", "credentials.json")); got != "LIVE" {
		t.Fatalf("work profile token = %q", got)
	}
	if got := f.token(filepath.Join(f.root, "profiles", "other", "credentials.json")); got != "OTHER" {
		t.Fatalf("other profile token = %q", got)
	}
	config := readJSONFile(t, filepath.Join(f.home, ".claude.json"))
	if getString(getMap(config, "oauthAccount"), "emailAddress") != "other@example.com" {
		t.Fatalf("identity = %v", config["oauthAccount"])
	}
	if _, kept := getMap(config, "mcpServers")["keep"]; !kept {
		t.Fatal("the rest of ~/.claude.json must survive")
	}
	if readText(t, filepath.Join(f.root, "profiles", ".current_profile")) != "other" {
		t.Fatal("the active-profile marker was not updated")
	}
}

func TestSwitchNeverSnapshotsIntoAnUnrelatedProfile(t *testing.T) {
	f := newSwitchFixture(t)
	// The live account matches no saved profile.
	writeJSON(t, filepath.Join(f.home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "stranger@example.com", "organizationUuid": "org-x"}})
	if _, err := Switch(f.backend, "other", 0, true); err != nil {
		t.Fatal(err)
	}
	if got := f.token(filepath.Join(f.root, "profiles", "work", "credentials.json")); got != "WORK-OLD" {
		t.Fatalf("work profile token = %q; writing a live token into the wrong profile destroys it", got)
	}
}

func TestSwitchWithoutIdentityFileLeavesClaudeJSONAlone(t *testing.T) {
	f := newSwitchFixture(t)
	if err := os.Remove(filepath.Join(f.root, "profiles", "other", "oauthAccount.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Switch(f.backend, "other", 0, true); err != nil {
		t.Fatal(err)
	}
	config := readJSONFile(t, filepath.Join(f.home, ".claude.json"))
	if getString(getMap(config, "oauthAccount"), "emailAddress") != "work@example.com" {
		t.Fatalf("identity = %v", config["oauthAccount"])
	}
}

func TestSwitchReportsAnUnreadableIdentityConfig(t *testing.T) {
	f := newSwitchFixture(t)
	writeFile(t, filepath.Join(f.home, ".claude.json"), "{not json")
	_, err := Switch(f.backend, "other", 0, true)
	if !strings.HasPrefix(expectActionError(t, err).Msg, "Switched credentials but could not update identity: ") {
		t.Fatalf("message = %q", err)
	}
	// The credentials did switch before the identity step failed, as in the Python.
	if f.token(filepath.Join(f.root, ".credentials.json")) != "OTHER" {
		t.Fatal("credentials should have been switched")
	}
}

// --- launch_command / login ---------------------------------------------------------

func TestShellQuoteMatchesShlex(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"plain":       "plain",
		"a/b_c-d.e:f": "a/b_c-d.e:f",
		"has space":   "'has space'",
		"it's":        `'it'"'"'s'`,
		"$HOME":       "'$HOME'",
		"a;b":         "'a;b'",
		"ünïcode":     "'ünïcode'",
		"--flag=x":    "--flag=x",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q want %q", in, got, want)
		}
	}
}

func TestLaunchCommandBuildsABashCommandInTheTerminal(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-nope")
	rec := &recorder{}
	out, err := LaunchCommand([]string{"codex", "resume", "it's here"}, "/work/my repo", rec.spawn, "gnome-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Started || out.Terminal != "gnome-terminal" {
		t.Fatalf("result = %+v", out)
	}
	want := []string{"gnome-terminal", "--", "bash", "-lc", `cd '/work/my repo' && exec codex resume 'it'"'"'s here'`}
	if !equalStrings(rec.calls[0].argv, want) {
		t.Fatalf("argv = %q", rec.calls[0].argv)
	}
	env := rec.calls[0].env
	if configDir, _ := envValue(env, "CLAUDE_CONFIG_DIR"); configDir != filepath.Join(home, ".claude") {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", configDir)
	}
	if _, leaked := envValue(env, "ANTHROPIC_API_KEY"); leaked {
		t.Fatal("blocked variables must be stripped")
	}
}

func TestLaunchCommandWithoutCwdAndFailures(t *testing.T) {
	isolateHome(t)
	rec := &recorder{}
	if _, err := LaunchCommand([]string{"claude"}, "", rec.spawn, "kitty"); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(rec.calls[0].argv, []string{"kitty", "-e", "bash", "-lc", "exec claude"}) {
		t.Fatalf("argv = %q", rec.calls[0].argv)
	}
	rec.err = errors.New("boom")
	_, err := LaunchCommand([]string{"claude"}, "", rec.spawn, "kitty")
	if expectActionError(t, err).Msg != "Could not start kitty: boom" {
		t.Fatalf("message = %q", err)
	}
	t.Setenv("HOTSEAT_TERMINAL", "")
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir())
	_, err = LaunchCommand([]string{"claude"}, "", rec.spawn, "")
	if expectActionError(t, err).Msg != "No supported terminal found. Set HOTSEAT_TERMINAL to the one you use." {
		t.Fatalf("message = %q", err)
	}
}

func TestLoginHandsOffToTheCLI(t *testing.T) {
	rec := &recorder{}
	out, err := Login(rec.spawn, "xfce4-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Started || out.Terminal != "xfce4-terminal" {
		t.Fatalf("result = %+v", out)
	}
	if !equalStrings(rec.calls[0].argv, []string{"xfce4-terminal", "-x", "claude", "auth", "login"}) {
		t.Fatalf("argv = %q", rec.calls[0].argv)
	}
	if rec.calls[0].env != nil {
		t.Fatal("login inherits the environment unchanged")
	}
	rec.err = errors.New("boom")
	_, err = Login(rec.spawn, "xterm")
	if expectActionError(t, err).Msg != "Could not start xterm: boom" {
		t.Fatalf("message = %q", err)
	}
	t.Setenv("HOTSEAT_TERMINAL", "")
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir())
	_, err = Login(rec.spawn, "")
	if expectActionError(t, err).Msg != "No supported terminal found. Run `claude auth login` yourself." {
		t.Fatalf("message = %q", err)
	}
}

func TestDetachedSpawnRunsAndDetaches(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	if err := detachedSpawn([]string{"sh", "-c", "echo ok > \"$1\"", "sh", marker}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the spawned command never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSwitchRefusesWhenProfilesCannotBeListed(t *testing.T) {
	// Listing the profiles is how the outgoing live token finds its way back
	// into its own profile. If that listing fails, the switch must stop before
	// overwriting the live file, as the Python did.
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	f := newSwitchFixture(t)
	profiles := filepath.Join(f.root, "profiles")
	if err := os.Chmod(profiles, 0o100); err != nil { // traversable, not listable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(profiles, 0o700) })
	_, err := Switch(f.backend, "other", 1, true)
	var be *BackendError
	if !errors.As(err, &be) {
		t.Fatalf("expected a BackendError, got %v", err)
	}
	live := filepath.Join(f.root, ".credentials.json")
	if f.token(live) != "LIVE" {
		t.Fatalf("live token = %q; the live file was overwritten without a snapshot", f.token(live))
	}
	if got := f.token(filepath.Join(profiles, "work", "credentials.json")); got != "WORK-OLD" {
		t.Fatalf("work profile token = %q", got)
	}
}
