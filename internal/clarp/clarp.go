// Package clarp reads Clarp's agent registry and state database, and prompts
// stopped agents through clarp-admin.
//
// Clarp keeps a registry of named agent sessions and starts a backend process for
// one only when it has work. So a session is usually a definition rather than a
// running process, and "is it live" is a separate question from "does it exist".
//
// Two things matter here that nothing else reports:
//
//   - Agents on the `claude` backend draw on the same account quota as everything
//     else, but they are invisible in a normal process listing until they are
//     busy.
//   - A running agent's account is decided by the config directory in its
//     environment, so a pinned agent can be attributed exactly.
//
// The integration is compiled in and activates only when the state database
// exists (Available). Everything here is read-only except Service.Continue, which
// is explicit. The shared native-transcript logic (stopped native sessions,
// readiness, priority, model families) lives in internal/work and is reused, not
// duplicated (design decision 12).
package clarp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Maxteabag/hotseat/internal/work"
)

// Admin is the Clarp administration command.
const Admin = "clarp-admin"

// ClaudeBackend is the backend name of agents that spend a Claude account.
const ClaudeBackend = "claude"

// adminTimeout bounds `clarp-admin sessions` and `ps`.
const adminTimeout = 30 * time.Second

// sessionInPrompt is the session slug an agent process carries in its own system
// prompt.
var sessionInPrompt = regexp.MustCompile(`session id for this agent is .([A-Za-z0-9._-]+)`)

// agentBinaries are the only executables that count as agent processes.
var agentBinaries = map[string]bool{"claude": true, "codex": true, "node": true}

// ClarpError means Clarp is not installed here, or its session registry could
// not be read.
type ClarpError struct{ Msg string }

func (e *ClarpError) Error() string { return e.Msg }

func clarpErr(format string, args ...any) error {
	return &ClarpError{Msg: fmt.Sprintf(format, args...)}
}

// RunOutput runs a prepared command and returns its standard output, like
// (*exec.Cmd).Output: a non-zero exit is an *exec.ExitError carrying Stderr.
// Tests inject one to avoid spawning anything.
type RunOutput func(*exec.Cmd) ([]byte, error)

// Service reads Clarp state. The zero value reads nothing; use New or set the
// paths explicitly.
type Service struct {
	// StatePath is Clarp's state database, read-only.
	StatePath string
	// ProjectsDir holds Claude Code's native transcripts, merged into Stopped.
	ProjectsDir string
	// ProcRoot is /proc, or a fake tree in tests.
	ProcRoot string
	// Run executes clarp-admin and ps; nil means the real commands.
	Run RunOutput
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// Test seams; nil means the real thing.
	lookPath func(string) (string, error)
	selfPID  func() int
}

// DefaultStatePath is ~/.local/share/clarp/state.sqlite.
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "clarp", "state.sqlite")
}

// Available says whether Clarp keeps state on this machine, which is what turns
// the integration on.
func Available() bool {
	return stateExists(DefaultStatePath())
}

func stateExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// New is a Service against the real machine.
func New() *Service {
	return &Service{
		StatePath:   DefaultStatePath(),
		ProjectsDir: work.DefaultProjectsDir(),
		ProcRoot:    "/proc",
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) procRoot() string {
	if s.ProcRoot == "" {
		return "/proc"
	}
	return s.ProcRoot
}

func (s *Service) run(cmd *exec.Cmd) ([]byte, error) {
	if s.Run != nil {
		return s.Run(cmd)
	}
	return cmd.Output()
}

// AdminInstalled says whether clarp-admin is on PATH. An injected runner counts
// as installed, since nothing real is executed.
func (s *Service) AdminInstalled() bool {
	if s.lookPath != nil {
		_, err := s.lookPath(Admin)
		return err == nil
	}
	if s.Run != nil {
		return true
	}
	_, err := exec.LookPath(Admin)
	return err == nil
}

// SessionRow is one registry entry from `clarp-admin sessions`. Fields the
// registry leaves out arrive as nil.
type SessionRow struct {
	Session *string `json:"session"`
	Persona *string `json:"persona"`
	Backend *string `json:"backend"`
	CWD     *string `json:"cwd"`
}

// Sessions is the registry: every defined agent, running or not.
func (s *Service) Sessions(ctx context.Context) ([]SessionRow, error) {
	if !s.AdminInstalled() {
		return nil, clarpErr("%s is not on PATH, so there is no Clarp installation to read.", Admin)
	}
	ctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	stdout, err := s.run(exec.CommandContext(ctx, Admin, "sessions"))
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return nil, &ClarpError{Msg: stderrMessage(exit, "clarp-admin failed")}
		}
		return nil, clarpErr("could not run %s: %v", Admin, err)
	}
	if strings.TrimSpace(string(stdout)) == "" {
		stdout = []byte("[]")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, clarpErr("clarp-admin returned output that was not JSON")
	}
	rows := make([]SessionRow, 0, len(raw))
	for _, element := range raw {
		// Only objects are registry rows; anything else is skipped.
		if len(element) == 0 || element[0] != '{' {
			continue
		}
		var row SessionRow
		if err := json.Unmarshal(element, &row); err != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// stderrMessage is the first 200 characters of a failed command's stderr, or
// the fallback when it said nothing.
func stderrMessage(exit *exec.ExitError, fallback string) string {
	text := strings.TrimSpace(string(exit.Stderr))
	if text == "" {
		text = fallback
	}
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200])
	}
	return text
}

// LiveAgent is a running agent process and the account it is spending.
type LiveAgent struct {
	PID       int
	ConfigDir string
	Account   string
}

// configDirOf is the CLAUDE_CONFIG_DIR of a process, from its environment.
func (s *Service) configDirOf(pid string) string {
	raw, err := os.ReadFile(filepath.Join(s.procRoot(), pid, "environ"))
	if err != nil {
		return ""
	}
	for _, entry := range strings.Split(strings.ToValidUTF8(string(raw), "�"), "\x00") {
		if value, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
			return value
		}
	}
	return ""
}

func (s *Service) pid() int {
	if s.selfPID != nil {
		return s.selfPID()
	}
	return os.Getpid()
}

// LiveAgents lists running agent processes, keyed by the session slug they
// carry.
//
// Only real agent binaries count. A shell whose command line merely mentions an
// agent is not one, and neither is this process. An unreadable process list
// yields nothing rather than an error.
func (s *Service) LiveAgents(ctx context.Context) map[string]LiveAgent {
	ctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	listing, err := s.run(exec.CommandContext(ctx, "ps", "-eo", "pid=,args="))
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || ctx.Err() != nil {
			return map[string]LiveAgent{}
		}
		// Python read stdout regardless of the exit status.
	}

	mine := strconv.Itoa(s.pid())
	found := map[string]LiveAgent{}
	for _, line := range strings.Split(string(listing), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid := fields[0]
		args := strings.TrimLeftFunc(strings.TrimPrefix(line, pid), unicode.IsSpace)
		if pid == mine {
			continue
		}
		if !agentBinaries[filepath.Base(fields[1])] {
			continue
		}
		match := sessionInPrompt.FindStringSubmatch(args)
		if match == nil {
			continue
		}
		number, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		configDir := s.configDirOf(pid)
		agent := LiveAgent{PID: number, ConfigDir: configDir}
		if configDir != "" {
			agent.Account = filepath.Base(configDir)
		}
		found[match[1]] = agent
	}
	return found
}

// Agent is one registry entry with its liveness and account attribution.
type Agent struct {
	Session string  `json:"session"`
	Persona *string `json:"persona"`
	Backend *string `json:"backend"`
	CWD     *string `json:"cwd"`
	Live    bool    `json:"live"`
	PID     *int    `json:"pid"`
	// Account is what a running Claude agent is spending; nil when idle, since
	// an idle agent spends nothing.
	Account *string `json:"account"`
	// WouldUse is the default account an idle Claude agent would start on.
	WouldUse *string `json:"would_use"`
	Pinned   bool    `json:"pinned"`
}

