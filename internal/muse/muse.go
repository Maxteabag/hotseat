// Package muse reads the Meta Muse Code subscription on this machine.
//
// The `muse` CLI keeps one signed-in Meta account in ~/.config/muse/auth.json
// (MUSE_CONFIG_DIR overrides the directory). Identity is read from that file;
// the tokens in it are never read into Hotseat's output or sent anywhere.
//
// Quota comes from `muse usage --json`, which prints the subscription
// allowance without a model turn. Muse Code has no documented usage endpoint:
// the CLI derives the allowance from what the server returns on its own
// requests (its JSON reports "source": "mint"), so calling the CLI is the one
// stable reading and keeps the credential inside the CLI. Before the first use
// in a new window the server sends no figures; that is reported as a reason,
// not an error.
package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// UsageTimeout bounds one `muse usage` call. It makes no model turn, so a
// reading that takes longer than this is stuck rather than slow.
const UsageTimeout = 20 * time.Second

// Window is one allowance window: the rolling 5-hour window or the week.
type Window struct {
	Label       string   `json:"label"`
	UsedPercent *float64 `json:"used_percent"`
	// ResetsAt is epoch seconds; nil when the server did not say.
	ResetsAt *float64 `json:"resets_at"`
}

// Account is the signed-in Muse Code account and its allowance.
type Account struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Mechanism string `json:"mechanism"`
	// Subscribed is the CLI's own record of an active subscription at sign-in.
	Subscribed bool     `json:"subscribed"`
	Tier       string   `json:"tier"`
	Windows    []Window `json:"windows"`
	// AsOf is when the allowance was read by the CLI, epoch seconds.
	AsOf *float64 `json:"as_of"`
	// Reason explains missing figures, e.g. server_sent_no_usage before the
	// first use in a new window.
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
	// Error is a failed reading; the account is still listed.
	Error string `json:"error,omitempty"`
}

// Overview is what the muse command and the snapshot publish. It is nil when
// Muse Code is not installed and has never been signed in.
type Overview struct {
	Installed bool     `json:"installed"`
	LoggedIn  bool     `json:"logged_in"`
	Account   *Account `json:"account"`
}

// Runner runs argv and returns stdout, stderr and the exit code. A non-zero
// exit is not an error; the error is for a command that could not run.
type Runner func(ctx context.Context, argv []string) (stdout, stderr []byte, code int, err error)

// Muse reads one Muse configuration. The zero value reads the real machine.
type Muse struct {
	// ConfigDir holds auth.json; empty means DefaultConfigDir().
	ConfigDir string
	// Binary is the muse executable; empty means Find().
	Binary string
	// Run executes the CLI; nil means exec with UsageTimeout.
	Run Runner
}

// DefaultConfigDir is $MUSE_CONFIG_DIR, else $XDG_CONFIG_HOME/muse, else ~/.config/muse.
func DefaultConfigDir() string {
	if dir := os.Getenv("MUSE_CONFIG_DIR"); dir != "" {
		return dir
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "muse")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "muse")
}

