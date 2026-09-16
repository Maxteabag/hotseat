package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// This file ports codexlaunch: launch Codex against a saved profile without
// changing the global default.

// SharedEntries are symlinked from the shared ~/.codex into a profile home so
// a pinned session keeps the user's configuration.
var SharedEntries = []string{"config.toml", "AGENTS.md", "skills", "plugins", "rules"}

// unsetVariables must not leak from the launcher into the pinned session: they
// would override the profile's identity.
var unsetVariables = []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_ACCESS_TOKEN", "CODEX_THREAD_ID", "CODEX_SESSION_ID"}

var launchAlias = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LaunchError mirrors actions.ActionError for the Codex launch path: the
// launch was refused or could not be completed.
type LaunchError struct{ Msg string }

func (e *LaunchError) Error() string { return e.Msg }

// Launcher is the terminal side of a launch, ported separately with
// actions.py. Detect picks the terminal to open a window in ("" when none is
// found), Flag is that terminal's run-a-command flag, and Spawn starts argv
// detached with the given environment.
type Launcher interface {
	Detect() (terminal string, err error)
	Flag(terminal string) string
	Spawn(argv []string, env []string) error
}

// LaunchResult is what Launch reports back to the TUI.
type LaunchResult struct {
	Started   bool   `json:"started"`
	Alias     string `json:"alias"`
	Terminal  string `json:"terminal"`
	ConfigDir string `json:"config_dir"`
}

// Prepare makes a saved profile usable as its own CODEX_HOME and returns the
// home and the environment to launch with.
func (c *Codex) Prepare(alias string) (home string, env []string, err error) {
	if !launchAlias.MatchString(alias) {
		return "", nil, &LaunchError{"Invalid saved profile name"}
	}
	found := false
	for _, account := range c.Accounts() {
		if account.Alias == alias && account.Saved {
			found = true
			break
		}
	}
	if !found {
		return "", nil, &LaunchError{"Saved Codex account not found"}
	}
	home = filepath.Join(c.ProfilesDir(), alias)
	if isSymlink(home) || isSymlink(filepath.Join(home, "auth.json")) {
		return "", nil, &LaunchError{"Refusing a symlinked profile directory"}
	}
	// The profile itself is the persistent CODEX_HOME: refreshed credentials stay
	// in the canonical auth.json instead of being stranded in a temporary copy.
	if err := os.Chmod(home, 0o700); err != nil {
		return "", nil, err
	}
	for _, name := range SharedEntries {
		source := filepath.Join(c.Home, name)
		target := filepath.Join(home, name)
		if !fileExists(source) || fileExists(target) || isSymlink(target) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return "", nil, err
		}
		if resolved, err = filepath.Abs(resolved); err != nil {
			return "", nil, err
		}
		if err := os.Symlink(resolved, target); err != nil {
			return "", nil, err
		}
	}
	env = append(environWithout(append([]string{"CODEX_HOME"}, unsetVariables...)...), "CODEX_HOME="+home)
	return home, env, nil
}

// Launch opens a terminal running Codex against the saved profile. terminal
// overrides detection when non-empty.
func (c *Codex) Launch(alias string, launcher Launcher, terminal string) (*LaunchResult, error) {
	if terminal == "" {
		detected, err := launcher.Detect()
		if err != nil {
			return nil, err
		}
		terminal = detected
	}
	if terminal == "" {
		return nil, &LaunchError{"No supported terminal found"}
	}
	executable, err := c.which("codex")
	if err != nil {
		return nil, &LaunchError{"Codex CLI is not installed"}
	}
	home, env, err := c.Prepare(alias)
	if err != nil {
		return nil, err
	}
	// Set identity inside the terminal too: terminal-server processes may inherit
	// a different environment from the launcher.
	inner := []string{"env"}
	for _, key := range unsetVariables {
		inner = append(inner, "-u", key)
	}
	inner = append(inner, "CODEX_HOME="+home, executable, "-c", `cli_auth_credentials_store="file"`)
	argv := append([]string{terminal, launcher.Flag(terminal)}, inner...)
	if err := launcher.Spawn(argv, env); err != nil {
		return nil, &LaunchError{fmt.Sprintf("Could not start %s: %v", terminal, err)}
	}
	return &LaunchResult{Started: true, Alias: alias, Terminal: terminal, ConfigDir: home}, nil
}
