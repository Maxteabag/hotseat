package claude

// The things the dashboard is allowed to do, and the guards around them.
//
// Safety model:
//
//	verify  - one API request. Touches nothing.
//	launch  - starts a session pinned to one account by environment variable. It
//	          writes no credential file, so other sessions are unaffected.
//	switch  - changes the machine-wide default. This is the dangerous one: every
//	          running session reads the same credential file, so switching
//	          retargets all of them. Guarded, and never silent.
//	login   - hands off to the official interactive sign-in.
//
// Tokens never appear in a response body, a log line, or a command line.
// `launch` passes the token through the child process environment only.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Terminals this knows how to launch a command in. Order is only the
// last-resort tiebreak: picking whatever is first on PATH opens a terminal the
// user may not even use, which is worse than useless when they are watching
// for a window.
var Terminals = []string{"ghostty", "kitty", "alacritty", "wezterm", "foot",
	"gnome-terminal", "konsole", "xfce4-terminal", "xterm"}

// XDGLauncher is the XDG default-terminal launcher. Honouring it is better than
// guessing: it opens whichever terminal the user actually set as their default,
// and it takes the command directly rather than behind -e.
const XDGLauncher = "xdg-terminal-exec"

// TerminalExecFlag is how each terminal takes "run this command". Most use -e.
var TerminalExecFlag = map[string]string{"gnome-terminal": "--", "xfce4-terminal": "-x",
	XDGLauncher: "--"}

// TerminalFlag is the "run this command" flag for a terminal, by base name.
func TerminalFlag(terminal string) string {
	if flag, ok := TerminalExecFlag[filepath.Base(terminal)]; ok {
		return flag
	}
	return "-e"
}

// Warn receives diagnostics the Python printed to stdout. The Go binary runs a
// TUI on stdout, so they go to stderr by default; replaceable for tests.
var Warn = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

const psTerminalTimeout = 15 * time.Second

// TerminalDetector finds the terminal to open windows in. The zero value reads
// the real environment, PATH, /proc and process list; tests fill in the fields.
type TerminalDetector struct {
	// LookPath resolves an executable; nil means exec.LookPath.
	LookPath func(name string) (string, error)
	// Getenv reads a variable; nil means os.Getenv.
	Getenv func(name string) string
	// ProcRoot is the procfs mount to walk ancestors in; "" means /proc.
	ProcRoot string
	// PID is the process whose ancestors are walked; 0 means this one.
	PID int
	// Run lists processes with `ps`; nil means (*exec.Cmd).Output.
	Run RunOutput
}

func (d TerminalDetector) which(name string) bool {
	look := d.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look(name)
	return err == nil
}

func (d TerminalDetector) getenv(name string) string {
	if d.Getenv != nil {
		return d.Getenv(name)
	}
	return os.Getenv(name)
}

func (d TerminalDetector) procRoot() string {
	if d.ProcRoot != "" {
		return d.ProcRoot
	}
	return "/proc"
}

