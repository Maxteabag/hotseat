package clarp

// Clarp agent discovery and account attribution. Ported from
// plugins/clarp/tests/test_clarp.py. No test runs clarp-admin or ps.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var registry = []SessionRow{
	{Session: str("adam"), Persona: str("Adam"), Backend: str("claude"), CWD: str("/home/u")},
	{Session: str("bella"), Persona: str("Bella"), Backend: str("claude"), CWD: str("/home/u")},
	{Session: str("cleo"), Persona: str("Cleo"), Backend: str("codex"), CWD: str("/home/u")},
}

func str(s string) *string { return &s }

// completed is a runner that behaves like a finished subprocess: stdout on
// success, an *exec.ExitError carrying stderr on a non-zero exit.
func completed(stdout string, code int, stderr string) RunOutput {
	return func(*exec.Cmd) ([]byte, error) {
		if code != 0 {
			return []byte(stdout), &exec.ExitError{Stderr: []byte(stderr)}
		}
		return []byte(stdout), nil
	}
}

// recording is a runner that records every argv and answers with the given
// runner.
func recording(calls *[][]string, answer RunOutput) RunOutput {
	return func(cmd *exec.Cmd) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), cmd.Args...))
		return answer(cmd)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// --- sessions ---------------------------------------------------------------

func TestMissingClarpIsAClearMessage(t *testing.T) {
	s := &Service{lookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	_, err := s.Sessions(context.Background())
	var clarpError *ClarpError
	if !errors.As(err, &clarpError) {
		t.Fatalf("want ClarpError, got %v", err)
	}
	if !strings.Contains(err.Error(), "clarp-admin") {
		t.Fatalf("message should name clarp-admin: %q", err)
	}
}

func TestRegistryIsParsed(t *testing.T) {
	s := &Service{Run: completed(mustJSON(t, registry), 0, "")}
	rows, err := s.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, row := range rows {
		sessions = append(sessions, *row.Session)
	}
	if want := []string{"adam", "bella", "cleo"}; !reflect.DeepEqual(sessions, want) {
		t.Fatalf("got %v, want %v", sessions, want)
	}
}

func TestNonJSONOutputIsAnErrorNotACrash(t *testing.T) {
	s := &Service{Run: completed("not json", 0, "")}
	_, err := s.Sessions(context.Background())
	var clarpError *ClarpError
	if !errors.As(err, &clarpError) {
		t.Fatalf("want ClarpError, got %v", err)
	}
}

func TestEmptyOutputIsAnEmptyRegistry(t *testing.T) {
	s := &Service{Run: completed("", 0, "")}
	rows, err := s.Sessions(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v, %v", rows, err)
	}
}

func TestNonObjectRowsAreSkipped(t *testing.T) {
	s := &Service{Run: completed(`[1, "x", {"session": "adam"}]`, 0, "")}
	rows, err := s.Sessions(context.Background())
	if err != nil || len(rows) != 1 || *rows[0].Session != "adam" {
		t.Fatalf("got %v, %v", rows, err)
	}
}

func TestFailureExitIsReported(t *testing.T) {
	s := &Service{Run: completed("", 1, "database locked")}
	_, err := s.Sessions(context.Background())
	var clarpError *ClarpError
	if !errors.As(err, &clarpError) {
		t.Fatalf("want ClarpError, got %v", err)
	}
	if !strings.Contains(err.Error(), "database locked") {
		t.Fatalf("stderr should reach the message: %q", err)
	}
}

func TestASilentFailureHasAFallbackMessage(t *testing.T) {
	s := &Service{Run: completed("", 1, "")}
	_, err := s.Sessions(context.Background())
	if err == nil || err.Error() != "clarp-admin failed" {
		t.Fatalf("got %v", err)
	}
}

func TestARunnerFailureIsACouldNotRunError(t *testing.T) {
	s := &Service{Run: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("nope") }}
	_, err := s.Sessions(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "could not run clarp-admin: nope") {
		t.Fatalf("got %v", err)
	}
}

// --- overview ---------------------------------------------------------------

