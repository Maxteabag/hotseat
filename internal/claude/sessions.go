package claude

// Count Claude Code processes currently running on this machine.
//
// This number is the whole reason the dashboard treats switching as dangerous:
// changing the machine-wide default retargets every one of them mid-conversation.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const psTimeout = 10 * time.Second

// RunningSessions is a best-effort count of live `claude` processes owned by
// this user.
//
// Returns 0 rather than failing if the process list cannot be read: a wrong
// count must never stop the dashboard from rendering.
func RunningSessions() int {
	return RunningSessionsWith(nil)
}

// RunningSessionsWith is RunningSessions with an injectable command runner
// (nil means (*exec.Cmd).Output).
func RunningSessionsWith(run RunOutput) int {
	if run == nil {
		run = func(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := run(exec.CommandContext(ctx, "ps", "-eo", "pid=,args="))
	if err != nil || ctx.Err() != nil {
		return 0
	}
	return CountSessions(string(out), os.Getpid())
}

// CountSessions parses `ps -eo pid=,args=` output, counting `claude` and
// `claude-*` executables other than the process `self` and anything that is
// part of hotseat itself.
func CountSessions(psOutput string, self int) int {
	count := 0
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if pid == self {
			continue
		}
		args := strings.Join(fields[1:], " ")
		name := filepath.Base(fields[1])
		// Match the CLI itself, not this dashboard or an editor holding the word.
		if name == "claude" || strings.HasPrefix(name, "claude-") {
			if strings.Contains(args, "hotseat") {
				continue
			}
			count++
		}
	}
	return count
}
