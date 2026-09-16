package clarp

// Finding and continuing work that a usage limit stopped. Ported from
// plugins/clarp/tests/test_resume.py and test_resume_regression.py. No test
// sends a real prompt or reads the real Clarp database.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/work"
)

var now = time.Now()

func nowSeconds() float64 { return float64(now.UnixNano()) / 1e9 }

func account(alias string, limited bool, blocked []string, usage bool) work.AccountQuota {
	a := work.AccountQuota{Alias: alias}
	if usage {
		a.Usage = &work.Quota{Limited: limited, BlockedModels: blocked}
	}
	return a
}

func healthy() work.AccountQuota { return account("work", false, nil, true) }

// fixture is a writable handle on a throwaway Clarp state database.
type fixture struct {
	t    *testing.T
	path string
	db   *sql.DB
	svc  *Service
}

func newFixture(t *testing.T, schema []string) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return &fixture{t: t, path: path, db: db, svc: &Service{
		StatePath:   path,
		ProjectsDir: filepath.Join(t.TempDir(), "projects"),
		ProcRoot:    t.TempDir(),
		Now:         func() time.Time { return now },
	}}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

var resumeSchema = []string{
	`CREATE TABLE agents (agent_id TEXT, persona TEXT, backend TEXT,
                                 cwd TEXT, session TEXT, deleted_at INT,
                                 archived_at INT, model TEXT)`,
	`CREATE TABLE state_log (state_id INTEGER PRIMARY KEY, agent_id TEXT,
                                    ts INT, kind TEXT, detail TEXT)`,
	`CREATE TABLE queue_state_revisions (agent_id TEXT, revision INT, paused INT)`,
	`CREATE TABLE queued_turns (queue_id TEXT, agent_id TEXT, status TEXT,
                                       enqueued_at INT, text TEXT)`,
	`CREATE TABLE messages (agent_id TEXT, seq INT, role TEXT, text TEXT)`,
}

func resumeFixture(t *testing.T) *fixture { return newFixture(t, resumeSchema) }

func (f *fixture) paused(agentID string, paused int) {
	f.exec("INSERT INTO queue_state_revisions VALUES (?,?,?)", agentID, 1, paused)
}

func (f *fixture) agent(agentID, session, backend string, deleted, archived any) {
	f.exec("INSERT INTO agents VALUES (?,?,?,?,?,?,?,?)",
		agentID, strings.ToUpper(session[:1])+session[1:], backend, "/tmp", session, deleted, archived, nil)
}

func (f *fixture) event(agentID, kind string, ts float64, reason string) {
	detail := "{}"
	if reason != "" {
		encoded, _ := json.Marshal(map[string]string{"reason": reason})
		detail = string(encoded)
	}
	f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
		agentID, int64(ts*1000), kind, detail)
}