func overviewOf(live map[string]LiveAgent, defaultAlias string) *Overview {
	return buildOverview(registry, live, defaultAlias)
}

func agentNamed(t *testing.T, out *Overview, session string) Agent {
	t.Helper()
	for _, agent := range out.Agents {
		if agent.Session == session {
			return agent
		}
	}
	t.Fatalf("no agent %q in %+v", session, out.Agents)
	return Agent{}
}

func TestCountsByBackend(t *testing.T) {
	out := overviewOf(nil, "work")
	if want := map[string]int{"claude": 2, "codex": 1}; !reflect.DeepEqual(out.ByBackend, want) {
		t.Fatalf("got %v, want %v", out.ByBackend, want)
	}
	if out.ClaudeBacked != 2 {
		t.Fatalf("claude_backed = %d", out.ClaudeBacked)
	}
}

func TestIdleAgentIsNotCreditedWithSpendingAnAccount(t *testing.T) {
	// An idle agent spends nothing; saying otherwise would misattribute quota.
	agent := agentNamed(t, overviewOf(nil, "work"), "adam")
	if agent.Live {
		t.Fatal("adam should be idle")
	}
	if agent.Account != nil {
		t.Fatalf("account = %q, want nil", *agent.Account)
	}
	if agent.WouldUse == nil || *agent.WouldUse != "work" {
		t.Fatalf("would_use = %v, want work", agent.WouldUse)
	}
}

func TestLivePinnedAgentReportsItsOwnAccount(t *testing.T) {
	live := map[string]LiveAgent{"adam": {PID: 42, ConfigDir: "/home/u/.claude-accounts/other", Account: "other"}}
	agent := agentNamed(t, overviewOf(live, "work"), "adam")
	if !agent.Live || agent.Account == nil || *agent.Account != "other" || !agent.Pinned {
		t.Fatalf("got %+v", agent)
	}
	if agent.PID == nil || *agent.PID != 42 {
		t.Fatalf("pid = %v", agent.PID)
	}
	if agent.WouldUse != nil {
		t.Fatalf("a live agent has no would_use, got %q", *agent.WouldUse)
	}
}

func TestLiveUnpinnedAgentFallsToTheDefaultAccount(t *testing.T) {
	live := map[string]LiveAgent{"adam": {PID: 42}}
	out := overviewOf(live, "work")
	if want := map[string]int{"work": 1}; !reflect.DeepEqual(out.LiveByAccount, want) {
		t.Fatalf("got %v, want %v", out.LiveByAccount, want)
	}
	if agent := agentNamed(t, out, "adam"); agent.Pinned || agent.Account != nil {
		t.Fatalf("got %+v", agent)
	}
}

func TestLiveUnpinnedAgentWithNoDefaultCountsAsDefault(t *testing.T) {
	out := overviewOf(map[string]LiveAgent{"adam": {PID: 42}}, "")
	if want := map[string]int{"default": 1}; !reflect.DeepEqual(out.LiveByAccount, want) {
		t.Fatalf("got %v, want %v", out.LiveByAccount, want)
	}
}

func TestCodexAgentsAreNeverAttributedToAClaudeAccount(t *testing.T) {
	agent := agentNamed(t, overviewOf(nil, "work"), "cleo")
	if agent.Account != nil || agent.WouldUse != nil {
		t.Fatalf("got %+v", agent)
	}
	live := map[string]LiveAgent{"cleo": {PID: 1, ConfigDir: "/x/acct", Account: "acct"}}
	out := overviewOf(live, "work")
	if agent := agentNamed(t, out, "cleo"); agent.Account != nil {
		t.Fatalf("a live codex agent must not be attributed: %+v", agent)
	}
	if len(out.LiveByAccount) != 0 {
		t.Fatalf("live_by_account = %v", out.LiveByAccount)
	}
}

func TestLiveAgentsSortFirst(t *testing.T) {
	live := map[string]LiveAgent{"cleo": {PID: 1}}
	out := overviewOf(live, "work")
	if out.Agents[0].Session != "cleo" {
		t.Fatalf("got %q first", out.Agents[0].Session)
	}
	if out.Agents[1].Session != "adam" || out.Agents[2].Session != "bella" {
		t.Fatalf("idle agents should follow by session: %+v", out.Agents)
	}
}

