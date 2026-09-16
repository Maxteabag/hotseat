// Command hotseat is the command line, which is the primary interface.
//
// Every capability is reachable from here, and each command takes `--json` so it
// can be scripted. The TUI is one optional consumer of the same core functions;
// it is not privileged, and nothing is reachable only through it.
//
// Exit codes: 0 success, 1 the operation failed, 2 usage error, 130 interrupted.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Maxteabag/hotseat/internal/clarp"
	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/collect"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/version"
	"github.com/Maxteabag/hotseat/internal/work"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code := newApp().run(ctx, os.Args[1:])
	if ctx.Err() != nil {
		code = 130
	}
	os.Exit(code)
}

// sessionStore is the part of *codex.Sessions the CLI uses.
type sessionStore interface {
	Recent(limit int) ([]codex.Session, error)
	Resolve(prefix string) (*codex.Session, error)
	Nudge(ctx context.Context, threadID, message string) (*codex.Nudged, error)
	Release(threadID string, signal codex.Signaller) ([]int, error)
	LockHolders(threadID string) []int
}

// app carries the streams and the seams the Python tests patched. newApp wires
// the real machine; tests replace individual fields.
type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	stdoutIsTTY    func() bool
	stdinIsTTY     func() bool
	now            func() time.Time
	sleep          func(time.Duration)

	build           func(ctx context.Context) (collect.Snapshot, error)
	backend         func() claude.Backend
	runningSessions func() int
	verify          func(ctx context.Context, backend claude.Backend, alias string) (claude.VerifyResult, error)
	execSession     func(backend claude.Backend, alias string, args []string) error
	launch          func(backend claude.Backend, alias string) (claude.LaunchResult, error)
	switchDefault   func(backend claude.Backend, alias string, sessions int) (claude.SwitchResult, error)
	refresh         func(backend claude.Backend, alias string, force bool) (claude.RefreshResult, error)
	expired         func(account claude.Account) bool
	modelUsage      func(days int) *claude.ModelSummary
	statusline      func() string

	codexOverview func(ctx context.Context, withLimits bool) (*codex.Overview, error)
	sessions      func() sessionStore
	launchCommand func(argv []string, cwd string) (claude.LaunchCommandResult, error)
	balances      func(ctx context.Context, aliases []string) ([]codex.Balance, error)
	codexAccount  func(ctx context.Context, request codexAccountRequest) error

	clarpAvailable func() bool
	stopped        func(ctx context.Context, days int) ([]work.StoppedItem, error)
	continueItem   func(ctx context.Context, item work.StoppedItem) (work.Continuation, error)
	inspect        func(ctx context.Context, identifier string) (*work.Detail, error)
	clarpReport    func(ctx context.Context, defaultAlias string, liveOnly bool, backend string) (*clarp.Report, error)

	runTUI func(ctx context.Context, options tuiOptions) int
	bridge func(ctx context.Context, args []string) int
}

