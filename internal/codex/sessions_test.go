package codex

// Recent Codex sessions, and the actions that get a stuck one moving.
//
// Fixture databases throughout; no test queues a real message or closes a real
// process.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var now = float64(time.Now().Unix())

func TestTextIsDecodedRatherThanPatternMatched(t *testing.T) {
	// Hand-decoding JSON escapes turns apostrophes into mojibake.
	blob, _ := json.Marshal(map[string]any{"type": "userMessage", "content": []any{map[string]any{"text": "I’ll use the skill"}}})
	if got := firstText(string(blob)); got != "I’ll use the skill" {
		t.Fatalf("firstText = %q", got)
	}
}

func TestBookkeepingIsNotMistakenForATopic(t *testing.T) {
	// Taking the first string finds the item type, which reads as a topic.
	blob := `{"type": "userMessage", "role": "user", "content": [{"text": "the real prompt"}]}`
	if got := firstText(blob); got != "the real prompt" {
		t.Fatalf("firstText = %q", got)
	}
}

func TestAnItemWithNoTextYieldsNothing(t *testing.T) {
	if got := firstText(`{"type": "toolCall"}`); got != "" {
		t.Fatalf("firstText = %q", got)
	}
}

func TestDamagedJSONIsSurvivable(t *testing.T) {
	if firstText("{not json") != "" || firstText("") != "" || firstText(`"just a string"`) != "" {
		t.Fatal("damaged input must yield nothing")
	}
}

func TestTheOwnTextKeyWinsOverNestedOnes(t *testing.T) {
	// Python checks the dict's own "text" before recursing into its values.
	if got := firstText(`{"a": {"text": "inner"}, "text": "outer"}`); got != "outer" {
		t.Fatalf("firstText = %q", got)
	}
	if got := firstText(`{"a": {"text": "  "}, "b": [{"text": "  spaced\n out  "}]}`); got != "spaced out" {
		t.Fatalf("firstText = %q", got)
	}
}

type sessionsFixture struct {
	t        *testing.T
	home     string
	db       *sql.DB
	sessions *Sessions
	holders  map[string][]int
}