func TestLiveCount(t *testing.T) {
	out := overviewOf(map[string]LiveAgent{"adam": {PID: 1}}, "work")
	if out.Live != 1 || out.Total != 3 {
		t.Fatalf("live=%d total=%d", out.Live, out.Total)
	}
}

func TestAnUnknownBackendIsCountedAsUnknown(t *testing.T) {
	out := buildOverview([]SessionRow{{Session: str("x")}}, nil, "work")
	if want := map[string]int{"unknown": 1}; !reflect.DeepEqual(out.ByBackend, want) {
		t.Fatalf("got %v", out.ByBackend)
	}
	if agent := out.Agents[0]; agent.Persona != nil || agent.WouldUse != nil {
		t.Fatalf("got %+v", agent)
	}
}

func TestOverviewJSONShape(t *testing.T) {
	out := overviewOf(nil, "work")
	encoded := mustJSON(t, out.Agents[0])
	want := `{"session":"adam","persona":"Adam","backend":"claude","cwd":"/home/u","live":false,"pid":null,"account":null,"would_use":"work","pinned":false}`
	if encoded != want {
		t.Fatalf("got %s\nwant %s", encoded, want)
	}
	for _, key := range []string{`"agents"`, `"total"`, `"live"`, `"by_backend"`, `"claude_backed"`, `"live_by_account"`} {
		if !strings.Contains(mustJSON(t, out), key) {
			t.Fatalf("overview JSON lacks %s", key)
		}
	}
}

func TestOverviewReadsRegistryAndProcesses(t *testing.T) {
	s := &Service{
		ProcRoot: t.TempDir(),
		selfPID:  func() int { return 1 },
		Run: func(cmd *exec.Cmd) ([]byte, error) {
			switch filepath.Base(cmd.Args[0]) {
			case Admin:
				return []byte(mustJSON(t, registry)), nil
			case "ps":
				return []byte("  222 /usr/bin/claude --append-system-prompt session id for this agent is `adam`\n"), nil
			}
			t.Fatalf("unexpected command %v", cmd.Args)
			return nil, nil
		},
	}
	out, err := s.Overview(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if out.Live != 1 || out.Agents[0].Session != "adam" || !out.Agents[0].Live {
		t.Fatalf("got %+v", out)
	}
}

// --- live agents ------------------------------------------------------------

func TestAShellThatMerelyMentionsAnAgentIsNotOne(t *testing.T) {
	listing := "  111 /bin/bash -c echo session id for this agent is `fake`\n" +
		"  222 /usr/bin/claude --append-system-prompt " +
		"the app session id for this agent is `real` --resume x\n"
	s := &Service{Run: completed(listing, 0, ""), ProcRoot: t.TempDir()}
	found := s.LiveAgents(context.Background())
	if len(found) != 1 {
		t.Fatalf("got %v", found)
	}
	if agent, ok := found["real"]; !ok || agent.PID != 222 || agent.ConfigDir != "" || agent.Account != "" {
		t.Fatalf("got %+v", found)
	}
}

func TestAnUnreadableProcessListYieldsNothingRatherThanRaising(t *testing.T) {
	s := &Service{Run: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("nope") }}
	if found := s.LiveAgents(context.Background()); len(found) != 0 {
		t.Fatalf("got %v", found)
	}
}

func TestThePinnedAccountComesFromTheProcessEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "222"), 0o700); err != nil {
		t.Fatal(err)
	}
	environ := "HOME=/home/u\x00CLAUDE_CONFIG_DIR=/home/u/.claude-accounts/other\x00TERM=xterm\x00"
	if err := os.WriteFile(filepath.Join(root, "222", "environ"), []byte(environ), 0o600); err != nil {
		t.Fatal(err)
	}
	listing := "222 node /opt/claude/cli.js --append-system-prompt session id for this agent is `adam`\n" +
		"333 codex exec session id for this agent is `bella`\n"
	s := &Service{Run: completed(listing, 0, ""), ProcRoot: root}
	found := s.LiveAgents(context.Background())
	if agent := found["adam"]; agent.ConfigDir != "/home/u/.claude-accounts/other" || agent.Account != "other" {
		t.Fatalf("got %+v", agent)
	}
	if agent := found["bella"]; agent.PID != 333 || agent.ConfigDir != "" || agent.Account != "" {
		t.Fatalf("an agent without an environ file is unpinned: %+v", agent)
	}
}

