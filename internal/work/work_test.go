package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- fakes -------------------------------------------------------------------

type fakeCodex struct {
	rows        []CodexRow
	recentErr   error
	lockHolders func(threadID string) []int
	items       []string
	itemsErr    error
}

func (f *fakeCodex) Recent(context.Context, int) ([]CodexRow, error) { return f.rows, f.recentErr }
func (f *fakeCodex) LockHolders(threadID string) ([]int, error) {
	if f.lockHolders == nil {
		return nil, nil
	}
	return f.lockHolders(threadID), nil
}
func (f *fakeCodex) ThreadItems(string, int) ([]string, error) { return f.items, f.itemsErr }

type fakeLauncher struct {
	calls [][]string
	cwds  []string
	err   error
}

func (f *fakeLauncher) LaunchCommand(argv []string, cwd string) error {
	f.calls = append(f.calls, argv)
	f.cwds = append(f.cwds, cwd)
	return f.err
}

// fakeProcess writes /proc/<pid>/{cmdline,stat,status} into a fake proc root.
func fakeProcess(t *testing.T, root string, pid int, argv []string, start, state string, ppid int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(argv, "\x00") + "\x00"
	fields := []string{state}
	for i := 0; i < 18; i++ {
		fields = append(fields, strconv.Itoa(i))
	}
	fields = append(fields, start, "6406144", "961")
	stat := fmt.Sprintf("%d (%s) %s\n", pid, "comm) tricky", strings.Join(fields, " "))
	status := fmt.Sprintf("Name:\tx\nState:\t%s\nPPid:\t%d\n", state, ppid)
	for name, body := range map[string]string{"cmdline": cmdline, "stat": stat, "status": status} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureItem(cwd string) Item {
	item := Item{ID: "fixture-session", Provider: "codex", State: "failed", Updated: 1, CWD: cwd, CanResume: true, CanReboot: false, Reason: "", updatedInteger: true}
	item.Revision = Revision(item, nil)
	return item
}

func resolveTo(item Item) func(context.Context, string, string) (Item, error) {
	return func(context.Context, string, string) (Item, error) { return item, nil }
}

func wantWorkError(t *testing.T, err error, fragment string) {
	t.Helper()
	var we *WorkError
	if !errors.As(err, &we) {
		t.Fatalf("expected WorkError containing %q, got %v", fragment, err)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("expected %q in %q", fragment, err.Error())
	}
}

// --- process identity ---------------------------------------------------------

func TestProcessIdentityFromAFakeProcTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	fakeProcess(t, root, 100, []string{"claude", "--resume", "abc"}, "1000", "S", 1)
	fakeProcess(t, root, 101, []string{"/usr/bin/node", "/lib/node_modules/@anthropic-ai/claude-code/cli.js", "--session-id", "def"}, "1001", "S", 1)
	fakeProcess(t, root, 102, []string{"node", "/opt/other/cli.js", "--resume", "ghi"}, "1002", "S", 1)
	fakeProcess(t, root, 103, []string{"/home/u/.local/bin/codex", "app-server"}, "1003", "S", 1)
	fakeProcess(t, root, 104, []string{"claude", "-p", "--input-format", "stream-json", "-r", "jkl"}, "1004", "S", 1)
	fakeProcess(t, root, 105, []string{"vim", "claude"}, "1005", "S", 1)
	fakeProcess(t, root, 106, []string{"nodejs", "/x/@anthropic-ai/claude-code/claude.js"}, "1006", "R", 1)
	s := &Service{ProcRoot: root}

	direct := s.ProcessIdentity(100, "claude")
	if direct == nil || direct.Start != "1000" || direct.Shared || strings.Join(direct.Argv, " ") != "claude --resume abc" || direct.PID != 100 {
		t.Fatalf("direct claude %+v", direct)
	}
	if wrapped := s.ProcessIdentity(101, "claude"); wrapped == nil || wrapped.Start != "1001" || wrapped.Shared {
		t.Fatalf("node wrapper %+v", wrapped)
	}
	if other := s.ProcessIdentity(102, "claude"); other != nil {
		t.Fatalf("a cli.js outside @anthropic-ai/claude-code is not claude: %+v", other)
	}
	if server := s.ProcessIdentity(103, "codex"); server == nil || !server.Shared {
		t.Fatalf("app-server must be shared: %+v", server)
	}
	if s.ProcessIdentity(103, "claude") != nil {
		t.Fatal("codex is not claude")
	}
	if stream := s.ProcessIdentity(104, "claude"); stream == nil || !stream.Shared {
		t.Fatalf("stream-json bridge must be shared: %+v", stream)
	}
	if s.ProcessIdentity(105, "claude") != nil {
		t.Fatal("no substring matching on later arguments")
	}
	if legacy := s.ProcessIdentity(106, "claude"); legacy == nil || legacy.Start != "1006" {
		t.Fatalf("nodejs + claude.js %+v", legacy)
	}
	if s.ProcessIdentity(999, "claude") != nil {
		t.Fatal("a missing pid has no identity")
	}

	processes := s.ClaudeProcesses()
	var pids []int
	for _, p := range processes {
		pids = append(pids, p.PID)
	}
	if fmt.Sprint(pids) != "[100 101 104 106]" {
		t.Fatalf("claude processes %v", pids)
	}
	holders := ClaudeHolders("abc", processes)
	if len(holders) != 1 || holders[0].PID != 100 {
		t.Fatalf("holders of abc %+v", holders)
	}
	if got := ClaudeHolders("jkl", processes); len(got) != 1 || got[0].PID != 104 {
		t.Fatalf("-r holder %+v", got)
	}
	if got := ClaudeHolders("def", processes); len(got) != 1 || got[0].PID != 101 {
		t.Fatalf("--session-id holder %+v", got)
	}
	if got := ClaudeHolders("ab", processes); len(got) != 0 {
		t.Fatalf("prefixes are not matches: %+v", got)
	}
	fakeProcess(t, root, 107, []string{"claude", "--resume"}, "1007", "S", 1)
	if got := ClaudeHolders("", s.ClaudeProcesses()); len(got) != 0 {
		t.Fatalf("a dangling flag holds nothing: %+v", got)
	}
}