func ids(items []work.StoppedItem) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// items, cont and snap unwrap a (value, error) result, failing the test on an
// error. They are curried so a multi-value call can be passed straight in.
func items(t *testing.T) func([]work.StoppedItem, error) []work.StoppedItem {
	return func(found []work.StoppedItem, err error) []work.StoppedItem {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
}

func cont(t *testing.T) func(work.Continuation, error) work.Continuation {
	return func(result work.Continuation, err error) work.Continuation {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
}

func snap(t *testing.T) func(Snapshot, error) Snapshot {
	return func(result Snapshot, err error) Snapshot {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
}

// --- readiness (work.ReadinessOf, exercised with the plugin's fixtures) --------

func clarpItem(backend string) work.StoppedItem {
	return work.StoppedItem{Kind: "clarp", ID: "a", Name: "A", Backend: backend}
}

func TestAHealthyAccountIsReady(t *testing.T) {
	state := work.ReadinessOf(clarpItem("claude"), []work.AccountQuota{healthy()}, "work")
	if !state.Ready || state.Hard != nil || state.Warning != nil {
		t.Fatalf("got %+v", state)
	}
}

func TestARateLimitedAccountIsAHardBlock(t *testing.T) {
	// Continuing here would reproduce the failure that caused the stop.
	state := work.ReadinessOf(clarpItem("claude"), []work.AccountQuota{account("work", true, nil, true)}, "work")
	if state.Ready || state.Hard == nil || !strings.Contains(*state.Hard, "rate limited") {
		t.Fatalf("got %+v", state)
	}
}

func TestASpentModelLimitWarnsButDoesNotBlock(t *testing.T) {
	// The resumed work may not use that model at all.
	state := work.ReadinessOf(clarpItem("claude"), []work.AccountQuota{account("work", false, []string{"Fable"}, true)}, "work")
	if !state.Ready || state.Hard != nil || state.Warning == nil || !strings.Contains(*state.Warning, "Fable") {
		t.Fatalf("got %+v", state)
	}
}

func TestUnknownQuotaBlocksRatherThanGuessing(t *testing.T) {
	state := work.ReadinessOf(clarpItem("claude"), []work.AccountQuota{account("work", false, nil, false)}, "work")
	if state.Ready {
		t.Fatalf("got %+v", state)
	}
}

func TestAMissingAccountBlocks(t *testing.T) {
	state := work.ReadinessOf(clarpItem("claude"), nil, "work")
	if state.Ready || state.Hard == nil || !strings.Contains(*state.Hard, "not saved") {
		t.Fatalf("got %+v", state)
	}
}

func TestCodexIsAllowedButSaysItsQuotaIsInvisible(t *testing.T) {
	state := work.ReadinessOf(clarpItem("codex"), []work.AccountQuota{account("work", true, nil, true)}, "work")
	if !state.Ready {
		t.Fatal("a Claude limit must not gate an OpenAI agent")
	}
	if state.Warning == nil || !strings.Contains(*state.Warning, "OpenAI") {
		t.Fatalf("got %+v", state)
	}
}

// --- clarp detection ------------------------------------------------------------

func TestPauseOverridesUsageStopAndPreventsPrompt(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-30, "usage_limit")
	f.paused("a1", 1)
	items := items(t)(f.svc.Stopped(context.Background(), work.DefaultWindowDays, true))
	if len(items) != 1 || items[0].Cause != "queue_paused" {
		t.Fatalf("got %+v", items)
	}
	if work.ReadinessOf(items[0], []work.AccountQuota{healthy()}, "work").Ready {
		t.Fatal("a paused agent must not be ready")
	}
	var calls [][]string
	f.svc.Run = recording(&calls, completed("", 0, ""))
	_, err := f.svc.Continue(context.Background(), items[0], "")
	var resumeError *work.ResumeError
	if !errors.As(err, &resumeError) {
		t.Fatalf("got %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("nothing may be run: %v", calls)
	}
}

func TestAnAgentStoppedByUsageIsListed(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-3600, "usage_limit")
	found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7))
	if got := ids(found); !reflect.DeepEqual(got, []string{"adam"}) {
		t.Fatalf("got %v", got)
	}
	item := found[0]
	if item.Kind != "clarp" || item.Cause != "usage_limit" || item.Name != "Adam" || item.Backend != "claude" ||
		item.CWD != "/tmp" || item.Pending != 0 || item.StoppedAt < nowSeconds()-3601 || item.StoppedAt > nowSeconds()-3599 {
		t.Fatalf("got %+v", item)
	}
}

func TestAnErrorStopIsListedToo(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "error", nowSeconds()-3600, "usage_limit")
	if got := ids(items(t)(f.svc.StoppedClarpAgents(context.Background(), 7))); !reflect.DeepEqual(got, []string{"adam"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAnAgentThatRanSinceIsNotListed(t *testing.T) {
	// It recovered on its own; continuing it would duplicate work.
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-7200, "usage_limit")
	f.event("a1", "done", nowSeconds()-60, "")
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestAStopForAnotherReasonIsNotListed(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-600, "user_stop")
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestDeletedAndArchivedAgentsAreSkipped(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "gone", "claude", 1, nil)
	f.agent("a2", "shelved", "claude", nil, 1)
	for _, agentID := range []string{"a1", "a2"} {
		f.event(agentID, "interrupted", nowSeconds()-600, "usage_limit")
	}
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestTheWindowExcludesOldStops(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-30*86400, "usage_limit")
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 60)); len(found) != 1 {
		t.Fatalf("got %+v", found)
	}
	if found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 0)); len(found) != 1 {
		t.Fatalf("no window means no cutoff: %+v", found)
	}
}