func TestThisProcessIsNotAnAgent(t *testing.T) {
	listing := "  777 /usr/bin/claude session id for this agent is `me`\n"
	s := &Service{Run: completed(listing, 0, ""), ProcRoot: t.TempDir(), selfPID: func() int { return 777 }}
	if found := s.LiveAgents(context.Background()); len(found) != 0 {
		t.Fatalf("got %v", found)
	}
}

func TestLiveAgentsInvokesPS(t *testing.T) {
	var calls [][]string
	s := &Service{Run: recording(&calls, completed("", 0, ""))}
	s.LiveAgents(context.Background())
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"ps", "-eo", "pid=,args="}) {
		t.Fatalf("got %v", calls)
	}
}

// --- report -----------------------------------------------------------------

func TestReportFiltersTheTableButNotTheOverview(t *testing.T) {
	s := &Service{
		ProcRoot: t.TempDir(),
		Run: func(cmd *exec.Cmd) ([]byte, error) {
			if cmd.Args[0] == "ps" {
				return []byte("  5 /usr/bin/codex session id for this agent is `cleo`\n"), nil
			}
			return []byte(mustJSON(t, registry)), nil
		},
	}
	report, err := s.Report(context.Background(), "work", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Filtered || len(report.Agents) != 1 || report.Agents[0].Session != "cleo" {
		t.Fatalf("got %+v", report)
	}
	if report.Overview.Total != 3 {
		t.Fatalf("the overview stays whole: %+v", report.Overview)
	}
	report, err = s.Report(context.Background(), "work", false, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Filtered || len(report.Agents) != 2 {
		t.Fatalf("got %+v", report.Agents)
	}
	report, err = s.Report(context.Background(), "work", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Filtered || len(report.Agents) != 3 {
		t.Fatalf("got %+v", report.Agents)
	}
}

func TestReportPropagatesClarpErrors(t *testing.T) {
	s := &Service{Run: completed("", 1, "down")}
	_, err := s.Report(context.Background(), "work", false, "")
	var clarpError *ClarpError
	if !errors.As(err, &clarpError) {
		t.Fatalf("got %v", err)
	}
}

// --- availability -------------------------------------------------------------

func TestDefaultsPointAtTheRealMachine(t *testing.T) {
	s := New()
	if !strings.HasSuffix(s.StatePath, filepath.Join(".local", "share", "clarp", "state.sqlite")) {
		t.Fatalf("state path = %q", s.StatePath)
	}
	if s.ProcRoot != "/proc" || s.ProjectsDir == "" {
		t.Fatalf("got %+v", s)
	}
}

func TestStateExistsDecidesAvailability(t *testing.T) {
	dir := t.TempDir()
	if stateExists(filepath.Join(dir, "absent.sqlite")) {
		t.Fatal("absent file reported present")
	}
	path := filepath.Join(dir, "state.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !stateExists(path) {
		t.Fatal("present file reported absent")
	}
}

func TestAdminInstalledHonoursTheSeams(t *testing.T) {
	if !(&Service{Run: completed("", 0, "")}).AdminInstalled() {
		t.Fatal("an injected runner counts as installed")
	}
	if (&Service{lookPath: func(string) (string, error) { return "", exec.ErrNotFound }}).AdminInstalled() {
		t.Fatal("a failed lookup means not installed")
	}
	if !(&Service{lookPath: func(string) (string, error) { return "/bin/clarp-admin", nil }}).AdminInstalled() {
		t.Fatal("a successful lookup means installed")
	}
}