func TestProcessRunningChecksStartTimeAndState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	fakeProcess(t, root, 200, []string{"codex"}, "5000", "S", 1)
	fakeProcess(t, root, 201, []string{"codex"}, "5001", "Z", 1)
	s := &Service{ProcRoot: root}
	if running, err := s.ProcessRunning(Process{PID: 200, Start: "5000"}); err != nil || !running {
		t.Fatalf("alive: %v %v", running, err)
	}
	if running, err := s.ProcessRunning(Process{PID: 200, Start: "4999"}); err != nil || running {
		t.Fatalf("a reused pid is not the same process: %v %v", running, err)
	}
	if running, err := s.ProcessRunning(Process{PID: 201, Start: "5001"}); err != nil || running {
		t.Fatalf("a zombie is gone: %v %v", running, err)
	}
	if running, err := s.ProcessRunning(Process{PID: 999, Start: "1"}); err != nil || running {
		t.Fatalf("a missing process is gone: %v %v", running, err)
	}
	if err := os.WriteFile(filepath.Join(root, "200", "stat"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessRunning(Process{PID: 200, Start: "5000"}); err == nil {
		t.Fatal("an unreadable stat must not be mistaken for exit")
	} else {
		wantWorkError(t, err, "Cannot verify process exit")
	}
}

// --- revision -----------------------------------------------------------------

func pythonAvailable(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	return path
}

// pyLiteral renders revision material as Python source so python3 can hash the
// same values with json.dumps.
func pyLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return "float(" + strconv.FormatFloat(v, 'g', 17, 64) + ")"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyLiteral(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("pyLiteral: %T", value))
}

func TestRevisionIsByteIdenticalToPython(t *testing.T) {
	python := pythonAvailable(t)
	awkward := "/home/pétur/Ω/😀 <dir> \"q\"\\" + "\n\t" + string(rune(0x7f)) + "tab"
	cases := []struct {
		item    Item
		holders []Process
	}{
		{fixtureItem("/tmp/x"), nil},
		{Item{ID: "01a0906b-8779-7782-b61b-16eb80138757", Provider: "codex", State: "stuck", Updated: 1789541715, CWD: "/home/peter/GIT/hotseat", updatedInteger: true},
			[]Process{{PID: 4242, Start: "123456"}, {PID: 17, Start: "99"}}},
		{Item{ID: "abc", Provider: "claude", State: "recorded", Updated: 1758000000.123456, CWD: ""}, nil},
		{Item{ID: "abc", Provider: "claude", State: "failed", Updated: 1758000000, CWD: awkward}, []Process{{PID: 5, Start: "7"}, {PID: 5, Start: "6"}}},
		{Item{ID: "tiny", Provider: "claude", State: "running", Updated: 0.00001, CWD: "/w"}, []Process{{PID: 1, Start: "1"}}},
		{Item{ID: "big", Provider: "claude", State: "running", Updated: 1e16, CWD: "/w"}, nil},
	}
	for i, c := range cases {
		pairs := []any{}
		for _, p := range c.holders {
			pairs = append(pairs, []any{p.PID, p.Start})
		}
		var updated any = c.item.Updated
		if c.item.updatedInteger {
			updated = int64(c.item.Updated)
		}
		material := fmt.Sprintf("[%s, %s, %s, %s, %s, sorted(%s)]",
			pyLiteral(c.item.ID), pyLiteral(c.item.Provider), pyLiteral(c.item.State),
			pyLiteral(updated), pyLiteral(c.item.CWD), pyLiteral(pairs))
		script := "import hashlib, json\nmaterial = " + material + "\n" +
			"material[5] = [tuple(p) for p in material[5]]\n" +
			"print(hashlib.sha256(json.dumps(material).encode()).hexdigest())"
		out, err := exec.Command(python, "-c", script).Output()
		if err != nil {
			t.Fatalf("case %d: python failed: %v", i, err)
		}
		if got, want := Revision(c.item, c.holders), strings.TrimSpace(string(out)); got != want {
			t.Errorf("case %d: Go %s != Python %s\nmaterial: %s\nGo dumps: %s", i, got, want, material, pyDumps([]any{c.item.ID, c.item.Provider, c.item.State, updated, c.item.CWD, pairs}))
		}
	}
}