func newSessionsFixture(t *testing.T) *sessionsFixture {
	t.Helper()
	home := t.TempDir()
	f := &sessionsFixture{t: t, home: home, sessions: NewSessions(home), holders: map[string][]int{}}
	f.sessions.holders = func(threadID string) []int { return f.holders[threadID] }
	db, err := sql.Open("sqlite", f.sessions.HistoryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE thread_turns (thread_id TEXT, turn_id TEXT, status TEXT, started_at INT)`,
		`CREATE TABLE thread_items (thread_id TEXT, rollout_ordinal INT, item_json TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	f.db = db
	return f
}

func (f *sessionsFixture) turn(thread, status string, at float64) {
	f.t.Helper()
	if at == 0 {
		at = now
	}
	if _, err := f.db.Exec("INSERT INTO thread_turns VALUES (?,?,?,?)", thread, "t"+status, status, int64(at)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sessionsFixture) item(thread, text string, ordinal int) {
	f.t.Helper()
	blob, _ := json.Marshal(map[string]any{"type": "userMessage", "content": []any{map[string]any{"text": text}}})
	if _, err := f.db.Exec("INSERT INTO thread_items VALUES (?,?,?)", thread, ordinal, string(blob)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sessionsFixture) recent() []Session {
	f.t.Helper()
	found, err := f.sessions.Recent(DefaultSessionLimit)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func TestSessionsAreListedNewestFirst(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "completed", now-600)
	f.turn("bbb", "completed", now)
	found := f.recent()
	if len(found) != 2 || found[0].Short != "bbb" || found[1].Short != "aaa" {
		t.Fatalf("found = %+v", found)
	}
	if found[0].LastAt != now || found[0].Turns != 1 {
		t.Fatalf("entry = %+v", found[0])
	}
}

func TestTheOpeningPromptIsShownAsTheTopic(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "completed", 0)
	f.item("aaa", "how do i enable device code login?", 1)
	if got := f.recent()[0].Topic; got != "how do i enable device code login?" {
		t.Fatalf("topic = %q", got)
	}
}

func TestPreamblesAreSkippedAndTopicsTruncated(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "completed", 0)
	f.item("aaa", "<environment_context>...", 1)
	f.item("aaa", "You are a helpful assistant", 2)
	f.item("aaa", "# AGENTS.md", 3)
	f.item("aaa", strings.Repeat("é", 150), 4)
	if got := f.recent()[0].Topic; got != strings.Repeat("é", 100) {
		t.Fatalf("topic = %q", got)
	}
	f.turn("bbb", "completed", now+1)
	f.item("bbb", "<system>", 1)
	if got := f.recent()[0].Topic; got != "(no prompt recorded)" {
		t.Fatalf("topic = %q", got)
	}
}

func TestFailedTurnsAreCounted(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "failed", now-10)
	f.turn("aaa", "failed", now-9)
	f.turn("aaa", "completed", now)
	entry := f.recent()[0]
	if entry.Failed != 2 || entry.Turns != 3 || entry.LastStatus != "completed" || entry.Active != 0 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestAHeldSessionWhoseLastTurnFailedIsStuck(t *testing.T) {
	// The holder keeps failing and nothing else can take the conversation.
	f := newSessionsFixture(t)
	f.turn("aaa", "failed", 0)
	f.holders["aaa"] = []int{123}
	entry := f.recent()[0]
	if entry.State != "stuck" || !entry.Live || len(entry.Holders) != 1 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestAFailedSessionNobodyHoldsIsMerelyFailed(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "failed", 0)
	if got := f.recent()[0].State; got != "failed" {
		t.Fatalf("state = %q", got)
	}
}

func TestARunningHeldSessionIsWorking(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "inProgress", 0)
	f.holders["aaa"] = []int{123}
	entry := f.recent()[0]
	if entry.State != "working" || entry.Active != 1 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestASessionWithNoHolderIsClosed(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "completed", 0)
	entry := f.recent()[0]
	if entry.State != "closed" {
		t.Fatalf("state = %q", entry.State)
	}
	raw, _ := json.Marshal(entry)
	if !strings.Contains(string(raw), `"holders":[]`) {
		t.Fatalf("holders must be an empty list: %s", raw)
	}
	for _, key := range []string{"thread_id", "short", "last_at", "turns", "failed", "active", "last_status", "topic", "holders", "live", "state"} {
		if !strings.Contains(string(raw), `"`+key+`":`) {
			t.Errorf("row lacks %q: %s", key, raw)
		}
	}
}

func TestAHeldCompletedSessionIsIdle(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("aaa", "completed", 0)
	f.holders["aaa"] = []int{9}
	if got := f.recent()[0].State; got != "idle" {
		t.Fatalf("state = %q", got)
	}
}

func TestAPrefixResolvesToOneSession(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("abc123", "completed", 0)
	entry, err := f.sessions.Resolve("abc")
	if err != nil || entry.ThreadID != "abc123" {
		t.Fatalf("entry = %+v err = %v", entry, err)
	}
}

func TestAnAmbiguousPrefixIsRefused(t *testing.T) {
	f := newSessionsFixture(t)
	f.turn("abc111", "completed", now)
	f.turn("abc222", "completed", now-5)
	_, err := f.sessions.Resolve("abc")
	var serr *SessionError
	if !errors.As(err, &serr) || serr.Msg != "'abc' matches several sessions: abc111, abc222" {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnknownPrefixIsRefused(t *testing.T) {
	f := newSessionsFixture(t)
	_, err := f.sessions.Resolve("zzz")
	var serr *SessionError
	if !errors.As(err, &serr) || serr.Msg != "No recent Codex session starting 'zzz'" {
		t.Fatalf("err = %v", err)
	}
}

func TestAMissingHistoryDatabaseIsAClearError(t *testing.T) {
	f := newSessionsFixture(t)
	f.db.Close()
	os.Remove(f.sessions.HistoryDB())
	_, err := f.sessions.Recent(DefaultSessionLimit)
	var serr *SessionError
	if !errors.As(err, &serr) || !strings.Contains(serr.Msg, "thread history") {
		t.Fatalf("err = %v", err)
	}
	if !f.sessions.Available() == false {
		t.Fatal("Available must be false without a database")
	}
}

func TestADamagedDatabaseIsAClearError(t *testing.T) {
	f := newSessionsFixture(t)
	f.db.Close()
	if err := os.WriteFile(f.sessions.HistoryDB(), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.sessions.Recent(DefaultSessionLimit)
	var serr *SessionError
	if !errors.As(err, &serr) || !strings.HasPrefix(serr.Msg, "could not read Codex history: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestAMessageGoesThroughTheDocumentedQueue(t *testing.T) {
	// No terminal, no typing: codex hands it to the live session itself.
	s := NewSessions(t.TempDir())
	var calls [][]string
	s.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		calls = append(calls, argv)
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("queue must run with a timeout")
		}
		return Result{Code: 0, Stdout: "Queued"}, nil
	}
	result, err := s.Nudge(context.Background(), "abc", "continue")
	if err != nil || !result.Queued || result.ThreadID != "abc" || result.Message != "continue" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if strings.Join(calls[0], " ") != "codex queue --thread abc --message continue" {
		t.Fatalf("argv = %q", calls[0])
	}
}

func TestARefusalIsReported(t *testing.T) {
	s := NewSessions(t.TempDir())
	s.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		return Result{Code: 1, Stderr: "no such thread\n"}, nil
	}
	_, err := s.Nudge(context.Background(), "abc", "continue")
	var serr *SessionError
	if !errors.As(err, &serr) || serr.Msg != "no such thread" {
		t.Fatalf("err = %v", err)
	}
	s.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		return Result{Code: 1}, nil
	}
	_, err = s.Nudge(context.Background(), "abc", "continue")
	if !errors.As(err, &serr) || serr.Msg != "codex queue failed" {
		t.Fatalf("err = %v", err)
	}
	s.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		return Result{}, errors.New("codex: not found")
	}
	_, err = s.Nudge(context.Background(), "abc", "continue")
	if !errors.As(err, &serr) || serr.Msg != "could not queue a message: codex: not found" {
		t.Fatalf("err = %v", err)
	}
}