func (d TerminalDetector) processName(pid int) string {
	raw, err := os.ReadFile(filepath.Join(d.procRoot(), strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (d TerminalDetector) parentOf(pid int) int {
	raw, err := os.ReadFile(filepath.Join(d.procRoot(), strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return parent
	}
	return 0
}

func isTerminal(name string) bool {
	for _, candidate := range Terminals {
		if candidate == name {
			return true
		}
	}
	return false
}

// fromAncestors is the terminal this process is actually running inside, if any.
func (d TerminalDetector) fromAncestors() string {
	pid := d.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	for range 12 { // shells nest; a terminal is never far up
		pid = d.parentOf(pid)
		if pid <= 1 {
			return ""
		}
		name := d.processName(pid)
		if isTerminal(name) && d.which(name) {
			return name
		}
	}
	return ""
}

// running is a terminal emulator already running for this user.
//
// This is the one that catches the case that matters: a detached server has no
// terminal ancestor, and the first entry on PATH may be an emulator that is
// installed but never used. What is already running is what the user is
// looking at.
func (d TerminalDetector) running() string {
	run := d.Run
	if run == nil {
		run = func(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTerminalTimeout)
	defer cancel()
	out, err := run(exec.CommandContext(ctx, "ps", "-u", strconv.Itoa(os.Getuid()), "-o", "comm="))
	if err != nil || ctx.Err() != nil {
		var exit *exec.ExitError
		// check=False: a non-zero exit still yields whatever was printed.
		if !errors.As(err, &exit) || ctx.Err() != nil {
			return ""
		}
	}
	live := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		live[strings.TrimSpace(line)] = true
	}
	for _, candidate := range Terminals {
		if live[candidate] && d.which(candidate) {
			return candidate
		}
	}
	return ""
}

// Detect picks the terminal to open a window in, most specific evidence first,
// or "" when none is known.
//
// Order matters. A terminal that is merely installed is the worst guess: it
// can open a window in an emulator the user never uses, which looks like
// nothing happened at all.
func (d TerminalDetector) Detect() string {
	preferred := d.getenv("HOTSEAT_TERMINAL")
	if preferred == "" {
		preferred = d.getenv("TERMINAL")
	}
	if preferred != "" && d.which(preferred) {
		return preferred
	}
	if d.which(XDGLauncher) {
		return XDGLauncher
	}
	if name := d.fromAncestors(); name != "" {
		return name
	}
	if name := d.running(); name != "" {
		return name
	}
	for _, candidate := range Terminals {
		if d.which(candidate) {
			return candidate
		}
	}
	return ""
}

// DetectTerminal is TerminalDetector{}.Detect(): the real machine.
func DetectTerminal() string { return TerminalDetector{}.Detect() }

// ActionError means an action was refused or could not be completed.
type ActionError struct {
	Msg string
	Err error
}

func (e *ActionError) Error() string { return e.Msg }

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *ActionError) Unwrap() error { return e.Err }

func actionErrorf(err error, format string, args ...any) error {
	return &ActionError{Msg: fmt.Sprintf(format, args...), Err: err}
}

func findAccount(backend Backend, alias string) (Account, error) {
	accounts, err := backend.Accounts()
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		if account.Alias == alias {
			return account, nil
		}
	}
	return Account{}, &ActionError{Msg: fmt.Sprintf("No account named %s", pyRepr(alias))}
}

// FindAccount is the account behind an alias, or an ActionError naming it.
func FindAccount(backend Backend, alias string) (Account, error) {
	return findAccount(backend, alias)
}

// Prober is the quota probe Verify spends its one request on; the rate-limit
// probe in internal/quota satisfies it through a small adapter. Summarise
// turns the response headers into the usage shape shown to people.
type Prober interface {
	Probe(ctx context.Context, token string) (map[string]string, error)
	Summarise(headers map[string]string) any
}

// VerifyResult is the answer to Verify; field names match the Python dict.
type VerifyResult struct {
	Alias string `json:"alias"`
	OK    bool   `json:"ok"`
	Usage any    `json:"usage"`
}

// Verify proves an account works. No local check can establish this.
func Verify(ctx context.Context, backend Backend, alias string, prober Prober) (VerifyResult, error) {
	account, err := findAccount(backend, alias)
	if err != nil {
		return VerifyResult{}, err
	}
	if account.Token == "" {
		return VerifyResult{}, &ActionError{Msg: fmt.Sprintf("%s has no stored token", alias)}
	}
	headers, err := prober.Probe(ctx, account.Token)
	if err != nil {
		return VerifyResult{}, actionErrorf(err, "%s did not answer: %v", alias, err)
	}
	return VerifyResult{Alias: alias, OK: true, Usage: prober.Summarise(headers)}, nil
}

// CleanEnv is this process's environment without the BlockedEnv variables and
// with CLAUDE_CONFIG_DIR pointing at configDir. An API key outranks account
// credentials and would bill the wrong one; the CLAUDE_CODE_* set makes a
// fresh session believe it is a nested child of whatever started this dashboard.
func CleanEnv(configDir string) []string {
	return withEnv(StripBlockedEnv(os.Environ()), "CLAUDE_CONFIG_DIR", configDir)
}

// credentialsFor fetches the full stored credentials and identity for an account.
//
// Falls back to what the account object carries, so a backend that exposes
// only a token still produces a usable session.
func credentialsFor(backend Backend, account Account) (map[string]any, map[string]any, error) {
	if store, ok := backend.(ProfileStore); ok {
		credentials, identity, err := store.RawCredentials(account.Alias)
		if err != nil {
			return nil, nil, err
		}
		if len(credentials) > 0 {
			return credentials, identity, nil
		}
	}
	credentials := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken":           account.Token,
		"expiresAt":             account.AccessExpiresAt,
		"refreshTokenExpiresAt": account.RefreshExpiresAt,
		"subscriptionType":      nullable(account.Plan),
	}}
	identity := map[string]any{"emailAddress": nullable(account.Email),
		"organizationName": nullable(account.Org), "organizationUuid": nullable(account.OrgUUID)}
	return credentials, identity, nil
}