func TestPyFloatReprMatchesPython(t *testing.T) {
	python := pythonAvailable(t)
	values := []float64{1758000000.123456, 1e16, 1e15, 9999999999999998, 0.0001, 0.00001, 1, 0, 2.5, 1.5e-7, 123456789012345680, 1e100, 1e22, 3.14159, -0.5, -1e17, 1758000000}
	args := []string{"-c", "import sys\nfor x in sys.argv[1:]:\n    print(repr(float(x)))"}
	for _, v := range values {
		args = append(args, strconv.FormatFloat(v, 'g', 17, 64))
	}
	out, err := exec.Command(python, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(values) {
		t.Fatalf("python printed %d lines", len(lines))
	}
	for i, v := range values {
		if got := pyFloatRepr(v); got != lines[i] {
			t.Errorf("repr(%v): Go %q, Python %q", v, got, lines[i])
		}
	}
}

func TestPyDumpsSeparatorsAndEscapes(t *testing.T) {
	got := pyDumps([]any{"a\"b", nil, 1, 1.0, []any{[]any{2, "x"}}, "é 😀" + string(rune(1))})
	want := `["a\"b", null, 1, 1.0, [[2, "x"]], "` + `\u00e9\u2028\ud83d\ude00\u0001"]`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// --- act: ported from tests/test_work.py WorkActionTests ------------------------

func TestResumeKeepsExactConversationAndDirectory(t *testing.T) {
	cwd := t.TempDir()
	for _, provider := range []string{"codex", "claude"} {
		item := fixtureItem(cwd)
		item.Provider = provider
		item.Revision = Revision(item, nil)
		launcher := &fakeLauncher{}
		s := &Service{
			Codex:             &fakeCodex{},
			Launcher:          launcher,
			resolveHook:       resolveTo(item),
			claudeHoldersHook: func(string) []Process { return nil },
		}
		result, err := s.Act(context.Background(), provider, "fixture-session", "resume", item.Revision, true)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		expected := "claude --resume fixture-session"
		if provider == "codex" {
			expected = "codex resume fixture-session"
		}
		if len(launcher.calls) != 1 || strings.Join(launcher.calls[0], " ") != expected || launcher.cwds[0] != cwd {
			t.Fatalf("%s: launched %v in %v", provider, launcher.calls, launcher.cwds)
		}
		if !result.Launched || result.Operation != "resume" || result.Provider != provider || result.ID != "fixture-session" {
			t.Fatalf("%s: result %+v", provider, result)
		}
	}
}

func TestLaunchFailureIsReported(t *testing.T) {
	item := fixtureItem(t.TempDir())
	launcher := &fakeLauncher{err: errors.New("No supported terminal found.")}
	s := &Service{Codex: &fakeCodex{}, Launcher: launcher, resolveHook: resolveTo(item)}
	if _, err := s.Act(context.Background(), "codex", "fixture-session", "resume", item.Revision, true); err == nil || err.Error() != "No supported terminal found." {
		t.Fatalf("err %v", err)
	}
	s.Launcher = nil
	if _, err := s.Act(context.Background(), "codex", "fixture-session", "resume", item.Revision, true); err == nil {
		t.Fatal("no launcher must be an error")
	}
}

func TestConfirmationRequiredBeforeInspectingOrMutating(t *testing.T) {
	var resolved int32
	launcher := &fakeLauncher{}
	s := &Service{
		Launcher: launcher,
		resolveHook: func(context.Context, string, string) (Item, error) {
			atomic.AddInt32(&resolved, 1)
			return Item{}, nil
		},
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", "anything", false)
	wantWorkError(t, err, "Confirmation is required")
	if resolved != 0 || len(launcher.calls) != 0 {
		t.Fatal("nothing may be inspected or launched before confirmation")
	}
	_, err = s.Act(context.Background(), "codex", "fixture", "delete", "anything", true)
	wantWorkError(t, err, "Unsupported work action")
	if resolved != 0 {
		t.Fatal("an unsupported action must not resolve")
	}
}

func TestChangedSessionCannotBeRebooted(t *testing.T) {
	var kills int32
	item := fixtureItem(t.TempDir())
	s := &Service{resolveHook: resolveTo(item), killHook: func(int) error { atomic.AddInt32(&kills, 1); return nil }}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", "old-revision", true)
	wantWorkError(t, err, "Session changed")
	if kills != 0 {
		t.Fatal("nothing may be signalled")
	}
}

func TestUnavailableActionReportsTheReason(t *testing.T) {
	item := fixtureItem(t.TempDir())
	item.CanReboot = false
	item.Reason = "Cannot identify the exact owning process"
	item.Revision = Revision(item, nil)
	s := &Service{resolveHook: resolveTo(item)}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "Cannot identify the exact owning process")
	item.Reason = ""
	item.CanResume = false
	item.Revision = Revision(item, nil)
	s.resolveHook = resolveTo(item)
	_, err = s.Act(context.Background(), "codex", "fixture", "resume", item.Revision, true)
	wantWorkError(t, err, "This action is not available for the current session state")
}

func TestMissingCwdCannotStopAnyProcess(t *testing.T) {
	var kills int32
	item := fixtureItem("/this/path/does/not/exist")
	item.CanReboot = true
	item.Revision = Revision(item, nil)
	s := &Service{resolveHook: resolveTo(item), killHook: func(int) error { atomic.AddInt32(&kills, 1); return nil }}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "directory")
	if kills != 0 {
		t.Fatal("nothing may be signalled")
	}
}

func TestSharedAppServerIsRefused(t *testing.T) {
	var kills int32
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, nil)
	owner := &Process{PID: 9999999, Start: "1", Shared: true, Argv: []string{"codex", "app-server"}}
	s := &Service{
		Codex:        &fakeCodex{lockHolders: func(string) []int { return []int{9999999} }},
		resolveHook:  resolveTo(item),
		identityHook: func(int, string) *Process { return owner },
		killHook:     func(int) error { atomic.AddInt32(&kills, 1); return nil },
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "shared")
	if kills != 0 {
		t.Fatal("nothing may be signalled")
	}
}