func newApp() *app {
	var (
		codexClient *codex.Codex
		store       *codex.Sessions
		clarpSvc    *clarp.Service
	)
	codexHome := func() *codex.Codex {
		if codexClient == nil {
			codexClient = codex.New(codex.DefaultHome())
		}
		return codexClient
	}
	clarpService := func() *clarp.Service {
		if clarpSvc == nil {
			clarpSvc = clarp.New()
		}
		return clarpSvc
	}
	a := &app{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		stdin:       os.Stdin,
		stdoutIsTTY: func() bool { return isTerminal(os.Stdout) },
		stdinIsTTY:  func() bool { return isTerminal(os.Stdin) },
		now:         time.Now,
		sleep:       time.Sleep,

		build:           func(ctx context.Context) (collect.Snapshot, error) { return collect.NewCollector().Build(ctx) },
		backend:         func() claude.Backend { return claude.ForPlatform("") },
		runningSessions: claude.RunningSessions,
		verify: func(ctx context.Context, backend claude.Backend, alias string) (claude.VerifyResult, error) {
			return claude.Verify(ctx, backend, alias, headerProber{})
		},
		execSession: func(backend claude.Backend, alias string, args []string) error {
			return claude.ExecSession(backend, alias, args, nil)
		},
		launch: func(backend claude.Backend, alias string) (claude.LaunchResult, error) {
			return claude.Launch(backend, alias, nil, "")
		},
		switchDefault: func(backend claude.Backend, alias string, sessions int) (claude.SwitchResult, error) {
			return claude.Switch(backend, alias, sessions, true)
		},
		refresh: func(backend claude.Backend, alias string, force bool) (claude.RefreshResult, error) {
			return claude.Refresh(backend, alias, claude.RefreshOptions{Force: force})
		},
		expired: func(account claude.Account) bool {
			return claude.Expired(account, float64(time.Now().UnixNano())/1e9, claude.RefreshMarginS())
		},
		modelUsage: func(days int) *claude.ModelSummary { return claude.RecentByModel(days, "", time.Time{}) },
		statusline: func() string { return claude.Render("") },

		codexOverview: func(ctx context.Context, withLimits bool) (*codex.Overview, error) {
			return codexHome().Overview(ctx, withLimits, 0)
		},
		sessions: func() sessionStore {
			if store == nil {
				store = codex.NewSessions(codex.DefaultHome())
			}
			return store
		},
		launchCommand: func(argv []string, cwd string) (claude.LaunchCommandResult, error) {
			return claude.LaunchCommand(argv, cwd, nil, "")
		},
		balances: func(ctx context.Context, aliases []string) ([]codex.Balance, error) {
			return codexHome().Balances(ctx, aliases)
		},

		clarpAvailable: clarp.Available,
		stopped: func(ctx context.Context, days int) ([]work.StoppedItem, error) {
			if clarp.Available() {
				return clarpService().Stopped(ctx, days, true)
			}
			return work.New(nil, nil).Stopped(days, true), nil
		},
		continueItem: func(ctx context.Context, item work.StoppedItem) (work.Continuation, error) {
			if clarp.Available() {
				return clarpService().Continue(ctx, item, "")
			}
			return work.ContinueItem(item)
		},
		inspect: func(ctx context.Context, identifier string) (*work.Detail, error) {
			if clarp.Available() {
				detail, err := clarpService().Inspect(ctx, identifier)
				if err != nil || detail != nil {
					return detail, err
				}
			}
			return work.Inspect(work.DefaultProjectsDir(), identifier)
		},
		clarpReport: func(ctx context.Context, defaultAlias string, liveOnly bool, backend string) (*clarp.Report, error) {
			return clarpService().Report(ctx, defaultAlias, liveOnly, backend)
		},
	}
	a.codexAccount = func(ctx context.Context, request codexAccountRequest) error {
		return request.run(ctx, codex.NewProfiles(codex.DefaultHome(), a.stdout))
	}
	a.runTUI = a.realTUI
	a.bridge = a.realBridge
	return a
}

// headerProber is the rate-limit probe as claude.Verify wants it.
type headerProber struct{}

func (headerProber) Probe(ctx context.Context, token string) (map[string]string, error) {
	headers, err := quota.Probe(ctx, token)
	return map[string]string(headers), err
}