func TestTheLastThingTheAgentSaidIsCarried(t *testing.T) {
	// The listing shows it, so detection has to collect it.
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-600, "usage_limit")
	f.exec("INSERT INTO messages VALUES ('a1', 1, 'assistant', '<speak>Pushed the fix.</speak>')")
	found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7))
	if found[0].LastMessage != "Pushed the fix." {
		t.Fatalf("speech markup must not reach the listing: %q", found[0].LastMessage)
	}
}

func TestAMissingDatabaseIsNotAnError(t *testing.T) {
	f := resumeFixture(t)
	f.db.Close()
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if found := items(t)(f.svc.StoppedClarpAgents(ctx, 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
	if found := items(t)(f.svc.ParkedClarpAgents(ctx, 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
	if found := items(t)(f.svc.PausedClarpAgents(ctx)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestAnUnreadableDatabaseIsAResumeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{StatePath: path}
	for name, call := range map[string]func() error{
		"stopped": func() error { _, err := s.StoppedClarpAgents(context.Background(), 7); return err },
		"parked":  func() error { _, err := s.ParkedClarpAgents(context.Background(), 7); return err },
		"paused":  func() error { _, err := s.PausedClarpAgents(context.Background()); return err },
	} {
		err := call()
		var resumeError *work.ResumeError
		if !errors.As(err, &resumeError) || !strings.HasPrefix(err.Error(), "could not read Clarp") {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestAParkedAgentIsFoundWithItsModel(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
		"a1", int64(nowSeconds()*1000), "thinking", `{"account_recovery": "waiting", "dispatch": "claude"}`)
	f.exec("UPDATE agents SET model='claude-fable-5-1'")
	found := items(t)(f.svc.ParkedClarpAgents(context.Background(), 7))
	if got := ids(found); !reflect.DeepEqual(got, []string{"domi"}) {
		t.Fatalf("got %v", got)
	}
	if found[0].Model != "claude-fable-5-1" || found[0].Cause != "waiting_for_account" || found[0].Pending != 0 {
		t.Fatalf("got %+v", found[0])
	}
}

func TestAParkedAgentThatHasSinceRunIsNotListed(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
		"a1", int64((nowSeconds()-600)*1000), "thinking", `{"account_recovery": "waiting"}`)
	f.event("a1", "done", nowSeconds(), "")
	if found := items(t)(f.svc.ParkedClarpAgents(context.Background(), 7)); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestAPausedAgentIsFound(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.paused("a1", 1)
	found := items(t)(f.svc.PausedClarpAgents(context.Background()))
	if got := ids(found); !reflect.DeepEqual(got, []string{"domi"}) {
		t.Fatalf("got %v", got)
	}
	if found[0].Cause != "queue_paused" || found[0].StoppedAt != 0 {
		t.Fatalf("got %+v", found[0])
	}
}

func TestAnUnpausedAgentIsNotFound(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.paused("a1", 0)
	if found := items(t)(f.svc.PausedClarpAgents(context.Background())); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestPendingTurnsBehindThePauseAreCounted(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.paused("a1", 1)
	f.exec("INSERT INTO queued_turns VALUES ('q1','a1','queued',?, 'work')", int64(nowSeconds()*1000))
	found := items(t)(f.svc.PausedClarpAgents(context.Background()))
	if found[0].Pending != 1 {
		t.Fatalf("got %+v", found[0])
	}
	if found[0].StoppedAt < nowSeconds()-1 || found[0].StoppedAt > nowSeconds()+1 {
		t.Fatalf("stopped_at is the oldest queued turn: %v", found[0].StoppedAt)
	}
}

func TestAUsageStopIsNotDuplicatedAsAPausedRow(t *testing.T) {
	// An agent in both states appears once, under the cause found first.
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.paused("a1", 1)
	f.event("a1", "interrupted", nowSeconds()-600, "usage_limit")
	var matched []work.StoppedItem
	for _, item := range items(t)(f.svc.Stopped(context.Background(), 7, true)) {
		if item.ID == "domi" {
			matched = append(matched, item)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("got %+v", matched)
	}
}

func TestAParkedAgentAlreadyStoppedIsNotDuplicated(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-600, "usage_limit")
	f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
		"a1", int64((nowSeconds()-300)*1000), "thinking", `{"account_recovery": "waiting"}`)
	items := items(t)(f.svc.Stopped(context.Background(), 7, true))
	if len(items) != 1 || items[0].Cause != "usage_limit" {
		t.Fatalf("the usage stop found first wins: %+v", items)
	}
}

func TestExcludingPausedKeepsTheUsageStop(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "domi", "claude", nil, nil)
	f.paused("a1", 1)
	f.event("a1", "interrupted", nowSeconds()-600, "usage_limit")
	items := items(t)(f.svc.Stopped(context.Background(), 7, false))
	if len(items) != 1 || items[0].Cause != "usage_limit" {
		t.Fatalf("got %+v", items)
	}
}

func TestStoppedItemJSONShape(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", 1_700_000_000, "usage_limit")
	items := items(t)(f.svc.Stopped(context.Background(), 0, true))
	encoded := mustJSON(t, items[0])
	want := `{"kind":"clarp","cause":"usage_limit","pending":0,"id":"adam","name":"Adam","backend":"claude","cwd":"/tmp","last_message":"","stopped_at":1700000000}`
	if encoded != want {
		t.Fatalf("got %s\nwant %s", encoded, want)
	}
}

// --- native detection through the merge ------------------------------------------

type nativeFixture struct {
	t        *testing.T
	projects string
	svc      *Service
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	if err := os.MkdirAll(filepath.Join(projects, "-home-u"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &nativeFixture{t: t, projects: projects, svc: &Service{
		StatePath:   filepath.Join(home, "absent.sqlite"),
		ProjectsDir: projects,
		ProcRoot:    t.TempDir(),
		Now:         func() time.Time { return now },
	}}
}

func (f *nativeFixture) transcript(name string, entries []map[string]any) string {
	f.t.Helper()
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, mustJSON(f.t, entry))
	}
	path := filepath.Join(f.projects, "-home-u", name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func entryUser(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}

func limitError() map[string]any {
	return map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"content": []any{map[string]any{"type": "text",
			"text": "You've hit your session limit · resets 6pm"}}}}
}

func (f *nativeFixture) stopped() []work.StoppedItem {
	f.t.Helper()
	return items(f.t)(f.svc.Stopped(context.Background(), 7, true))
}

func TestATranscriptEndingOnALimitIsListed(t *testing.T) {
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{{"type": "user", "message": map[string]any{"content": "hi"}}, limitError()})
	found := f.stopped()
	if got := ids(found); !reflect.DeepEqual(got, []string{"s1"}) {
		t.Fatalf("got %v", got)
	}
	if found[0].Kind != "native" || found[0].Cause != "usage_limit" || found[0].Backend != "claude" {
		t.Fatalf("got %+v", found[0])
	}
}

func TestALimitThatWasWorkedThroughIsHistory(t *testing.T) {
	// An earlier limit followed by real work is not waiting to be continued.
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{limitError(),
		{"type": "user", "message": map[string]any{"content": "retry"}},
		{"type": "assistant", "message": map[string]any{"content": "done"}}})
	if found := f.stopped(); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestADifferentAPIErrorIsNotAUsageStop(t *testing.T) {
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Request timed out"}}}}})
	if found := f.stopped(); len(found) != 0 {
		t.Fatalf("got %+v", found)
	}
}

func TestTheWorkingDirectoryIsRecovered(t *testing.T) {
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{limitError()})
	if got := f.stopped()[0].CWD; got != "/home/u" {
		t.Fatalf("got %q", got)
	}
}

func TestTheLastReplyIsCarriedForTheListing(t *testing.T) {
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{entryUser("go"),
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Pushed the fix."}}}},
		limitError()})
	if got := f.stopped()[0].LastMessage; got != "Pushed the fix." {
		t.Fatalf("got %q", got)
	}
}

func TestTheStoppingErrorIsNotUsedAsTheLastReply(t *testing.T) {
	// Quoting the limit message back would say nothing about the work.
	f := newNativeFixture(t)
	f.transcript("s1", []map[string]any{entryUser("go"), limitError()})
	if got := f.stopped()[0].LastMessage; got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTheRecordedWorkingDirectoryWinsOverTheEncodedOne(t *testing.T) {
	f := newNativeFixture(t)
	entry := limitError()
	entry["cwd"] = "/actual/place"
	f.transcript("s1", []map[string]any{entry})
	if got := f.stopped()[0].CWD; got != "/actual/place" {
		t.Fatalf("got %q", got)
	}
}

func TestDamagedLinesDoNotBreakTheScan(t *testing.T) {
	f := newNativeFixture(t)
	path := f.transcript("s1", []map[string]any{limitError()})
	content, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append([]byte("{not json\n"), content...), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := f.stopped(); len(found) != 1 {
		t.Fatalf("got %+v", found)
	}
}

func TestNativeAndClarpItemsMergeInPriorityOrder(t *testing.T) {
	f := resumeFixture(t)
	if err := os.MkdirAll(filepath.Join(f.svc.ProjectsDir, "-home-u"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := &nativeFixture{t: t, projects: f.svc.ProjectsDir, svc: f.svc}
	native.transcript("s1", []map[string]any{limitError()})
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()+60, "usage_limit")
	f.agent("a2", "domi", "claude", nil, nil)
	f.paused("a2", 1)
	f.exec("INSERT INTO queued_turns VALUES ('q1','a2','queued',?, 'work')", int64((nowSeconds()-86400)*1000))
	f.agent("a3", "ea5a", "claude", nil, nil)
	f.paused("a3", 1)
	items := items(t)(f.svc.Stopped(context.Background(), 7, true))
	// Blocked queued work first, then the usage stops by recency (adam's stop is
	// newer than the transcript's mtime), then the idle pause last.
	if got := ids(items); !reflect.DeepEqual(got, []string{"domi", "adam", "s1", "ea5a"}) {
		t.Fatalf("got %v", got)
	}
}

// --- paused queue ---------------------------------------------------------------

var pausedItem = work.StoppedItem{Kind: "clarp", ID: "domi", Name: "Domi", Backend: "claude",
	Cause: "queue_paused", Pending: 1}

func TestAPausedAgentIsNeverReady(t *testing.T) {
	state := work.ReadinessOf(pausedItem, []work.AccountQuota{healthy()}, "work")
	if state.Ready || state.Hard == nil || !strings.Contains(*state.Hard, "paused") {
		t.Fatalf("got %+v", state)
	}
}

func TestTheBlockNamesTheWorkStuckBehindIt(t *testing.T) {
	state := work.ReadinessOf(pausedItem, []work.AccountQuota{healthy()}, "work")
	if state.Hard == nil || !strings.Contains(*state.Hard, "1 turn") {
		t.Fatalf("got %+v", state)
	}
}

func TestAHealthyAccountDoesNotUnblockAPausedQueue(t *testing.T) {
	// Quota is irrelevant here; the pause is what stops it.
	if work.ReadinessOf(pausedItem, []work.AccountQuota{healthy()}, "work").Ready {
		t.Fatal("paused queue reported ready")
	}
}

func TestContinuingAPausedAgentRefusesRatherThanQueueing(t *testing.T) {
	var calls [][]string
	s := &Service{Run: recording(&calls, completed("", 0, ""))}
	_, err := s.Continue(context.Background(), pausedItem, "")
	if len(calls) != 0 {
		t.Fatal("a prompt here would silently queue")
	}
	var resumeError *work.ResumeError
	if !errors.As(err, &resumeError) || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("got %v", err)
	}
}

func TestTheRefusalSaysHowToClearIt(t *testing.T) {
	_, err := (&Service{}).Continue(context.Background(), pausedItem, "")
	if err == nil || !strings.Contains(err.Error(), "Clarp app") {
		t.Fatalf("got %v", err)
	}
}

// --- starvation -----------------------------------------------------------------

var (
	healthyAccount = work.AccountQuota{Alias: "work", Usage: &work.Quota{}}
	noFable        = work.AccountQuota{Alias: "spent", Usage: &work.Quota{BlockedModels: []string{"Fable"}}}
	limitedAccount = work.AccountQuota{Alias: "out", Usage: &work.Quota{Limited: true}}
)

func parked(name, model string) work.StoppedItem {
	return work.StoppedItem{Kind: "clarp", ID: name, Name: name, Backend: "claude",
		Cause: "waiting_for_account", Model: model}
}

func TestAHealthyAccountCanServeAnything(t *testing.T) {
	if got := work.ServableBy("claude-opus-5", []work.AccountQuota{healthyAccount}); !reflect.DeepEqual(got, []string{"work"}) {
		t.Fatalf("got %v", got)
	}
}

func TestARateLimitedAccountCanServeNothing(t *testing.T) {
	if got := work.ServableBy("claude-opus-5", []work.AccountQuota{limitedAccount}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestASpentFamilyLimitRulesOutOnlyThatFamily(t *testing.T) {
	// The exact case seen in practice: Opus fine, Fable gone, same account.
	if got := work.ServableBy("claude-opus-5", []work.AccountQuota{noFable}); !reflect.DeepEqual(got, []string{"spent"}) {
		t.Fatalf("got %v", got)
	}
	if got := work.ServableBy("claude-fable-5-1", []work.AccountQuota{noFable}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestADatedOrSuffixedModelIDStillMatchesItsFamily(t *testing.T) {
	if got := work.ServableBy("claude-fable-5-1[1m]", []work.AccountQuota{noFable}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestTheUnservableModelIsNamedWithWhoWantsIt(t *testing.T) {
	items := []work.StoppedItem{parked("Sindre", "claude-fable-5-1"), parked("Domi", "claude-fable-5-1"), parked("Ea5a", "claude-opus-5")}
	starving := work.StarvingModels(items, []work.AccountQuota{noFable})
	if want := map[string][]string{"claude-fable-5-1": {"Sindre", "Domi"}}; !reflect.DeepEqual(starving, want) {
		t.Fatalf("got %v", starving)
	}
}

func TestNothingStarvesWhenEveryModelIsServable(t *testing.T) {
	if starving := work.StarvingModels([]work.StoppedItem{parked("Ea5a", "claude-opus-5")}, []work.AccountQuota{healthyAccount}); len(starving) != 0 {
		t.Fatalf("got %v", starving)
	}
}

func TestAParkedAgentWhoseModelIsAvailableIsReady(t *testing.T) {
	state := work.ReadinessOf(parked("Ea5a", "claude-opus-5"), []work.AccountQuota{noFable}, "")
	if !state.Ready || state.Warning == nil || !strings.Contains(*state.Warning, "spent") {
		t.Fatalf("got %+v", state)
	}
}

func TestAParkedAgentWithNoServableAccountIsBlocked(t *testing.T) {
	state := work.ReadinessOf(parked("Domi", "claude-fable-5-1"), []work.AccountQuota{noFable}, "")
	if state.Ready || state.Hard == nil || !strings.Contains(*state.Hard, "claude-fable-5-1") {
		t.Fatalf("got %+v", state)
	}
}

func TestOnlyParkedAgentsAreConsidered(t *testing.T) {
	usageStopped := work.StoppedItem{Kind: "clarp", ID: "x", Name: "X", Backend: "claude", Cause: "usage_limit", Model: "claude-fable-5-1"}
	if starving := work.StarvingModels([]work.StoppedItem{usageStopped}, []work.AccountQuota{noFable}); len(starving) != 0 {
		t.Fatalf("got %v", starving)
	}
}

// --- priority -------------------------------------------------------------------

func priorityItem(name, cause string, pending int, when float64) work.StoppedItem {
	return work.StoppedItem{Kind: "clarp", ID: name, Name: name, Backend: "claude", Cause: cause, Pending: pending, StoppedAt: when}
}

func order(items []work.StoppedItem) []string {
	work.SortByPriority(items)
	names := []string{}
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

func TestBlockedWorkOutranksAMoreRecentStop(t *testing.T) {
	// Someone is waiting on queued work; recency does not outrank that.
	got := order([]work.StoppedItem{priorityItem("recent", "usage_limit", 0, nowSeconds()),
		priorityItem("blocked", "queue_paused", 1, nowSeconds()-86400*7)})
	if got[0] != "blocked" {
		t.Fatalf("got %v", got)
	}
}

func TestAnEmptyPausedQueueSortsLast(t *testing.T) {
	// Leftover state, not stalled work.
	got := order([]work.StoppedItem{priorityItem("leftover", "queue_paused", 0, nowSeconds()),
		priorityItem("usage", "usage_limit", 0, nowSeconds()-86400)})
	if got[len(got)-1] != "leftover" {
		t.Fatalf("got %v", got)
	}
}

func TestRecencyBreaksTiesWithinAGroup(t *testing.T) {
	got := order([]work.StoppedItem{priorityItem("older", "usage_limit", 0, nowSeconds()-600),
		priorityItem("newer", "usage_limit", 0, nowSeconds())})
	if !reflect.DeepEqual(got, []string{"newer", "older"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAMissingTimestampDoesNotRaise(t *testing.T) {
	got := order([]work.StoppedItem{priorityItem("nostamp", "queue_paused", 0, 0)})
	if !reflect.DeepEqual(got, []string{"nostamp"}) {
		t.Fatalf("got %v", got)
	}
}

// --- continuing -----------------------------------------------------------------

var clarpAdam = work.StoppedItem{Kind: "clarp", ID: "adam", Name: "Adam", Backend: "claude"}

func TestAClarpAgentIsPrompted(t *testing.T) {
	var calls [][]string
	s := &Service{Run: recording(&calls, completed("", 0, ""))}
	result := cont(t)(s.Continue(context.Background(), clarpAdam, ""))
	if !result.Continued || result.Kind != "clarp" || result.ID != "adam" {
		t.Fatalf("got %+v", result)
	}
	want := []string{Admin, "prompt", "--to", "adam", "--text", work.ResumePrompt, "--origin", "automation"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("got %v\nwant %v", calls, want)
	}
}

func TestACustomPromptIsPassedThrough(t *testing.T) {
	var calls [][]string
	s := &Service{Run: recording(&calls, completed("", 0, ""))}
	cont(t)(s.Continue(context.Background(), clarpAdam, "carry on"))
	if calls[0][5] != "carry on" {
		t.Fatalf("got %v", calls[0])
	}
}

func TestThePromptWarnsAgainstRepeatingFinishedWork(t *testing.T) {
	if !strings.Contains(work.ResumePrompt, "do not repeat work that already completed") {
		t.Fatal(work.ResumePrompt)
	}
}

func TestAFailedPromptIsReported(t *testing.T) {
	s := &Service{Run: completed("", 1, "runtime down")}
	_, err := s.Continue(context.Background(), clarpAdam, "")
	var resumeError *work.ResumeError
	if !errors.As(err, &resumeError) || !strings.Contains(err.Error(), "runtime down") {
		t.Fatalf("got %v", err)
	}
	s = &Service{Run: completed("", 1, "")}
	if _, err := s.Continue(context.Background(), clarpAdam, ""); err == nil || err.Error() != "clarp-admin prompt failed" {
		t.Fatalf("got %v", err)
	}
	s = &Service{Run: func(*exec.Cmd) ([]byte, error) { return nil, errors.New("no binary") }}
	if _, err := s.Continue(context.Background(), clarpAdam, ""); err == nil || err.Error() != "could not prompt Adam: no binary" {
		t.Fatalf("got %v", err)
	}
}

func TestANativeSessionHandsBackACommandRatherThanActing(t *testing.T) {
	// There is no supervisor to prompt, so nothing is run behind the user.
	item := work.StoppedItem{Kind: "native", ID: "uuid-1", Name: "uuid", Backend: "claude", CWD: "/home/u"}
	var calls [][]string
	s := &Service{Run: recording(&calls, completed("", 0, ""))}
	result := cont(t)(s.Continue(context.Background(), item, ""))
	if result.Continued || result.Command != "claude --resume uuid-1" || result.CWD != "/home/u" {
		t.Fatalf("got %+v", result)
	}
	if len(calls) != 0 {
		t.Fatal("nothing may be launched without being asked")
	}
}

func TestJSONGoRunsAndReportsSuccessOrFailure(t *testing.T) {
	// The CLI half of test_resume_regression.py (`resume --go --json`) belongs to
	// cmd/hotseat; this keeps the part the package owns: one Continue per item,
	// with the outcome the CLI turns into {"continued": ...} or {"error": ...}.
	item := work.StoppedItem{Kind: "clarp", ID: "fixture", Name: "Fixture", Backend: "codex", Cause: "usage_limit"}
	for _, failure := range []bool{false, true} {
		var calls [][]string
		answer := completed("", 0, "")
		if failure {
			answer = completed("", 1, "fixture failure")
		}
		s := &Service{Run: recording(&calls, answer)}
		result, err := s.Continue(context.Background(), item, "")
		if len(calls) != 1 {
			t.Fatalf("failure=%v: %d calls", failure, len(calls))
		}
		if failure {
			if err == nil || !strings.Contains(err.Error(), "fixture failure") {
				t.Fatalf("got %v", err)
			}
			continue
		}
		if err != nil || !result.Continued {
			t.Fatalf("got %+v, %v", result, err)
		}
	}
}

// --- snapshot -------------------------------------------------------------------

func TestSnapshotCarriesOverviewAndReadyStoppedAgents(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-600, "usage_limit")
	f.agent("a2", "domi", "claude", nil, nil)
	f.paused("a2", 1)
	if err := os.MkdirAll(filepath.Join(f.svc.ProjectsDir, "-home-u"), 0o700); err != nil {
		t.Fatal(err)
	}
	(&nativeFixture{t: t, projects: f.svc.ProjectsDir, svc: f.svc}).transcript("s1", []map[string]any{limitError()})
	f.svc.Run = func(cmd *exec.Cmd) ([]byte, error) {
		if cmd.Args[0] == "ps" {
			return nil, nil
		}
		return []byte(mustJSON(t, registry)), nil
	}
	snapshot := snap(t)(f.svc.Snapshot(context.Background(), []work.AccountQuota{healthy()}, "work"))
	if snapshot.Agents == nil || snapshot.Agents.Total != 3 {
		t.Fatalf("got %+v", snapshot.Agents)
	}
	if got := ids(snapshot.Stopped); !reflect.DeepEqual(got, []string{"adam", "domi"}) {
		t.Fatalf("native items are the core's, not the plugin's: %v", got)
	}
	if r := snapshot.Stopped[0].Readiness; r == nil || !r.Ready {
		t.Fatalf("adam should be ready: %+v", r)
	}
	if r := snapshot.Stopped[1].Readiness; r == nil || r.Ready {
		t.Fatalf("domi is paused: %+v", r)
	}
	encoded := mustJSON(t, snapshot)
	if !strings.HasPrefix(encoded, `{"agents":{"agents":[`) || !strings.Contains(encoded, `"readiness":{"ready":true,"hard":null,"warning":null}`) {
		t.Fatalf("got %s", encoded)
	}
}

func TestSnapshotSurvivesAMissingRegistry(t *testing.T) {
	f := resumeFixture(t)
	f.svc.Run = completed("", 1, "no clarp-admin")
	snapshot := snap(t)(f.svc.Snapshot(context.Background(), nil, "work"))
	if snapshot.Agents != nil {
		t.Fatalf("got %+v", snapshot.Agents)
	}
	if encoded := mustJSON(t, snapshot); encoded != `{"agents":null,"stopped":[]}` {
		t.Fatalf("got %s", encoded)
	}
}

func TestSnapshotPropagatesStateErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{StatePath: path, Run: completed("[]", 0, "")}
	_, err := s.Snapshot(context.Background(), nil, "work")
	var resumeError *work.ResumeError
	if !errors.As(err, &resumeError) {
		t.Fatalf("got %v", err)
	}
}