func TestUnknownOwnerIsRefused(t *testing.T) {
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, nil)
	s := &Service{
		Codex:        &fakeCodex{lockHolders: func(string) []int { return []int{9999999} }},
		resolveHook:  resolveTo(item),
		identityHook: func(int, string) *Process { return nil },
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "unknown process")
	s.Codex = &fakeCodex{}
	_, err = s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "Owner changed")
}

func TestResumeRefusesANewLockHolder(t *testing.T) {
	item := fixtureItem(t.TempDir())
	launcher := &fakeLauncher{}
	s := &Service{
		Codex:       &fakeCodex{lockHolders: func(string) []int { return []int{123} }},
		Launcher:    launcher,
		resolveHook: resolveTo(item),
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "resume", item.Revision, true)
	wantWorkError(t, err, "owner")
	if len(launcher.calls) != 0 {
		t.Fatal("nothing may be launched")
	}
	item.Provider = "claude"
	item.Revision = Revision(item, nil)
	s.resolveHook = resolveTo(item)
	s.claudeHoldersHook = func(string) []Process { return []Process{{PID: 1}} }
	_, err = s.Act(context.Background(), "claude", "fixture", "resume", item.Revision, true)
	wantWorkError(t, err, "owner")
}

func TestProcessReuseIsRefusedBeforeSignal(t *testing.T) {
	var kills int32
	owner := Process{PID: 9999999, Start: "1", Shared: false, Argv: []string{"codex"}}
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, []Process{owner})
	calls := 0
	s := &Service{
		Codex:       &fakeCodex{lockHolders: func(string) []int { return []int{9999999} }},
		resolveHook: resolveTo(item),
		identityHook: func(int, string) *Process {
			calls++
			if calls == 1 {
				return &owner
			}
			reused := owner
			reused.Start = "2"
			return &reused
		},
		killHook: func(int) error { atomic.AddInt32(&kills, 1); return nil },
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "identity changed")
	if kills != 0 {
		t.Fatal("nothing may be signalled")
	}
}

func TestOwningProcessChangeIsRefused(t *testing.T) {
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, []Process{{PID: 1, Start: "1"}})
	owner := Process{PID: 2, Start: "1"}
	s := &Service{
		Codex:        &fakeCodex{lockHolders: func(string) []int { return []int{2} }},
		resolveHook:  resolveTo(item),
		identityHook: func(int, string) *Process { return &owner },
		killHook:     func(int) error { t.Fatal("must not signal"); return nil },
	}
	_, err := s.Act(context.Background(), "codex", "fixture", "reboot", item.Revision, true)
	wantWorkError(t, err, "Owning process changed")
}

func TestRebootRefusesToStopTheControllerProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	// controller 300 <- 301 <- 302 (us); the claude holder is 300.
	fakeProcess(t, root, 300, []string{"claude", "--resume", "fixture-session"}, "1", "S", 1)
	fakeProcess(t, root, 301, []string{"bash"}, "2", "S", 300)
	fakeProcess(t, root, 302, []string{"hotseat"}, "3", "S", 301)
	owner := *processIdentity(root, 300, "claude")
	item := fixtureItem(t.TempDir())
	item.Provider = "claude"
	item.CanReboot = true
	item.Revision = Revision(item, []Process{owner})
	s := &Service{
		ProcRoot:          root,
		resolveHook:       resolveTo(item),
		claudeHoldersHook: func(string) []Process { return []Process{owner} },
		selfPID:           func() int { return 302 },
		killHook:          func(int) error { t.Fatal("must not signal"); return nil },
	}
	_, err := s.Act(context.Background(), "claude", "fixture-session", "reboot", item.Revision, true)
	wantWorkError(t, err, "controller")
	if got := s.ancestors(); !got[302] || !got[301] || !got[300] || len(got) != 3 {
		t.Fatalf("ancestors %v", got)
	}
}

func TestRebootGivesUpWhenTheProcessDoesNotExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	fakeProcess(t, root, 400, []string{"codex", "resume", "fixture-session"}, "1", "S", 1)
	owner := *processIdentity(root, 400, "codex")
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, []Process{owner})
	clock := time.Unix(1_000_000, 0)
	var kills []int
	launcher := &fakeLauncher{}
	s := &Service{
		ProcRoot:    root,
		Codex:       &fakeCodex{lockHolders: func(string) []int { return []int{400} }},
		Launcher:    launcher,
		resolveHook: resolveTo(item),
		selfPID:     func() int { return 1 },
		killHook:    func(pid int) error { kills = append(kills, pid); return nil },
		Now:         func() time.Time { return clock },
		sleep:       func(d time.Duration) { clock = clock.Add(d) },
	}
	_, err := s.Act(context.Background(), "codex", "fixture-session", "reboot", item.Revision, true)
	wantWorkError(t, err, "did not exit")
	if len(kills) != 1 || kills[0] != 400 || len(launcher.calls) != 0 {
		t.Fatalf("kills %v launches %v", kills, launcher.calls)
	}
	if clock.Sub(time.Unix(1_000_000, 0)) < 10*time.Second {
		t.Fatalf("polled only %v", clock.Sub(time.Unix(1_000_000, 0)))
	}
}

func TestRebootWaitsForTheCodexLockToBeReleased(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	fakeProcess(t, root, 500, []string{"codex", "resume", "fixture-session"}, "1", "S", 1)
	owner := *processIdentity(root, 500, "codex")
	item := fixtureItem(t.TempDir())
	item.CanReboot = true
	item.Revision = Revision(item, []Process{owner})
	clock := time.Unix(1_000_000, 0)
	launcher := &fakeLauncher{}
	locked := true
	s := &Service{
		ProcRoot: root,
		Codex: &fakeCodex{lockHolders: func(string) []int {
			if locked {
				return []int{500}
			}
			return nil
		}},
		Launcher:    launcher,
		resolveHook: resolveTo(item),
		selfPID:     func() int { return 1 },
		killHook: func(pid int) error {
			// The process dies but keeps the lock for a moment.
			return os.RemoveAll(filepath.Join(root, "500"))
		},
		Now:   func() time.Time { return clock },
		sleep: func(d time.Duration) { clock = clock.Add(d); locked = false },
	}
	result, err := s.Act(context.Background(), "codex", "fixture-session", "reboot", item.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Launched || len(launcher.calls) != 1 || strings.Join(launcher.calls[0], " ") != "codex resume fixture-session" {
		t.Fatalf("result %+v launches %v", result, launcher.calls)
	}
}

func TestRebootTerminatesOnlyOwnedFixtureAndPreservesId(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process identity test")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	python := pythonAvailable(t)
	cwd := t.TempDir()
	for _, provider := range []string{"codex", "claude"} {
		// Real disposable process with exact CLI-shaped argv, no provider requests.
		cmd := exec.Command("bash", "-c", `exec -a "$1" `+python+` -c "import time;time.sleep(60)" --resume fixture-session`, "fixture", provider)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var exited atomic.Bool
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); exited.Store(true); close(done) }()
		defer func() {
			if !exited.Load() {
				_ = cmd.Process.Kill()
			}
			<-done
		}()
		s := New(nil, nil)
		var owner *Process
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if owner = s.ProcessIdentity(cmd.Process.Pid, provider); owner != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if owner == nil {
			t.Fatalf("%s: process was never identified", provider)
		}
		item := fixtureItem(cwd)
		item.Provider = provider
		item.State = "stuck"
		item.CanReboot = true
		item.CanResume = false
		item.Revision = Revision(item, []Process{*owner})
		launcher := &fakeLauncher{}
		s.Launcher = launcher
		s.Codex = &fakeCodex{lockHolders: func(string) []int {
			if exited.Load() {
				return nil
			}
			return []int{cmd.Process.Pid}
		}}
		s.resolveHook = resolveTo(item)
		s.claudeHoldersHook = func(string) []Process { return []Process{*owner} }
		result, err := s.Act(context.Background(), provider, "fixture-session", "reboot", item.Revision, true)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: process still running", provider)
		}
		if !result.Launched || len(launcher.calls) != 1 || launcher.cwds[0] != cwd || launcher.calls[0][len(launcher.calls[0])-1] != "fixture-session" {
			t.Fatalf("%s: result %+v launches %v in %v", provider, result, launcher.calls, launcher.cwds)
		}
	}
}

