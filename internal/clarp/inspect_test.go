package clarp

// Recovering what a stopped piece of work was doing. Ported from
// plugins/clarp/tests/test_inspect.py. Uses fixture databases and transcripts;
// never the real ones.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/work"
)

const preamble = "This session is being continued from a previous conversation that ran " +
	"out of context.\n\nSummary: 1. Primary Request and Intent: migrate the " +
	"warehouse and verify the row counts."

// inspectMerged is the merge the Python `inspect` handler did and the CLI will do
// again: Clarp answers first, the native transcript second, then it is an error.
func inspectMerged(s *Service, identifier string) (*work.Detail, error) {
	found, err := s.Inspect(context.Background(), identifier)
	if err != nil || found != nil {
		return found, err
	}
	return work.Inspect(s.ProjectsDir, identifier)
}

// --- cleaning (shared helpers in internal/work, exercised with the plugin's cases)

func TestSpeechMarkupIsStripped(t *testing.T) {
	text := `<speak><speed ratio="1.1"/>Done and pushed.<break time="300ms"/> Next.</speak>`
	if got := work.Clean(text); got != "Done and pushed. Next." {
		t.Fatalf("got %q", got)
	}
}

func TestWhitespaceIsCollapsed(t *testing.T) {
	if got := work.Clean("a\n\n  b\tc"); got != "a b c" {
		t.Fatalf("got %q", got)
	}
}

func TestLongTextIsTruncatedWithAMarker(t *testing.T) {
	out := work.Trim(strings.Repeat("x", 900), 50)
	if len([]rune(out)) != 51 || !strings.HasSuffix(out, "…") {
		t.Fatalf("got %q", out)
	}
}

func TestASummaryIsMinedOutOfAContinuationPreamble(t *testing.T) {
	// The preamble is not user intent, but it describes the work.
	summary := work.SummaryOf(preamble)
	if !strings.Contains(summary, "migrate the warehouse") || strings.Contains(summary, "ran out of context") {
		t.Fatalf("got %q", summary)
	}
}

func TestOrdinaryTextYieldsNoSummary(t *testing.T) {
	if got := work.SummaryOf("just a normal message"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyInputIsSafe(t *testing.T) {
	if work.Clean("") != "" || work.SummaryOf("") != "" {
		t.Fatal("empty input should yield empty output")
	}
}

// --- clarp -------------------------------------------------------------------

var inspectSchema = []string{
	`CREATE TABLE agents (agent_id TEXT, persona TEXT, backend TEXT, cwd TEXT,
                                 session TEXT, model TEXT, deleted_at INT)`,
	`CREATE TABLE messages (agent_id TEXT, seq INT, role TEXT, text TEXT,
                                   timestamp TEXT)`,
	`CREATE TABLE queued_turns (agent_id TEXT, queue_seq INT, status TEXT,
                                       text TEXT, origin TEXT, enqueued_at INT)`,
}

func inspectFixture(t *testing.T) *fixture {
	f := newFixture(t, inspectSchema)
	f.exec("INSERT INTO agents VALUES ('a1','Domi','claude','/work','domi','fable',NULL)")
	return f
}

func (f *fixture) message(seq int, role, text string) {
	f.exec("INSERT INTO messages VALUES ('a1',?,?,?,?)", seq, role, text, "2026-09-14T10:00:00Z")
}

func (f *fixture) detail(identifier string) *work.Detail {
	f.t.Helper()
	found, err := inspectMerged(f.svc, identifier)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func TestIdentityAndLocationAreReported(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "do the thing")
	detail := f.detail("domi")
	if detail.Kind != "clarp" || detail.ID != "domi" || detail.Name != "Domi" || detail.Backend != "claude" {
		t.Fatalf("got %+v", detail)
	}
	if detail.CWD == nil || *detail.CWD != "/work" || detail.Model == nil || *detail.Model != "fable" {
		t.Fatalf("got cwd=%v model=%v", detail.CWD, detail.Model)
	}
	if detail.LastActivity == nil || *detail.LastActivity != "2026-09-14T10:00:00Z" {
		t.Fatalf("got %v", detail.LastActivity)
	}
}

func TestTheLastRealPromptIsFound(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "first ask")
	f.message(2, "assistant", "working")
	f.message(3, "user", "second ask")
	detail := f.detail("domi")
	if detail.LastUser != "second ask" || detail.LastAssistant != "working" {
		t.Fatalf("got %+v", detail)
	}
	if len(detail.Recent) != 3 || detail.Recent[0].Text != "first ask" || detail.Recent[2].Role != "user" {
		t.Fatalf("recent is oldest first: %+v", detail.Recent)
	}
}

func TestAPreambleDoesNotMasqueradeAsTheUsersRequest(t *testing.T) {
	// It is machine-generated; reporting it as what they asked would mislead.
	f := inspectFixture(t)
	f.message(1, "user", "the real ask")
	f.message(2, "user", preamble)
	detail := f.detail("domi")
	if detail.LastUser != "the real ask" {
		t.Fatalf("got %q", detail.LastUser)
	}
	if !strings.Contains(detail.Summary, "migrate the warehouse") {
		t.Fatalf("got %q", detail.Summary)
	}
}

func TestQueuedWorkIsSurfaced(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "go")
	f.exec("INSERT INTO queued_turns VALUES ('a1',1,'queued','continue please','automation',0)")
	queued := f.detail("domi").Queued
	if len(queued) != 1 {
		t.Fatalf("got %+v", queued)
	}
	turn, ok := queued[0].(QueuedTurn)
	if !ok || turn.Origin == nil || *turn.Origin != "automation" || turn.Text != "continue please" {
		t.Fatalf("got %+v", queued[0])
	}
}