// Find locates the muse launcher: PATH first, then the installer's default
// ~/.local/bin, which a non-login shell often lacks. Empty when absent.
func Find() string {
	if path, err := exec.LookPath("muse"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".local", "bin", "muse")
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func (m *Muse) configDir() string {
	if m.ConfigDir == "" {
		return DefaultConfigDir()
	}
	return m.ConfigDir
}

func (m *Muse) binary() string {
	if m.Binary == "" {
		return Find()
	}
	return m.Binary
}

func (m *Muse) run(ctx context.Context, argv []string) ([]byte, []byte, int, error) {
	if m.Run != nil {
		return m.Run(ctx, argv)
	}
	ctx, cancel := context.WithTimeout(ctx, UsageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.Bytes(), stderr.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return nil, nil, -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

// identity is the public part of auth.json; the tokens are deliberately not
// fields here, so they never leave the file.
type identity struct {
	Mechanism  string `json:"mechanism"`
	UserEmail  string `json:"user_email"`
	UserName   string `json:"user_full_name"`
	SubsActive bool   `json:"subs_active"`
}

// readIdentity returns the signed-in Meta account, or nil when there is none.
func (m *Muse) readIdentity() (*identity, error) {
	raw, err := os.ReadFile(filepath.Join(m.configDir(), "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Providers map[string]identity `json:"providers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("auth.json: %w", err)
	}
	meta, ok := file.Providers["meta"]
	if !ok {
		return nil, nil
	}
	return &meta, nil
}

// usage is `muse usage --json`, schema version 1.
type usage struct {
	SchemaVersion int `json:"schemaVersion"`
	Allowance     *struct {
		AsOfMs float64 `json:"asOfMs"`
		Source string  `json:"source"`
		Tier   string  `json:"tier"`
		Window *window `json:"window"`
		Weekly *window `json:"weekly"`
	} `json:"allowance"`
	Hint   *string `json:"hint"`
	Reason *string `json:"reason"`
}

type window struct {
	ResetsAtMs  *float64 `json:"resetsAtMs"`
	UsedPercent *float64 `json:"usedPercent"`
}

func (w *window) public(label string) Window {
	out := Window{Label: label, UsedPercent: w.UsedPercent}
	if w.ResetsAtMs != nil {
		seconds := *w.ResetsAtMs / 1000
		out.ResetsAt = &seconds
	}
	return out
}

// ParseUsage turns `muse usage --json` output into the account's tier,
// windows and reasons. Exported for tests and for callers holding the JSON.
func ParseUsage(data []byte, account *Account) error {
	var u usage
	if err := json.Unmarshal(bytes.TrimSpace(data), &u); err != nil {
		return fmt.Errorf("muse usage: %w", err)
	}
	if u.SchemaVersion != 1 {
		return fmt.Errorf("muse usage: unsupported schemaVersion %d", u.SchemaVersion)
	}
	if u.Reason != nil {
		account.Reason = *u.Reason
	}
	if u.Hint != nil {
		account.Hint = *u.Hint
	}
	if a := u.Allowance; a != nil {
		account.Tier = a.Tier
		if a.AsOfMs != 0 {
			seconds := a.AsOfMs / 1000
			account.AsOf = &seconds
		}
		if a.Window != nil {
			account.Windows = append(account.Windows, a.Window.public("5-hour"))
		}
		if a.Weekly != nil {
			account.Windows = append(account.Windows, a.Weekly.public("weekly"))
		}
	}
	return nil
}

// Overview reads the signed-in account and its allowance. It returns nil, nil
// when Muse Code is neither installed nor signed in: absent is a normal state.
func (m *Muse) Overview(ctx context.Context) (*Overview, error) {
	return m.overview(ctx, m.binary())
}

// overview is Overview with the executable already resolved; "" means absent.
func (m *Muse) overview(ctx context.Context, binary string) (*Overview, error) {
	who, err := m.readIdentity()
	if err != nil {
		return nil, err
	}
	if binary == "" && who == nil {
		return nil, nil
	}
	overview := &Overview{Installed: binary != "", LoggedIn: who != nil}
	if who == nil {
		return overview, nil
	}
	account := &Account{Email: who.UserEmail, Name: who.UserName, Mechanism: who.Mechanism, Subscribed: who.SubsActive,
		Windows: []Window{}}
	overview.Account = account
	if binary == "" {
		account.Error = "muse CLI not found; install it to read quota"
		return overview, nil
	}
	stdout, stderr, code, err := m.run(ctx, []string{binary, "usage", "--json"})
	switch {
	case err != nil:
		account.Error = "muse usage: " + err.Error()
	case code != 0:
		msg := string(bytes.TrimSpace(stderr))
		if msg == "" {
			msg = string(bytes.TrimSpace(stdout))
		}
		account.Error = fmt.Sprintf("muse usage exited %d: %s", code, msg)
	default:
		if err := ParseUsage(stdout, account); err != nil {
			account.Error = err.Error()
		}
	}
	return overview, nil
}