func (headerProber) Summarise(headers map[string]string) any {
	return quota.SummariseHeaders(quota.Headers(headers))
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (a *app) colour() bool { return a.stdoutIsTTY() }

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// --- wiring ---------------------------------------------------------------

// command is one subcommand: its help line, whether it takes --json, and the
// positional arguments its usage line names.
type command struct {
	name       string
	help       string
	json       bool
	positional string
	run        func(a *app, cmd *command, ctx context.Context, args []string) (int, error)
}

var commands = []command{
	{"tui", "Open the Bubble Tea account desk", false, "", (*app).cmdTUI},
	{"list", "Show every account, its quota and its sign-in deadline", true, "", (*app).cmdList},
	{"show", "Everything known about one account", true, "alias", (*app).cmdShow},
	{"verify", "Prove an account works by calling the API", true, "alias", (*app).cmdVerify},
	{"use", "Run a session pinned to an account, in this terminal", false, "alias ...", (*app).cmdUse},
	{"window", "Open a new terminal window pinned to an account", true, "alias", (*app).cmdWindow},
	{"refresh", "Refresh expired access tokens of saved profiles through the CLI", true, "[alias ...]", (*app).cmdRefresh},
	{"switch", "Change the machine-wide default account (affects running sessions)", true, "alias", (*app).cmdSwitch},
	{"models", "Token usage by model, across all accounts", true, "", (*app).cmdModels},
	{"inspect", "What a stopped agent or session was working on", true, "id", (*app).cmdInspect},
	{"resume", "List work stopped by a usage limit, and continue it", true, "", (*app).cmdResume},
	{"sessions", "Recent Codex sessions and their state", true, "", (*app).cmdSessions},
	{"nudge", "Send a message to a running Codex session", true, "id message", (*app).cmdNudge},
	{"reboot", "Close a stuck Codex session and resume the same conversation", true, "id", (*app).cmdReboot},
	{"resets", "Read available Codex usage-reset credits (no redemption)", true, "[aliases ...]", (*app).cmdResets},
	{"codex-account", "Codex profiles: list, current, save, switch, login, verify, probe, remove", false, "...", (*app).cmdCodexAccount},
	{"codex", "Codex accounts and their quota", true, "", (*app).cmdCodex},
	{"statusline", "Print the current account, for a status line", false, "", (*app).cmdStatusline},
	{"clarp", "Clarp agents and account usage", true, "", (*app).cmdClarp},
}

func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func commandNames() []string {
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.name)
	}
	return names
}

// usageError is a bad invocation: printed argparse-style, exit code 2.
type usageError struct {
	prog string
	msg  string
}

func (e *usageError) Error() string { return e.prog + ": error: " + e.msg }

// run dispatches argv and returns the exit code.
func (a *app) run(ctx context.Context, argv []string) int {
	if len(argv) == 0 {
		a.printHelp(a.stdout)
		return 2
	}
	switch argv[0] {
	case "-h", "--help":
		a.printHelp(a.stdout)
		return 0
	case "--version":
		a.println("hotseat " + version.Version)
		return 0
	case "bridge":
		// Hidden verification mode kept for scripts/compare_bridge.py.
		return a.bridge(ctx, argv[1:])
	}
	cmd := lookup(argv[0])
	if cmd == nil {
		if strings.HasPrefix(argv[0], "-") {
			a.usageFailure(topUsage(), "hotseat", fmt.Sprintf("unrecognized arguments: %s", argv[0]))
		} else {
			a.usageFailure(topUsage(), "hotseat", fmt.Sprintf("argument command: invalid choice: '%s' (choose from %s)",
				argv[0], "'"+strings.Join(commandNames(), "', '")+"'"))
		}
		return 2
	}
	code, err := cmd.run(a, cmd, ctx, argv[1:])
	if err == nil {
		return code
	}
	var usage *usageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &usage):
		a.usageFailure(cmdUsage(cmd), usage.prog, usage.msg)
		return 2
	}
	// Anything else is the Python's `except BackendError` in main(): say why, exit 1.
	a.errorln(a.paint("✗ "+err.Error(), red))
	return 1
}

func (a *app) usageFailure(usage, prog, msg string) {
	fmt.Fprint(a.stderr, usage)
	fmt.Fprintf(a.stderr, "%s: error: %s\n", prog, msg)
}

func topUsage() string {
	return fmt.Sprintf("usage: hotseat [-h] [--version]\n               {%s} ...\n", strings.Join(commandNames(), ","))
}

func cmdUsage(cmd *command) string {
	parts := []string{"[-h]"}
	if cmd.json {
		parts = append(parts, "[--json]")
	}
	parts = append(parts, extraUsage[cmd.name]...)
	if cmd.positional != "" {
		parts = append(parts, cmd.positional)
	}
	return fmt.Sprintf("usage: hotseat %s %s\n", cmd.name, strings.Join(parts, " "))
}

