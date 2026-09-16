package codex

// Smoke tests for the real subprocess paths, using only POSIX tools and
// skipping where they are absent. Nothing here runs codex.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available", name)
	}
}

func TestRunCommandCapturesOutputAndExitCode(t *testing.T) {
	requireTool(t, "sh")
	result, err := RunCommand(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != 3 || result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("result = %+v", result)
	}
	result, err = RunCommand(context.Background(), []string{"sh", "-c", "echo \"$CODEX_HOME\""}, []string{"CODEX_HOME=/pinned"})
	if err != nil || result.Stdout != "/pinned\n" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestRunCommandReportsAMissingBinaryAndATimeout(t *testing.T) {
	if _, err := RunCommand(context.Background(), []string{filepath.Join(t.TempDir(), "absent")}, nil); err == nil {
		t.Fatal("a missing binary must be an error, not a panic")
	}
	requireTool(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := RunCommand(ctx, []string{"sleep", "5"}, nil); err == nil {
		t.Fatal("a timeout must be reported")
	}
}

func TestSpawnCommandDrivesARealProcess(t *testing.T) {
	// cat echoes each request, which the client accepts as an empty result.
	requireTool(t, "cat")
	c := New(t.TempDir())
	c.Spawn = func(ctx context.Context, argv []string, env []string) (Process, error) {
		return SpawnCommand(ctx, []string{"cat"}, env)
	}
	auth := filepath.Join(c.Home, "auth.json")
	if err := os.WriteFile(auth, credential("live@example.com", "live"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []Row, 1)
	go func() { done <- c.Compare(context.Background(), nil) }()
	select {
	case rows := <-done:
		if len(rows) != 1 || rows[0].Error != nil || rows[0].Name != LiveName {
			t.Fatalf("rows = %+v", rows)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the real process was not terminated")
	}
}

func TestEnvironWithoutNeverMutatesTheProcessEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", "/original")
	t.Setenv("HOTSEAT_TEST_KEEP", "1")
	env := isolatedEnv("/isolated")
	if envValue(env, "CODEX_HOME") != "/isolated" || envValue(env, "HOTSEAT_TEST_KEEP") != "1" {
		t.Fatalf("env = %v", env)
	}
	if os.Getenv("CODEX_HOME") != "/original" {
		t.Fatal("process environment changed")
	}
	count := 0
	for _, entry := range env {
		if len(entry) >= 11 && entry[:11] == "CODEX_HOME=" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("CODEX_HOME appears %d times", count)
	}
}

func TestAtomicWriteLeavesNoTemporaryBehind(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "auth.json")
	source := filepath.Join(dir, "source.json")
	if err := os.WriteFile(source, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicCopy(source, target); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(target); string(raw) != `{"a":1}` {
		t.Fatalf("content = %s", raw)
	}
}

func TestRunCommandIsNotHeldOpenByAnOrphanedChild(t *testing.T) {
	// A grandchild that inherited the output pipes must not keep RunCommand
	// waiting past the deadline, nor past a clean exit.
	requireTool(t, "sh")
	requireTool(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := RunCommand(ctx, []string{"sh", "-c", "( sleep 6 & ); sleep 6"}, nil); err == nil {
		t.Fatal("a timeout must be reported")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("RunCommand held open for %s by the orphaned child after the timeout", elapsed)
	}

	started = time.Now()
	result, err := RunCommand(context.Background(), []string{"sh", "-c", "( sleep 6 & ); echo done; exit 4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("RunCommand held open for %s by the orphaned child after a normal exit", elapsed)
	}
	if result.Code != 4 || result.Stdout != "done\n" {
		t.Fatalf("result = %+v", result)
	}
}
