package claude

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

const psFixture = `    1 /sbin/init
  200 /usr/bin/claude
  201 claude --resume abc
  202 /home/u/.local/bin/claude-code-wrapper
  203 python3 -m hotseat.tui
  204 claude --model haiku hotseat
  205 /usr/bin/nvim claude.py
  206 claude-desktop
  junk line
  999 claude
`

func TestCountSessionsMatchesTheCLIOnly(t *testing.T) {
	// 200, 201, 202, 206 count; 204 mentions hotseat; 205 is an editor; 999 is us.
	if got := CountSessions(psFixture, 999); got != 4 {
		t.Fatalf("count = %d", got)
	}
	if got := CountSessions(psFixture, 1); got != 5 {
		t.Fatalf("count = %d", got)
	}
	if got := CountSessions("", 1); got != 0 {
		t.Fatalf("count = %d", got)
	}
}

func TestRunningSessionsWithInjectedPs(t *testing.T) {
	var argv []string
	got := RunningSessionsWith(func(cmd *exec.Cmd) ([]byte, error) {
		argv = cmd.Args
		return []byte(psFixture), nil
	})
	if !equalStrings(argv, []string{"ps", "-eo", "pid=,args="}) {
		t.Fatalf("argv = %v", argv)
	}
	if got != 5 {
		t.Fatalf("count = %d", got)
	}
	if got := RunningSessionsWith(func(*exec.Cmd) ([]byte, error) { return nil, errors.New("no ps") }); got != 0 {
		t.Fatalf("failure must count as zero, got %d", got)
	}
	if got := RunningSessionsWith(func(*exec.Cmd) ([]byte, error) { return []byte(psFixture), fakeExit{1} }); got != 0 {
		t.Fatalf("non-zero exit must count as zero, got %d", got)
	}
}

func TestRunningSessionsSkipsItself(t *testing.T) {
	self := os.Getpid()
	out := []byte("  " + itoa(self) + " claude\n")
	if got := RunningSessionsWith(func(*exec.Cmd) ([]byte, error) { return out, nil }); got != 0 {
		t.Fatalf("our own pid counted: %d", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
