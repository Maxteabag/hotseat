package collect

import (
	"context"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/work"
)

// codexSessions is codex.Sessions as the work package sees it.
type codexSessions struct{ *codex.Sessions }

func (s *codexSessions) Recent(ctx context.Context, limit int) ([]work.CodexRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	found, err := s.Sessions.Recent(limit)
	if err != nil {
		return nil, err
	}
	rows := make([]work.CodexRow, 0, len(found))
	for _, entry := range found {
		rows = append(rows, work.CodexRow{
			ThreadID: entry.ThreadID, Short: entry.Short, LastAt: entry.LastAt, Turns: entry.Turns,
			Failed: entry.Failed, Active: entry.Active, LastStatus: entry.LastStatus, Topic: entry.Topic,
			Holders: entry.Holders, Live: entry.Live, State: entry.State,
		})
	}
	return rows, nil
}

func (s *codexSessions) LockHolders(threadID string) ([]int, error) {
	return s.Sessions.LockHolders(threadID), nil
}

// terminalLauncher opens windows in the user's terminal. It is both the
// work.Launcher (a command in a directory) and the codex.Launcher (detect, flag,
// spawn) so one detection path serves every launch.
type terminalLauncher struct {
	spawn    claude.Spawn
	terminal string
}

func (l *terminalLauncher) LaunchCommand(argv []string, cwd string) error {
	_, err := claude.LaunchCommand(argv, cwd, l.spawn, l.terminal)
	return err
}

func (l *terminalLauncher) Detect() (string, error) {
	if l.terminal != "" {
		return l.terminal, nil
	}
	return claude.DetectTerminal(), nil
}

func (l *terminalLauncher) Flag(terminal string) string { return claude.TerminalFlag(terminal) }

func (l *terminalLauncher) Spawn(argv []string, env []string) error {
	if l.spawn != nil {
		return l.spawn(argv, env)
	}
	return claude.DetachedSpawn(argv, env)
}