type signalRecord struct {
	pid int
	sig syscall.Signal
}

func TestHoldersAreTerminatedNotKilled(t *testing.T) {
	// Codex releases its lock and flushes history on the way out.
	s := NewSessions(t.TempDir())
	s.holders = func(string) []int { return []int{42} }
	s.parentOf = func(int) int { return 0 }
	var signals []signalRecord
	closed, err := s.Release("abc", func(pid int, sig syscall.Signal) error {
		signals = append(signals, signalRecord{pid, sig})
		return nil
	})
	if err != nil || len(closed) != 1 || closed[0] != 42 {
		t.Fatalf("closed = %v err = %v", closed, err)
	}
	if len(signals) != 1 || signals[0] != (signalRecord{42, syscall.SIGTERM}) {
		t.Fatalf("signals = %v", signals)
	}
}

func TestTheLauncherIsClosedRatherThanTheInnerProcess(t *testing.T) {
	// Signalling the child alone orphans the session that launched it.
	s := NewSessions(t.TempDir())
	s.holders = func(string) []int { return []int{42} }
	s.parentOf = func(int) int { return 7 }
	s.looksLikeCodex = func(int) bool { return true }
	var pids []int
	closed, err := s.Release("abc", func(pid int, sig syscall.Signal) error {
		pids = append(pids, pid)
		return nil
	})
	if err != nil || len(pids) != 1 || pids[0] != 7 || closed[0] != 7 {
		t.Fatalf("pids = %v closed = %v err = %v", pids, closed, err)
	}
	// A parent that is not codex (a shell, say) is left alone.
	s.looksLikeCodex = func(int) bool { return false }
	pids = nil
	_, _ = s.Release("abc", func(pid int, sig syscall.Signal) error { pids = append(pids, pid); return nil })
	if len(pids) != 1 || pids[0] != 42 {
		t.Fatalf("pids = %v", pids)
	}
}

func TestNothingToCloseIsNotAnError(t *testing.T) {
	s := NewSessions(t.TempDir())
	s.holders = func(string) []int { return []int{} }
	closed, err := s.Release("abc", nil)
	if err != nil || closed == nil || len(closed) != 0 {
		t.Fatalf("closed = %v err = %v", closed, err)
	}
}

func TestASignalFailureIsReported(t *testing.T) {
	s := NewSessions(t.TempDir())
	s.holders = func(string) []int { return []int{42} }
	s.parentOf = func(int) int { return 0 }
	_, err := s.Release("abc", func(int, syscall.Signal) error { return errors.New("operation not permitted") })
	var serr *SessionError
	if !errors.As(err, &serr) || serr.Msg != "could not close process 42: operation not permitted" {
		t.Fatalf("err = %v", err)
	}
}

func TestLockHoldersAreReadThroughFuser(t *testing.T) {
	s := NewSessions(t.TempDir())
	lock := filepath.Join(s.LockDir(), "abc.lock")
	if err := os.MkdirAll(s.LockDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := s.LockHolders("abc"); len(got) != 0 {
		t.Fatalf("no lock file must mean no holders: %v", got)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Which = func(string) (string, error) { return "", errors.New("missing") }
	if got := s.LockHolders("abc"); len(got) != 0 {
		t.Fatalf("without fuser there are no holders: %v", got)
	}
	s.Which = func(string) (string, error) { return "/usr/bin/fuser", nil }
	var argv []string
	s.Run = func(ctx context.Context, cmd, env []string) (Result, error) {
		argv = cmd
		return Result{Code: 0, Stdout: " 1234  5678 c\n", Stderr: lock + ":"}, nil
	}
	got := s.LockHolders("abc")
	if len(got) != 2 || got[0] != 1234 || got[1] != 5678 {
		t.Fatalf("holders = %v", got)
	}
	if strings.Join(argv, " ") != "fuser "+lock {
		t.Fatalf("argv = %q", argv)
	}
	s.Run = func(context.Context, []string, []string) (Result, error) { return Result{}, errors.New("boom") }
	if got := s.LockHolders("abc"); len(got) != 0 {
		t.Fatalf("a failing fuser must mean no holders: %v", got)
	}
}

func TestProcessLookupsSurviveMissingProcesses(t *testing.T) {
	if parentPID(1<<30) != 0 || cmdlineMentionsCodex(1<<30) {
		t.Fatal("a missing process has no parent and is not codex")
	}
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc on this platform")
	}
	if parentPID(os.Getpid()) != os.Getppid() {
		t.Fatalf("parentPID = %d, want %d", parentPID(os.Getpid()), os.Getppid())
	}
}