// LauncherScript is the run.sh a launched window executes: it pins one session
// to one config directory. It holds no secret, only a path.
func LauncherScript(configDir string) string {
	return "#!/bin/sh\n" +
		"# Starts a Claude session pinned to one account. Generated by hotseat.\n" +
		"unset " + strings.Join(BlockedEnv, " ") + "\n" +
		`PATH="${PATH:-/usr/bin:/bin}:/usr/bin:/bin"` + "\n" +
		`CLAUDE_CONFIG_DIR="` + configDir + `"` + "\n" +
		"export CLAUDE_CONFIG_DIR\n" +
		`exec claude "$@"` + "\n"
}

// writeLauncher writes a script that points one session at one config directory.
//
// A terminal in single-instance mode hands the command to an existing daemon,
// and what that daemon does with a caller's environment varies. Setting the
// variable inside the script removes the question.
func writeLauncher(configDir string) (string, error) {
	work, err := os.MkdirTemp("", "hotseat-launch-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(work, 0o700); err != nil {
		_ = os.RemoveAll(work)
		return "", err
	}
	script := filepath.Join(work, "run.sh")
	if err := os.WriteFile(script, []byte(LauncherScript(configDir)), 0o700); err != nil {
		_ = os.RemoveAll(work)
		return "", err
	}
	if err := os.Chmod(script, 0o700); err != nil {
		_ = os.RemoveAll(work)
		return "", err
	}
	return script, nil
}

// LauncherSweepAge is how old a leftover launcher directory must be before the
// next launch removes it.
const LauncherSweepAge = time.Hour