// Overview is the registry and liveness together, with the account each live
// agent is using.
type Overview struct {
	Agents        []Agent        `json:"agents"`
	Total         int            `json:"total"`
	Live          int            `json:"live"`
	ByBackend     map[string]int `json:"by_backend"`
	ClaudeBacked  int            `json:"claude_backed"`
	LiveByAccount map[string]int `json:"live_by_account"`
}

// Overview reads the registry and the process table. defaultAlias is the
// machine's active account, which idle Claude agents would start on.
func (s *Service) Overview(ctx context.Context, defaultAlias string) (*Overview, error) {
	rows, err := s.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	return buildOverview(rows, s.LiveAgents(ctx), defaultAlias), nil
}

func buildOverview(rows []SessionRow, running map[string]LiveAgent, defaultAlias string) *Overview {
	agents := make([]Agent, 0, len(rows))
	for _, row := range rows {
		slug := deref(row.Session)
		live, isLive := running[slug]
		isClaude := deref(row.Backend) == ClaudeBackend
		agent := Agent{
			Session: slug,
			Persona: row.Persona,
			Backend: row.Backend,
			CWD:     row.CWD,
			Live:    isLive,
			Pinned:  isLive && live.ConfigDir != "",
		}
		if isLive {
			pid := live.PID
			agent.PID = &pid
		}
		// Only a running agent is actually spending an account. An idle one
		// would use the machine default, which is a different claim and is
		// reported separately rather than dressed up as current usage.
		if isClaude && isLive && live.Account != "" {
			account := live.Account
			agent.Account = &account
		}
		if isClaude && !isLive && defaultAlias != "" {
			alias := defaultAlias
			agent.WouldUse = &alias
		}
		agents = append(agents, agent)
	}

	byBackend := map[string]int{}
	for _, agent := range agents {
		key := deref(agent.Backend)
		if key == "" {
			key = "unknown"
		}
		byBackend[key]++
	}

	claudeBacked := 0
	byAccount := map[string]int{}
	liveCount := 0
	for _, agent := range agents {
		if agent.Live {
			liveCount++
		}
		if deref(agent.Backend) != ClaudeBackend {
			continue
		}
		claudeBacked++
		if agent.Live {
			// A live agent with no config directory is on the shared default.
			key := deref(agent.Account)
			if key == "" {
				key = defaultAlias
			}
			if key == "" {
				key = "default"
			}
			byAccount[key]++
		}
	}

	slices.SortStableFunc(agents, func(a, b Agent) int {
		if a.Live != b.Live {
			if a.Live {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Session, b.Session)
	})
	return &Overview{
		Agents:        agents,
		Total:         len(agents),
		Live:          liveCount,
		ByBackend:     byBackend,
		ClaudeBacked:  claudeBacked,
		LiveByAccount: byAccount,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Report is what the `clarp` subcommand shows: the whole overview, which is what
// --json emits, and the agents left after the --live and --backend filters,
// which is what the table shows.
type Report struct {
	Overview *Overview `json:"overview"`
	Agents   []Agent   `json:"agents"`
	// Filtered says whether --live or --backend narrowed the table, which
	// decides between "No Clarp agents match." and "No Clarp agents defined.".
	Filtered bool `json:"filtered"`
}

// Report builds the `clarp` subcommand's data. liveOnly and backend are the
// --live and --backend filters; an empty backend does not filter.
func (s *Service) Report(ctx context.Context, defaultAlias string, liveOnly bool, backend string) (*Report, error) {
	overview, err := s.Overview(ctx, defaultAlias)
	if err != nil {
		return nil, err
	}
	return &Report{
		Overview: overview,
		Agents:   FilterAgents(overview.Agents, liveOnly, backend),
		Filtered: liveOnly || backend != "",
	}, nil
}

// FilterAgents applies the `clarp` subcommand's --live and --backend filters.
func FilterAgents(agents []Agent, liveOnly bool, backend string) []Agent {
	kept := make([]Agent, 0, len(agents))
	for _, agent := range agents {
		if liveOnly && !agent.Live {
			continue
		}
		if backend != "" && deref(agent.Backend) != backend {
			continue
		}
		kept = append(kept, agent)
	}
	return kept
}