func TestStartedTurnsAreNotReportedAsWaiting(t *testing.T) {
	f := inspectFixture(t)
	f.exec("INSERT INTO queued_turns VALUES ('a1',1,'started','ran already','user',0)")
	if queued := f.detail("domi").Queued; len(queued) != 0 {
		t.Fatalf("got %+v", queued)
	}
}

func TestAnUnknownNameIsAnError(t *testing.T) {
	f := inspectFixture(t)
	found, err := f.svc.Inspect(context.Background(), "nobody")
	if found != nil || err != nil {
		t.Fatalf("Clarp alone says nil, nil: %+v, %v", found, err)
	}
	_, err = inspectMerged(f.svc, "nobody")
	var inspectError *work.InspectError
	if !errors.As(err, &inspectError) || err.Error() != "nothing found for 'nobody'" {
		t.Fatalf("got %v", err)
	}
}

func TestADeletedAgentIsNotInspected(t *testing.T) {
	f := inspectFixture(t)
	f.exec("UPDATE agents SET deleted_at = 1")
	if found, err := f.svc.Inspect(context.Background(), "domi"); found != nil || err != nil {
		t.Fatalf("got %+v, %v", found, err)
	}
}

func TestOnlyTheLastTwoHundredMessagesAreRead(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "ancient ask")
	for seq := 2; seq <= 202; seq++ {
		f.message(seq, "assistant", "noise")
	}
	detail := f.detail("domi")
	if detail.LastUser != "" || detail.LastAssistant != "noise" || len(detail.Recent) != 6 {
		t.Fatalf("got %+v", detail)
	}
}

func TestEmptyAndMarkedUpMessagesAreCleaned(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "   ")
	f.message(2, "assistant", "<speak>Pushed.</speak>")
	f.exec("INSERT INTO queued_turns VALUES ('a1',1,'queued',?,NULL,0)", strings.Repeat("y", 400))
	detail := f.detail("domi")
	if detail.LastUser != "" || detail.LastAssistant != "Pushed." {
		t.Fatalf("got %+v", detail)
	}
	turn := detail.Queued[0].(QueuedTurn)
	if turn.Origin != nil || len([]rune(turn.Text)) != 301 {
		t.Fatalf("got %+v", turn)
	}
}