// sweepOldLaunchers removes leftovers from launches that never ran. It never
// fails the launch. Only directories owned by the current uid are touched
// (design decision 9).
func sweepOldLaunchers(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	stale, err := filepath.Glob(filepath.Join(os.TempDir(), "hotseat-launch-*"))
	if err != nil {
		return
	}
	for _, path := range stale {
		info, err := os.Stat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				Warn(fmt.Sprintf("hotseat: could not clear %s: %v", filepath.Base(path), err))
			}
			continue
		}
		if !info.IsDir() || !info.ModTime().Before(cutoff) || !ownedByCurrentUser(info) {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

// Prepare sets up an account's session directory and returns it with a clean
// environment.
//
// This is the primitive both front ends share. The command line execs a
// session in place with it; the dashboard hands it to a new terminal window.
//
// The session gets its own config directory holding that account's
// credentials. That is what makes the account visible from inside the session,
// and it lets the session refresh its own token. Other running sessions read
// the shared credential file and are unaffected.
func Prepare(backend Backend, alias string) (configDir string, env []string, err error) {
	account, err := findAccount(backend, alias)
	if err != nil {
		return "", nil, err
	}
	if account.Token == "" {
		return "", nil, &ActionError{Msg: fmt.Sprintf("%s has no stored token", alias)}
	}
	credentials, identity, err := credentialsFor(backend, account)
	if err != nil {
		return "", nil, err
	}
	configDir, err = Ensure(account.Alias, credentials, identity, "", "")
	if err != nil {
		return "", nil, err
	}
	return configDir, CleanEnv(configDir), nil
}

// Execute replaces the current process with path, given argv (argv[0]
// included) and an environment. Tests inject one that records instead.
type Execute func(path string, argv []string, env []string) error

// ExecSession replaces this process with a session pinned to the account.
//
// The command line's launch path. Nothing is spawned and no terminal is
// opened: the shell that ran hotseat becomes the pinned session. It only
// returns on failure.
func ExecSession(backend Backend, alias string, argv []string, execute Execute) error {
	_, env, err := Prepare(backend, alias)
	if err != nil {
		return err
	}
	if execute == nil {
		execute = func(path string, argv []string, env []string) error {
			resolved, err := exec.LookPath(path)
			if err != nil {
				return err
			}
			return execReplace(resolved, argv, env)
		}
	}
	full := append([]string{"claude"}, argv...)
	if err := execute("claude", full, env); err != nil {
		return actionErrorf(err, "Could not start claude: %v", err)
	}
	return nil
}

// Spawn starts a detached process: its own session, stdout and stderr on
// /dev/null. A nil env inherits this process's environment. Tests inject one
// that records the argv instead.
type Spawn func(argv []string, env []string) error

func orDefaultSpawn(spawn Spawn) Spawn {
	if spawn != nil {
		return spawn
	}
	return detachedSpawn
}

// LaunchResult reports a launched window; field names match the Python dict.
type LaunchResult struct {
	Alias     string `json:"alias"`
	Terminal  string `json:"terminal"`
	Started   bool   `json:"started"`
	ConfigDir string `json:"config_dir"`
}

// Launch opens a NEW terminal window running a session pinned to this account.
//
// Used by the dashboard, which has no terminal of its own to hand over. A nil
// spawn uses the real detached spawner; an empty terminal is detected.
func Launch(backend Backend, alias string, spawn Spawn, terminal string) (LaunchResult, error) {
	account, err := findAccount(backend, alias)
	if err != nil {
		return LaunchResult{}, err
	}
	if account.Token == "" {
		return LaunchResult{}, &ActionError{Msg: fmt.Sprintf("%s has no stored token", alias)}
	}

	spawn = orDefaultSpawn(spawn)
	if terminal == "" {
		terminal = DetectTerminal()
	}
	if terminal == "" {
		return LaunchResult{}, &ActionError{Msg: "No supported terminal found. Set HOTSEAT_TERMINAL to the " +
			"one you use, or start a pinned session from the command " +
			"line with `hotseat use <alias>`."}
	}

	configDir, env, err := Prepare(backend, alias)
	if err != nil {
		return LaunchResult{}, err
	}

	sweepOldLaunchers(LauncherSweepAge)
	script, err := writeLauncher(configDir)
	if err != nil {
		return LaunchResult{}, err
	}
	argv := []string{terminal, TerminalFlag(terminal), script}
	if err := spawn(argv, env); err != nil {
		_ = os.RemoveAll(filepath.Dir(script))
		return LaunchResult{}, actionErrorf(err, "Could not start %s: %v", terminal, err)
	}
	return LaunchResult{Alias: alias, Terminal: terminal, Started: true, ConfigDir: configDir}, nil
}

// SwitchResult reports a completed switch; field names match the Python dict.
type SwitchResult struct {
	Alias            string `json:"alias"`
	Switched         bool   `json:"switched"`
	SessionsAffected int    `json:"sessions_affected"`
}

// liveSnapshotter is what Switch needs from a file backend to snapshot the
// outgoing account: the live account and the raw profile list, unmerged.
type liveSnapshotter interface {
	active() (*Account, error)
	profiles() ([]Account, error)
}

// Switch changes the machine-wide default account.
//
// Refuses unless the caller has acknowledged how many sessions this disturbs,
// so the dangerous path can never be taken by a stray click.
func Switch(backend Backend, alias string, sessions int, acknowledged bool) (SwitchResult, error) {
	// The safety gate comes first, so an unacknowledged switch is refused the
	// same way everywhere rather than falling through to a platform message.
	if !acknowledged {
		return SwitchResult{}, &ActionError{Msg: fmt.Sprintf(
			"Switching retargets %d running session(s). Confirm before proceeding.", sessions)}
	}
	store, ok := backend.(ProfileStore)
	if !ok || !backend.CanSwitch() {
		return SwitchResult{}, &ActionError{Msg: "Switching the default account is not supported on this platform"}
	}

	target := filepath.Join(store.ProfilesDir(), alias)
	source := filepath.Join(target, "credentials.json")
	if _, err := os.Stat(source); err != nil {
		return SwitchResult{}, &ActionError{Msg: fmt.Sprintf("No saved profile named %s", pyRepr(alias))}
	}

	live := filepath.Join(store.Root(), ".credentials.json")
	identityFile := filepath.Join(target, "oauthAccount.json")

	// Snapshot the outgoing account first, but only into a profile that already
	// holds that same account. Writing it anywhere else destroys a live token.
	if snap, ok := backend.(liveSnapshotter); ok {
		current, err := snap.active()
		if err != nil {
			var backendErr *BackendError
			if !errors.As(err, &backendErr) {
				return SwitchResult{}, err
			}
			current = nil
		}
		if _, statErr := os.Stat(live); current != nil && statErr == nil {
			candidates, err := snap.profiles()
			if err != nil {
				return SwitchResult{}, err
			}
			for _, candidate := range candidates {
				sameAccount := candidate.Email != "" && candidate.Email == current.Email &&
					candidate.OrgUUID == current.OrgUUID
				if sameAccount {
					payload, err := os.ReadFile(live)
					if err != nil {
						return SwitchResult{}, err
					}
					destination := filepath.Join(store.ProfilesDir(), candidate.Alias, "credentials.json")
					if err := WritePrivate(destination, payload); err != nil {
						return SwitchResult{}, err
					}
					break
				}
			}
		}
	}

	payload, err := os.ReadFile(source)
	if err != nil {
		return SwitchResult{}, err
	}
	if err := WritePrivate(live, payload); err != nil {
		return SwitchResult{}, err
	}

	if _, err := os.Stat(identityFile); err == nil {
		config := filepath.Join(HomeDir(), ".claude.json")
		if err := replaceIdentity(config, identityFile); err != nil {
			return SwitchResult{}, actionErrorf(err, "Switched credentials but could not update identity: %v", err)
		}
	}

	marker := filepath.Join(store.ProfilesDir(), ".current_profile")
	if err := os.WriteFile(marker, []byte(alias), 0o644); err != nil {
		return SwitchResult{}, actionErrorf(err, "Switched, but the active-profile marker is now wrong: %v", err)
	}

	return SwitchResult{Alias: alias, Switched: true, SessionsAffected: sessions}, nil
}

// replaceIdentity rewrites config's oauthAccount with the contents of identityFile.
func replaceIdentity(config, identityFile string) error {
	raw, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	data, err := DecodeObject(raw)
	if err != nil {
		return err
	}
	identityRaw, err := os.ReadFile(identityFile)
	if err != nil {
		return err
	}
	identity, err := decodeAny(identityRaw)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	data["oauthAccount"] = identity
	return WritePrivateJSON(config, data)
}

// ShellQuote is Python's shlex.quote: a shell-escaped version of s, safe to
// splice into a `bash -c` string.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("@%+=:,./_-", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// LaunchCommandResult reports a command window; field names match the Python dict.
type LaunchCommandResult struct {
	Terminal string `json:"terminal"`
	Started  bool   `json:"started"`
}

// LaunchCommand runs a command in a new terminal window, in the terminal the
// user uses, from cwd when it is not empty.
func LaunchCommand(command []string, cwd string, spawn Spawn, terminal string) (LaunchCommandResult, error) {
	spawn = orDefaultSpawn(spawn)
	if terminal == "" {
		terminal = DetectTerminal()
	}
	if terminal == "" {
		return LaunchCommandResult{}, &ActionError{Msg: "No supported terminal found. Set HOTSEAT_TERMINAL to the " +
			"one you use."}
	}
	quoted := make([]string, len(command))
	for i, part := range command {
		quoted[i] = ShellQuote(part)
	}
	inner := strings.Join(quoted, " ")
	where := ""
	if cwd != "" {
		where = "cd " + ShellQuote(cwd) + " && "
	}
	argv := []string{terminal, TerminalFlag(terminal), "bash", "-lc", where + "exec " + inner}
	if err := spawn(argv, CleanEnv(filepath.Join(HomeDir(), ".claude"))); err != nil {
		return LaunchCommandResult{}, actionErrorf(err, "Could not start %s: %v", terminal, err)
	}
	return LaunchCommandResult{Terminal: terminal, Started: true}, nil
}

// LoginResult reports a sign-in window; field names match the Python dict.
type LoginResult struct {
	Started  bool   `json:"started"`
	Terminal string `json:"terminal"`
}

// Login hands off to the official interactive sign-in, in a terminal. The
// window inherits this process's environment unchanged, as the Python did.
func Login(spawn Spawn, terminal string) (LoginResult, error) {
	spawn = orDefaultSpawn(spawn)
	if terminal == "" {
		terminal = DetectTerminal()
	}
	if terminal == "" {
		return LoginResult{}, &ActionError{Msg: "No supported terminal found. Run `claude auth login` yourself."}
	}
	argv := []string{terminal, TerminalFlag(terminal), "claude", "auth", "login"}
	if err := spawn(argv, nil); err != nil {
		return LoginResult{}, actionErrorf(err, "Could not start %s: %v", terminal, err)
	}
	return LoginResult{Started: true, Terminal: terminal}, nil
}
