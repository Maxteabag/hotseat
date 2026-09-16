package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Result is what a finished subprocess left behind.
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// Runner runs argv to completion with the given environment (nil inherits the
// parent's) and captures its output. A non-zero exit is a Result, not an error;
// the error is for a command that could not run or ran out of time.
type Runner func(ctx context.Context, argv []string, env []string) (Result, error)

// Caller runs argv attached to the user's terminal, for interactive logins, and
// returns its exit code.
type Caller func(ctx context.Context, argv []string, env []string) (int, error)

// RunCommand is the Runner used when none is injected.
func RunCommand(ctx context.Context, argv []string, env []string) (Result, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Code: -1, Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil {
			return result, fmt.Errorf("%s timed out: %w", argv[0], ctx.Err())
		}
		if errors.As(err, &exit) {
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// CallCommand is the Caller used when none is injected: stdin, stdout and
// stderr are the user's own.
func CallCommand(ctx context.Context, argv []string, env []string) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// environWithout copies the process environment, dropping the named variables.
// The process environment itself is never mutated.
func environWithout(names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, name := range names {
		drop[name] = true
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !drop[key] {
			env = append(env, entry)
		}
	}
	return env
}

// isolatedEnv is the parent environment pointed at another CODEX_HOME.
func isolatedEnv(home string) []string {
	return append(environWithout("CODEX_HOME"), "CODEX_HOME="+home)
}

// envValue reads one variable from an environment slice; "" when unset.
func envValue(env []string, name string) string {
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value
		}
	}
	return ""
}

// atomicCopy writes source's bytes to target through a temporary file in the
// same directory, so a reader never sees a half-written credential.
func atomicCopy(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicWrite(target, payload)
}

func atomicWrite(target string, payload []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".auth-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

// copyFile copies bytes and mode, like shutil.copy.
func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, payload, info.Mode().Perm())
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