func TestClarpDetailJSONShape(t *testing.T) {
	f := inspectFixture(t)
	f.message(1, "user", "go")
	f.exec("INSERT INTO queued_turns VALUES ('a1',1,'queued','next','automation',0)")
	encoded := mustJSON(t, f.detail("domi"))
	want := `{"kind":"clarp","id":"domi","name":"Domi","backend":"claude","model":"fable","cwd":"/work","branch":null,` +
		`"last_user":"go","summary":"","last_assistant":"","last_activity":"2026-09-14T10:00:00Z",` +
		`"queued":[{"origin":"automation","text":"next"}],"recent":[{"role":"user","text":"go"}],"turns":0}`
	if encoded != want {
		t.Fatalf("got %s\nwant %s", encoded, want)
	}
}

func TestAnUnreadableStateIsAnInspectError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&Service{StatePath: path}).Inspect(context.Background(), "domi")
	var inspectError *work.InspectError
	if !errors.As(err, &inspectError) || !strings.HasPrefix(err.Error(), "could not read Clarp state: ") {
		t.Fatalf("got %v", err)
	}
}

// --- native, through the merge ---------------------------------------------------

func nativeInspectFixture(t *testing.T) *nativeFixture {
	t.Helper()
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	if err := os.MkdirAll(filepath.Join(projects, "-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &nativeFixture{t: t, projects: projects, svc: &Service{
		// Clarp must not answer for a native id.
		StatePath:   filepath.Join(home, "absent.sqlite"),
		ProjectsDir: projects,
		Now:         func() time.Time { return now },
	}}
}

func (f *nativeFixture) write(name string, entries []map[string]any) string {
	f.t.Helper()
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, mustJSON(f.t, entry))
	}
	path := filepath.Join(f.projects, "-work", name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func entry(kind, text string, extra map[string]any) map[string]any {
	e := map[string]any{"type": kind, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func (f *nativeFixture) detail(identifier string) *work.Detail {
	f.t.Helper()
	found, err := inspectMerged(f.svc, identifier)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func TestPromptReplyAndLocationAreRecovered(t *testing.T) {
	f := nativeInspectFixture(t)
	f.write("abc123", []map[string]any{
		entry("user", "build the report", map[string]any{"cwd": "/work", "gitBranch": "main"}),
		entry("assistant", "starting now", map[string]any{"cwd": "/work"}),
	})
	detail := f.detail("abc123")
	if detail.Kind != "native" || detail.LastUser != "build the report" || detail.LastAssistant != "starting now" {
		t.Fatalf("got %+v", detail)
	}
	if detail.CWD == nil || *detail.CWD != "/work" || detail.Branch == nil || *detail.Branch != "main" {
		t.Fatalf("got cwd=%v branch=%v", detail.CWD, detail.Branch)
	}
}

func TestTheErrorThatStoppedItIsNotQuotedAsItsLastWords(t *testing.T) {
	f := nativeInspectFixture(t)
	f.write("abc123", []map[string]any{
		entry("user", "go", nil),
		entry("assistant", "on it", nil),
		entry("assistant", "You've hit your session limit", map[string]any{"isApiErrorMessage": true}),
	})
	if got := f.detail("abc123").LastAssistant; got != "on it" {
		t.Fatalf("got %q", got)
	}
}

func TestAShortIDResolves(t *testing.T) {
	f := nativeInspectFixture(t)
	f.write("abc123def", []map[string]any{entry("user", "hello", nil)})
	if got := f.detail("abc123").LastUser; got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestDamagedLinesAreSkipped(t *testing.T) {
	f := nativeInspectFixture(t)
	path := f.write("abc123", []map[string]any{entry("user", "hello", nil)})
	content, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append([]byte("{broken\n"), content...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.detail("abc123").LastUser; got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestClarpAnswersBeforeTheNativeTranscript(t *testing.T) {
	f := inspectFixture(t)
	if err := os.MkdirAll(filepath.Join(f.svc.ProjectsDir, "-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := &nativeFixture{t: t, projects: f.svc.ProjectsDir, svc: f.svc}
	native.write("domi", []map[string]any{entry("user", "native hello", nil)})
	f.message(1, "user", "clarp hello")
	if got := f.detail("domi"); got.Kind != "clarp" || got.LastUser != "clarp hello" {
		t.Fatalf("got %+v", got)
	}
	if got := f.detail("dom"); got.Kind != "native" || got.LastUser != "native hello" {
		t.Fatalf("a non-slug falls through to the transcript prefix match: %+v", got)
	}
}