// extraUsage lists each command's optional flags for its usage line.
var extraUsage = map[string][]string{
	"tui":      {"[--demo]", "[--demo-file FILE]", "[--render]", "[--width N]", "[--height N]", "[--snapshot]"},
	"refresh":  {"[--force]"},
	"switch":   {"[--yes]"},
	"models":   {"[--days DAYS]"},
	"inspect":  {"[--full]"},
	"resume":   {"[--go]", "[--kind KIND]", "[--cause {usage_limit,queue_paused}]", "[--days DAYS]"},
	"reboot":   {"[--message MESSAGE]", "[--cwd CWD]", "[--yes]"},
	"codex":    {"[--no-quota]"},
	"clarp":    {"[--live]", "[--backend BACKEND]"},
	"sessions": {"[--limit LIMIT]"},
}

func (a *app) printHelp(w io.Writer) {
	fmt.Fprint(w, topUsage())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Inspect and use the Claude Code and Codex accounts on this machine.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "positional arguments:")
	fmt.Fprintf(w, "  {%s}\n", strings.Join(commandNames(), ","))
	for _, c := range commands {
		fmt.Fprintf(w, "    %-19s %s\n", c.name, c.help)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "options:")
	fmt.Fprintln(w, "  -h, --help            show this help message and exit")
	fmt.Fprintln(w, "  --version             show program's version number and exit")
}

// --- argument parsing -------------------------------------------------------

// flags is a FlagSet for one subcommand. Its usage text goes to stdout on -h
// (argparse behaviour); parse errors are reported as usageErrors.
func (a *app) flags(cmd *command) *flag.FlagSet {
	fs := flag.NewFlagSet("hotseat "+cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parse reads flags anywhere in args, the way argparse does, and returns the
// positionals. It needs at least min and at most max positionals (max < 0 means
// any number); names label the missing ones in the error.
func (a *app) parse(cmd *command, fs *flag.FlagSet, args []string, min, max int, names ...string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				a.printCommandHelp(cmd, fs)
				return nil, flag.ErrHelp
			}
			return nil, &usageError{"hotseat " + cmd.name, err.Error()}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	if len(positional) < min {
		missing := names
		if len(positional) < len(names) {
			missing = names[len(positional):]
		}
		return nil, &usageError{"hotseat " + cmd.name,
			"the following arguments are required: " + strings.Join(missing, ", ")}
	}
	if max >= 0 && len(positional) > max {
		return nil, &usageError{"hotseat " + cmd.name,
			"unrecognized arguments: " + strings.Join(positional[max:], " ")}
	}
	return positional, nil
}

func (a *app) printCommandHelp(cmd *command, fs *flag.FlagSet) {
	fmt.Fprint(a.stdout, cmdUsage(cmd))
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "options:")
	fmt.Fprintln(a.stdout, "  -h, --help  show this help message and exit")
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(a.stdout, "  --%-10s %s\n", f.Name, f.Usage)
	})
}

// jsonFlag registers --json when the command takes it.
func jsonFlag(cmd *command, fs *flag.FlagSet) *bool {
	if !cmd.json {
		return new(bool)
	}
	return fs.Bool("json", false, "Machine-readable output")
}

// defaultAlias is the active account's alias, or "" when none is.
func defaultAlias(accounts []collect.AccountView) string {
	for _, account := range accounts {
		if account.IsActive {
			return account.Alias
		}
	}
	return ""
}

// nullable maps "" to JSON null, the way the Python emitted None.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// accountQuotas is the readiness view of a snapshot's accounts.
func accountQuotas(accounts []collect.AccountView) []work.AccountQuota {
	out := make([]work.AccountQuota, 0, len(accounts))
	for _, view := range accounts {
		item := work.AccountQuota{Alias: view.Alias}
		if view.Usage != nil {
			item.Usage = &work.Quota{Limited: view.Usage.Limited, BlockedModels: view.Usage.BlockedModels}
		}
		out = append(out, item)
	}
	return out
}

// confirm asks a yes/no question on stdin. Only reached when stdin is a terminal.
func (a *app) confirm(prompt string) bool {
	fmt.Fprint(a.stdout, prompt)
	var line strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := a.stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line.WriteByte(buf[0])
		}
		if err != nil {
			break
		}
	}
	answer := strings.ToLower(strings.TrimSpace(line.String()))
	return answer == "y" || answer == "yes"
}