// --- listing: ported from tests/test_work.py WorkListingTests ------------------

func TestNativeErrorIsListedWithOriginalCwd(t *testing.T) {
	directory := t.TempDir()
	project := filepath.Join(directory, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type": "user", "cwd": "` + directory + `", "message": {"content": "Finish the fixture task"}}` + "\n" +
		`{"type": "assistant", "isApiErrorMessage": true, "cwd": "` + directory + `", "message": {"content": "Usage limit reached"}}` + "\n"
	if err := os.WriteFile(filepath.Join(project, "fixture.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{ProjectsDir: directory, Codex: &fakeCodex{}, claudeHoldersHook: func(string) []Process { return nil }}
	result := s.Listing(context.Background(), 100)
	if len(result.Items) != 1 || len(result.Errors) != 0 {
		t.Fatalf("listing %+v", result)
	}
	item := result.Items[0]
	if item.State != "failed" || item.CWD != directory || !item.CanResume || item.CanReboot {
		t.Fatalf("item %+v", item)
	}
	if item.Title != "Finish the fixture task" || item.ID != "fixture" || item.Provider != "claude" || item.Live || item.Reason != "Reboot requires an exact --resume/--session-id process match" {
		t.Fatalf("item %+v", item)
	}
	if item.Revision != Revision(item, nil) || item.Updated == 0 {
		t.Fatalf("revision/updated %+v", item)
	}
}

func TestNativeListingStatesAndTitles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	f := newNativeFixture(t, "-home-u")
	fakeProcess(t, root, 600, []string{"claude", "--resume", "live"}, "1", "S", 1)
	fakeProcess(t, root, 601, []string{"claude", "--input-format", "stream-json", "--resume", "bridge"}, "2", "S", 1)
	fakeProcess(t, root, 602, []string{"claude", "--resume", "stuck"}, "3", "S", 1)
	writeTranscript(t, f.dir, "live", entryUser("<system>hidden</system>"), entryUser("# AGENTS.md"), map[string]any{"type": "user", "cwd": "/w", "message": map[string]any{"content": strings.Repeat("x", 200)}})
	writeTranscript(t, f.dir, "bridge", entryUser("bridge prompt"), map[string]any{"type": "assistant", "cwd": "/b", "message": map[string]any{"content": "ok"}})
	writeTranscript(t, f.dir, "stuck", entryUser("stuck prompt"), limitError())
	writeTranscript(t, f.dir, "quiet", entryUser("quiet prompt"))
	writeTranscript(t, f.dir, "notitle", map[string]any{"type": "assistant", "message": map[string]any{"content": "no user turn"}})
	writeTranscript(t, f.dir, "junk", map[string]any{"type": "summary"})
	writeTranscript(t, f.dir, "empty")
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"live", "bridge", "stuck", "quiet", "notitle", "junk", "empty"} {
		when := base.Add(-time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(f.dir, name+".jsonl"), when, when); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{ProjectsDir: f.projects, ProcRoot: root}
	result := s.Listing(context.Background(), 100)
	byID := map[string]Item{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	if len(byID) != 5 {
		t.Fatalf("items %+v", result.Items)
	}
	if result.Items[0].ID != "stuck" || result.Items[0].State != "stuck" || result.Items[0].CanReboot != true {
		t.Fatalf("stuck first: %+v", result.Items[0])
	}
	live := byID["live"]
	if live.State != "running" || !live.Live || live.CanResume || !live.CanReboot || live.Reason != "" || len(live.Title) != 160 || live.CWD != "/w" {
		t.Fatalf("live %+v", live)
	}
	if live.Revision != Revision(live, []Process{*processIdentity(root, 600, "claude")}) {
		t.Fatal("live revision must include its holder")
	}
	bridge := byID["bridge"]
	if bridge.State != "running" || bridge.CanReboot || bridge.Reason != "Managed streaming process; use its host integration" || bridge.Title != "bridge prompt" || bridge.CWD != "/b" {
		t.Fatalf("bridge %+v", bridge)
	}
	if quiet := byID["quiet"]; quiet.State != "recorded" || !quiet.CanResume || quiet.CanReboot || quiet.CWD != "" {
		t.Fatalf("quiet %+v", quiet)
	}
	if notitle := byID["notitle"]; notitle.Title != "notitle" {
		t.Fatalf("title must fall back to the id: %+v", notitle)
	}
	if _, ok := byID["junk"]; ok {
		t.Fatal("a transcript without turns is not listed")
	}
	for i := 1; i < len(result.Items); i++ {
		if result.Items[i-1].State != "stuck" && result.Items[i-1].Updated < result.Items[i].Updated {
			t.Fatalf("not ordered by recency: %+v", result.Items)
		}
	}
	if limited := s.Listing(context.Background(), 2); len(limited.Items) != 2 || limited.Items[0].ID != "live" || limited.Items[1].ID != "bridge" {
		t.Fatalf("limit applies to native transcripts: %+v", limited.Items)
	}
}

func createStateDB(t *testing.T, path string, rows [][3]string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT, title TEXT)"); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var cwd, title any = row[1], row[2]
		if row[1] == "" {
			cwd = nil
		}
		if row[2] == "" {
			title = nil
		}
		if _, err := db.Exec("INSERT INTO threads VALUES (?,?,?)", row[0], cwd, title); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexListingUsesStateMetadataAndProcessIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /proc on Windows")
	}
	root := t.TempDir()
	codexHome := t.TempDir()
	fakeProcess(t, root, 700, []string{"codex", "resume", "t-live"}, "10", "S", 1)
	fakeProcess(t, root, 701, []string{"codex", "app-server"}, "11", "S", 1)
	fakeProcess(t, root, 702, []string{"codex", "resume", "t-live"}, "12", "S", 1)
	old := filepath.Join(codexHome, "state_1.sqlite")
	createStateDB(t, old, [][3]string{{"t-live", "/old", "old title"}})
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	createStateDB(t, filepath.Join(codexHome, "state_5.sqlite"), [][3]string{{"t-live", "/live", "Live title"}, {"t-shared", "/shared", ""}})
	if err := os.WriteFile(filepath.Join(codexHome, "state_9.sqlite"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := []CodexRow{
		{ThreadID: "t-live", Short: "t-live", LastAt: 300, Topic: "topic", Holders: []int{700}, Live: true, State: "idle"},
		{ThreadID: "t-shared", Short: "t-shared", LastAt: 200, Topic: "shared topic", Holders: []int{701}, Live: true, State: "working"},
		{ThreadID: "t-unknown", Short: "t-unknown", LastAt: 100, Topic: "unknown topic", Holders: []int{999999}, Live: true, State: "stuck"},
		{ThreadID: "t-closed", Short: "t-closed", LastAt: 400.5, Topic: "closed topic", State: "closed"},
		{ThreadID: "t-failed", Short: "t-failed", LastAt: 50, Topic: "failed topic", State: "failed"},
	}
	s := &Service{ProjectsDir: filepath.Join(t.TempDir(), "none"), CodexHome: codexHome, ProcRoot: root, Codex: &fakeCodex{rows: rows}}
	result := s.Listing(context.Background(), 100)
	if len(result.Items) != 5 || len(result.Errors) != 0 {
		t.Fatalf("listing %+v", result)
	}
	var order []string
	byID := map[string]Item{}
	for _, item := range result.Items {
		order = append(order, item.ID)
		byID[item.ID] = item
	}
	if strings.Join(order, ",") != "t-unknown,t-failed,t-closed,t-live,t-shared" {
		t.Fatalf("order %v", order)
	}
	live := byID["t-live"]
	if live.Title != "Live title" || live.CWD != "/live" || !live.CanReboot || live.CanResume || live.Reason != "" || !live.Live {
		t.Fatalf("live %+v", live)
	}
	if live.Revision != Revision(live, []Process{*processIdentity(root, 700, "codex")}) || !live.updatedInteger {
		t.Fatalf("live revision %+v", live)
	}
	shared := byID["t-shared"]
	if shared.Title != "shared topic" || shared.CWD != "/shared" || shared.CanReboot || shared.Reason != "Managed by a shared app-server; use its host integration" {
		t.Fatalf("shared %+v", shared)
	}
	unknown := byID["t-unknown"]
	if unknown.CanReboot || unknown.Reason != "Cannot identify the exact owning process" || unknown.Title != "unknown topic" || unknown.CWD != "" {
		t.Fatalf("unknown %+v", unknown)
	}
	closed := byID["t-closed"]
	if !closed.CanResume || closed.CanReboot || closed.Live || closed.updatedInteger || closed.Revision != Revision(closed, nil) {
		t.Fatalf("closed %+v", closed)
	}

	meta := s.CodexMetadata(context.Background(), []string{"t-live"})
	if meta["t-live"].Title != "Live title" {
		t.Fatalf("newest database must win: %+v", meta)
	}
	if got := s.CodexMetadata(context.Background(), nil); len(got) != 0 {
		t.Fatalf("no identifiers, no lookup: %v", got)
	}
	if got := s.CodexMetadata(context.Background(), []string{"nope"}); len(got) != 0 {
		t.Fatalf("unknown ids: %v", got)
	}
}

func TestListingCarriesCodexErrors(t *testing.T) {
	s := &Service{ProjectsDir: filepath.Join(t.TempDir(), "none"), Codex: &fakeCodex{recentErr: errors.New("No Codex thread history at /x.")}, claudeHoldersHook: func(string) []Process { return nil }}
	result := s.Listing(context.Background(), 100)
	if len(result.Items) != 0 || len(result.Errors) != 1 || result.Errors[0] != "No Codex thread history at /x." {
		t.Fatalf("listing %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"items":[],"errors":["No Codex thread history at /x."]}` {
		t.Fatalf("json %s", encoded)
	}
	if none := (&Service{ProjectsDir: filepath.Join(t.TempDir(), "none"), claudeHoldersHook: func(string) []Process { return nil }}).Listing(context.Background(), 100); len(none.Items) != 0 || len(none.Errors) != 0 {
		t.Fatalf("no codex means no codex error: %+v", none)
	}
}

func TestItemJSONShape(t *testing.T) {
	item := Item{ID: "a", Provider: "claude", Title: "t", State: "failed", Updated: 1.5, CWD: "/w", Live: false, CanResume: true, CanReboot: false, Reason: "r", Revision: "v"}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"a","provider":"claude","title":"t","state":"failed","updated":1.5,"cwd":"/w","live":false,"can_resume":true,"can_reboot":false,"reason":"r","revision":"v"}`
	if string(encoded) != want {
		t.Fatalf("json %s", encoded)
	}
	encoded, err = json.Marshal(ItemDetail{Item: item, LastUser: "u", LastAssistant: "a", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(encoded), `,"revision":"v","last_user":"u","last_assistant":"a","model":"m"}`) {
		t.Fatalf("detail json %s", encoded)
	}
	encoded, _ = json.Marshal(ActResult{ID: "a", Provider: "codex", Operation: "resume", Launched: true})
	if string(encoded) != `{"id":"a","provider":"codex","operation":"resume","launched":true}` {
		t.Fatalf("act json %s", encoded)
	}
}

func TestResolveNeedsExactlyOneMatch(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "one", entryUser("hello"))
	s := &Service{ProjectsDir: f.projects, claudeHoldersHook: func(string) []Process { return nil }}
	item, err := s.Resolve(context.Background(), "claude", "one")
	if err != nil || item.ID != "one" {
		t.Fatalf("resolve %+v %v", item, err)
	}
	if _, err := s.Resolve(context.Background(), "codex", "one"); err == nil {
		t.Fatal("provider must match")
	} else {
		wantWorkError(t, err, "missing or ambiguous")
	}
	if _, err := s.Resolve(context.Background(), "claude", "on"); err == nil {
		t.Fatal("prefixes are not matches")
	}
}

func TestDetailForClaudeAndCodex(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "one",
		inspectEntry("user", "please do the thing", nil),
		map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-fable-5-1", "content": []any{map[string]any{"type": "text", "text": "doing <speak>" + strings.Repeat("it ", 300) + "</speak>"}}}})
	var items []string
	for i := 0; i < 10; i++ {
		items = append(items, fmt.Sprintf("message %d", i))
	}
	items = append(items, "")
	codex := &fakeCodex{
		rows:  []CodexRow{{ThreadID: "thread", LastAt: 5, Topic: "the topic", State: "closed"}},
		items: items,
	}
	s := &Service{ProjectsDir: f.projects, CodexHome: t.TempDir(), Codex: codex, claudeHoldersHook: func(string) []Process { return nil }}

	claude, err := s.Detail(context.Background(), "claude", "one")
	if err != nil {
		t.Fatal(err)
	}
	if claude.LastUser != "please do the thing" || claude.Model != "claude-fable-5-1" || !strings.HasSuffix(claude.LastAssistant, "…") || len([]rune(claude.LastAssistant)) != 401 || strings.Contains(claude.LastAssistant, "<speak>") {
		t.Fatalf("claude detail %+v", claude)
	}
	if claude.Provider != "claude" || claude.Revision == "" {
		t.Fatalf("detail keeps the item: %+v", claude)
	}

	detail, err := s.Detail(context.Background(), "codex", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LastUser != "the topic" || detail.LastAssistant != "message 4\n\nmessage 5\n\nmessage 6\n\nmessage 7\n\nmessage 8\n\nmessage 9" || detail.Model != "" {
		t.Fatalf("codex detail %+v", detail)
	}
	codex.items = []string{strings.Repeat("y", 7000)}
	detail, err = s.Detail(context.Background(), "codex", "thread")
	if err != nil || len(detail.LastAssistant) != 6000 {
		t.Fatalf("tail truncation: %d %v", len(detail.LastAssistant), err)
	}
	codex.itemsErr = errors.New("database is locked")
	if _, err := s.Detail(context.Background(), "codex", "thread"); err == nil {
		t.Fatal("a history error must surface")
	} else {
		wantWorkError(t, err, "database is locked")
	}
	if _, err := s.Detail(context.Background(), "codex", "missing"); err == nil {
		t.Fatal("unknown items are refused")
	}
}
