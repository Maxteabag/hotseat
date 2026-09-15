package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Window struct {
	Label string  `json:"label"`
	Used  float64 `json:"used"`
	Reset float64 `json:"reset"`
}
type Account struct {
	Cached            bool     `json:"cached"`
	Warning           string   `json:"warning"`
	DisplayAlias      string   `json:"display_alias"`
	Aliases           []string `json:"aliases"`
	ProfileNotes      []string `json:"profile_notes"`
	ResetCredits      *int     `json:"reset_credits"`
	ResetCreditsError string   `json:"reset_credits_error"`
	Provider          string   `json:"provider"`
	Alias             string   `json:"alias"`
	Email             string   `json:"email"`
	Plan              string   `json:"plan"`
	Workspace         string   `json:"workspace"`
	Active            bool     `json:"active"`
	Saved             bool     `json:"saved"`
	CanSwitch         bool     `json:"can_switch"`
	CanLaunch         bool     `json:"can_launch"`
	Limited           bool     `json:"limited"`
	Windows           []Window `json:"windows"`
	Error             string   `json:"error"`
	CheckedAt         float64  `json:"checked_at"`
}

func (a Account) Name() string {
	if a.DisplayAlias != "" {
		return a.DisplayAlias
	}
	return a.Alias
}
func (a Account) ID() string { return a.Provider + ":" + a.Name() }
func (a Account) Status() string {
	if a.Error != "" {
		return "Check failed"
	}
	if a.CheckedAt == 0 || len(a.Windows) == 0 {
		return "Unknown"
	}
	if a.Limited {
		return "Limited"
	}
	for _, w := range a.Windows {
		if w.Used >= 1 {
			return "Some limits"
		}
	}
	return "Available"
}

type Snapshot struct {
	Accounts    []Account `json:"accounts"`
	GeneratedAt float64   `json:"generated_at"`
	Errors      []string  `json:"errors"`
	Sessions    int       `json:"sessions"`
}
type Backend interface {
	Snapshot(context.Context, bool) (Snapshot, error)
	Action(context.Context, Account, string) error
}
type PythonBackend struct {
	Python, Root string
	mu           sync.Mutex
	closed       bool
	running      map[*exec.Cmd]chan struct{}
}

func (b *PythonBackend) execute(ctx context.Context, args ...string) ([]byte, error) {
	cmd := b.command(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, context.Canceled
	}
	if err := cmd.Start(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if b.running == nil {
		b.running = make(map[*exec.Cmd]chan struct{})
	}
	done := make(chan struct{})
	b.running[cmd] = done
	b.mu.Unlock()
	err := cmd.Wait()
	b.mu.Lock()
	delete(b.running, cmd)
	close(done)
	b.mu.Unlock()
	return out.Bytes(), err
}

// Close prevents new work, terminates owned process groups, and waits for reaping.
func (b *PythonBackend) Close() {
	b.mu.Lock()
	b.closed = true
	waits := []chan struct{}{}
	for cmd, done := range b.running {
		_ = cmd.Cancel()
		waits = append(waits, done)
	}
	b.mu.Unlock()
	for _, done := range waits {
		<-done
	}
}

func (b *PythonBackend) command(ctx context.Context, args ...string) *exec.Cmd {
	invocation := append([]string{"-m", "hotseat.tui_bridge"}, args...)
	if b.Root != "" {
		invocation = append([]string{"-c", "import runpy,sys;sys.path.insert(0,sys.argv.pop(1));runpy.run_module('hotseat.tui_bridge',run_name='__main__')", b.Root}, args...)
	}
	cmd := exec.CommandContext(ctx, b.Python, invocation...)
	isolateCommand(cmd)
	return cmd
}
func (b *PythonBackend) Snapshot(ctx context.Context, refresh bool) (Snapshot, error) {
	var s Snapshot
	args := []string{"snapshot"}
	if refresh {
		args = append(args, "--refresh")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	out, err := b.execute(ctx, args...)
	if err != nil {
		return s, fmt.Errorf("backend failed: %w (Python: %s)", err, b.Python)
	}
	if err = json.Unmarshal(out, &s); err != nil {
		return s, fmt.Errorf("invalid backend response: %w", err)
	}
	return s, nil
}
func (b *PythonBackend) Action(ctx context.Context, a Account, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := b.execute(ctx, operation, "--provider", a.Provider, "--alias", a.Alias, "--acknowledged")
	var result struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(out, &result)
	if result.Error != "" {
		return fmt.Errorf("%s", result.Error)
	}
	return err
}
func FindRoot() string {
	if root := os.Getenv("HOTSEAT_ROOT"); root != "" {
		return root
	}
	cwd, _ := os.Getwd()
	exe, _ := os.Executable()
	for _, start := range []string{cwd, filepath.Dir(exe)} {
		for p := start; ; p = filepath.Dir(p) {
			if _, err := os.Stat(filepath.Join(p, "hotseat", "tui_bridge.py")); err == nil {
				return p
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return ""
}
