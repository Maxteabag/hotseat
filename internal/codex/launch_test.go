package codex

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLauncher records what a launch would have opened.
type fakeLauncher struct {
	terminal string
	argv     []string
	env      []string
	spawnErr error
	spawned  int
}

func (l *fakeLauncher) Detect() (string, error) { return l.terminal, nil }
func (l *fakeLauncher) Flag(string) string      { return "-e" }
func (l *fakeLauncher) Spawn(argv, env []string) error {
	l.spawned++
	l.argv, l.env = argv, env
	return l.spawnErr
}

type launchFixture struct {
	shared  string
	profile string
	codex   *Codex
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	shared := filepath.Join(t.TempDir(), "codex")
	profile := filepath.Join(shared, "profiles", "work")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(profile, "auth.json"):  `{"test_identity":"work"}`,
		filepath.Join(shared, "auth.json"):   `{"test_identity":"global"}`,
		filepath.Join(shared, "config.toml"): `model = "example"`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := New(shared)
	c.Which = func(string) (string, error) { return "/usr/bin/codex", nil }
	return &launchFixture{shared: shared, profile: profile, codex: c}
}

func readIdentity(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded["test_identity"]
}

func TestLaunchUsesCanonicalProfileAndPreservesDefault(t *testing.T) {
	f := newLaunchFixture(t)
	t.Setenv("OPENAI_API_KEY", "secret-key")
	t.Setenv("CODEX_ACCESS_TOKEN", "secret-token")
	t.Setenv("CODEX_HOME", "/elsewhere")
	launcher := &fakeLauncher{}
	result, err := f.codex.Launch("work", launcher, "kitty")
	if err != nil {
		t.Fatal(err)
	}
	argv, env := launcher.argv, launcher.env
	if strings.Join(argv[:3], " ") != "kitty -e env" {
		t.Fatalf("argv = %q", argv)
	}
	if envValue(env, "CODEX_HOME") != f.profile {
		t.Fatalf("CODEX_HOME = %q", envValue(env, "CODEX_HOME"))
	}
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "CODEX_HOME="+f.profile) {
		t.Fatalf("argv lacks CODEX_HOME: %q", argv)
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_ACCESS_TOKEN"} {
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("%s leaked into the launch environment", key)
			}
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("secret in argv: %q", argv)
	}
	// What a child started with this environment would read as its identity.
	if got := readIdentity(t, filepath.Join(envValue(env, "CODEX_HOME"), "auth.json")); got != "work" {
		t.Fatalf("child identity = %q", got)
	}
	if got := readIdentity(t, filepath.Join(f.shared, "auth.json")); got != "global" {
		t.Fatalf("shared identity = %q", got)
	}
	if !isSymlink(filepath.Join(f.profile, "config.toml")) {
		t.Fatal("config.toml must be symlinked into the profile")
	}
	target, _ := os.Readlink(filepath.Join(f.profile, "config.toml"))
	if resolved, _ := filepath.EvalSymlinks(filepath.Join(f.shared, "config.toml")); target != resolved {
		t.Fatalf("symlink target = %q", target)
	}
	if result.ConfigDir != f.profile || !result.Started || result.Alias != "work" || result.Terminal != "kitty" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"kitty", "-e", "env", "-u", "OPENAI_API_KEY", "-u", "OPENAI_BASE_URL", "-u", "CODEX_ACCESS_TOKEN",
		"-u", "CODEX_THREAD_ID", "-u", "CODEX_SESSION_ID", "CODEX_HOME=" + f.profile, "/usr/bin/codex", "-c",
		`cli_auth_credentials_store="file"`}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q", argv)
	}
	info, _ := os.Stat(f.profile)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("profile mode = %o", info.Mode().Perm())
	}
	if os.Getenv("CODEX_HOME") != "/elsewhere" {
		t.Fatal("the process environment must not be mutated")
	}
	raw, _ := json.Marshal(result)
	if string(raw) != `{"started":true,"alias":"work","terminal":"kitty","config_dir":"`+f.profile+`"}` {
		t.Fatalf("json = %s", raw)
	}
}

func TestExistingProfileConfigIsPreserved(t *testing.T) {
	f := newLaunchFixture(t)
	if err := os.WriteFile(filepath.Join(f.profile, "config.toml"), []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.codex.Prepare("work"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.profile, "config.toml"))
	if string(raw) != "custom" {
		t.Fatalf("config = %q", raw)
	}
	// Shared entries that do not exist are not linked; a dangling link is left.
	if isSymlink(filepath.Join(f.profile, "AGENTS.md")) {
		t.Fatal("absent shared entries must not be linked")
	}
}

func TestUnknownAndTraversalNamesAreRejected(t *testing.T) {
	f := newLaunchFixture(t)
	for alias, want := range map[string]string{"../other": "Invalid saved profile name", "missing": "Saved Codex account not found"} {
		_, _, err := f.codex.Prepare(alias)
		var lerr *LaunchError
		if !errors.As(err, &lerr) || lerr.Msg != want {
			t.Fatalf("%q: err = %v", alias, err)
		}
	}
}

func TestSymlinkedAuthCannotTargetGlobalCredentials(t *testing.T) {
	f := newLaunchFixture(t)
	os.Remove(filepath.Join(f.profile, "auth.json"))
	if err := os.Symlink(filepath.Join(f.shared, "auth.json"), filepath.Join(f.profile, "auth.json")); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.codex.Prepare("work")
	var lerr *LaunchError
	if !errors.As(err, &lerr) || lerr.Msg != "Refusing a symlinked profile directory" {
		t.Fatalf("err = %v", err)
	}
}

func TestSymlinkedProfileDirectoryIsRefused(t *testing.T) {
	f := newLaunchFixture(t)
	real := filepath.Join(t.TempDir(), "real")
	if err := os.Rename(f.profile, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, f.profile); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.codex.Prepare("work")
	var lerr *LaunchError
	if !errors.As(err, &lerr) || lerr.Msg != "Refusing a symlinked profile directory" {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchDetectsTheTerminalWhenNoneIsGiven(t *testing.T) {
	f := newLaunchFixture(t)
	launcher := &fakeLauncher{terminal: "foot"}
	result, err := f.codex.Launch("work", launcher, "")
	if err != nil || result.Terminal != "foot" || launcher.argv[0] != "foot" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	_, err = f.codex.Launch("work", &fakeLauncher{}, "")
	var lerr *LaunchError
	if !errors.As(err, &lerr) || lerr.Msg != "No supported terminal found" {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchRefusesWithoutCodexOrTerminal(t *testing.T) {
	f := newLaunchFixture(t)
	f.codex.Which = func(string) (string, error) { return "", errors.New("not found") }
	launcher := &fakeLauncher{}
	_, err := f.codex.Launch("work", launcher, "kitty")
	var lerr *LaunchError
	if !errors.As(err, &lerr) || lerr.Msg != "Codex CLI is not installed" || launcher.spawned != 0 {
		t.Fatalf("err = %v spawned = %d", err, launcher.spawned)
	}
	f.codex.Which = func(string) (string, error) { return "/usr/bin/codex", nil }
	launcher.spawnErr = io.ErrClosedPipe
	_, err = f.codex.Launch("work", launcher, "kitty")
	if !errors.As(err, &lerr) || !strings.HasPrefix(lerr.Msg, "Could not start kitty: ") {
		t.Fatalf("err = %v", err)
	}
}
