package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTranscript writes entries as JSONL the way the Python fixtures do:
// json.dumps per entry, joined by newlines, no trailing newline.
func writeTranscript(t *testing.T, dir, name string, entries ...any) string {
	t.Helper()
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		encoded, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	path := filepath.Join(dir, name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
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

func prepend(t *testing.T, path, prefix string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte(prefix), body...), 0o600); err != nil {
		t.Fatal(err)
	}
}

type nativeFixture struct {
	projects string
	dir      string
}

func newNativeFixture(t *testing.T, project string) nativeFixture {
	t.Helper()
	projects := filepath.Join(t.TempDir(), "projects")
	dir := filepath.Join(projects, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return nativeFixture{projects: projects, dir: dir}
}

func (f nativeFixture) stopped(windowDays int) []StoppedItem {
	return StoppedNativeSessions(f.projects, windowDays, time.Now())
}

func TestATranscriptEndingOnALimitIsListed(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", map[string]any{"type": "user", "message": map[string]any{"content": "hi"}}, limitError())
	found := f.stopped(7)
	if len(found) != 1 || found[0].ID != "s1" {
		t.Fatalf("found %+v", found)
	}
	item := found[0]
	if item.Kind != "native" || item.Cause != "usage_limit" || item.Pending != 0 || item.Name != "s1" || item.Backend != "claude" || item.StoppedAt == 0 {
		t.Fatalf("item shape %+v", item)
	}
}

func TestALimitThatWasWorkedThroughIsHistory(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", limitError(),
		map[string]any{"type": "user", "message": map[string]any{"content": "retry"}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": "done"}})
	if found := f.stopped(7); len(found) != 0 {
		t.Fatalf("expected nothing, got %+v", found)
	}
}

func TestADifferentApiErrorIsNotAUsageStop(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Request timed out"}}}})
	if found := f.stopped(7); len(found) != 0 {
		t.Fatalf("expected nothing, got %+v", found)
	}
}

func TestTheWorkingDirectoryIsRecovered(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", limitError())
	if got := f.stopped(7)[0].CWD; got != "/home/u" {
		t.Fatalf("cwd %q", got)
	}
}

func TestTheLastReplyIsCarriedForTheListing(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", entryUser("go"),
		map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Pushed the fix."}}}},
		limitError())
	if got := f.stopped(7)[0].LastMessage; got != "Pushed the fix." {
		t.Fatalf("last_message %q", got)
	}
}

func TestTheStoppingErrorIsNotUsedAsTheLastReply(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	writeTranscript(t, f.dir, "s1", entryUser("go"), limitError())
	if got := f.stopped(7)[0].LastMessage; got != "" {
		t.Fatalf("last_message %q", got)
	}
}

func TestTheRecordedWorkingDirectoryWinsOverTheEncodedOne(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	entry := limitError()
	entry["cwd"] = "/actual/place"
	writeTranscript(t, f.dir, "s1", entry)
	if got := f.stopped(7)[0].CWD; got != "/actual/place" {
		t.Fatalf("cwd %q", got)
	}
}

func TestDamagedLinesDoNotBreakTheScan(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	path := writeTranscript(t, f.dir, "s1", limitError())
	prepend(t, path, "{not json\n")
	if found := f.stopped(7); len(found) != 1 {
		t.Fatalf("found %+v", found)
	}
}

func TestOldStopsFallOutsideTheWindow(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	path := writeTranscript(t, f.dir, "s1", limitError())
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if found := f.stopped(7); len(found) != 0 {
		t.Fatalf("expected the window to exclude it, got %+v", found)
	}
	if found := f.stopped(0); len(found) != 1 {
		t.Fatalf("a zero window means no cutoff, got %+v", found)
	}
	if found := StoppedNativeSessions(filepath.Join(f.projects, "missing"), 7, time.Now()); found != nil {
		t.Fatalf("missing projects dir must list nothing, got %+v", found)
	}
}

func TestLastEntriesKeepsATailOfALongTranscript(t *testing.T) {
	dir := t.TempDir()
	var entries []any
	for i := 0; i < 1000; i++ {
		entries = append(entries, map[string]any{"type": "user", "n": i})
	}
	path := writeTranscript(t, dir, "long", entries...)
	tail := LastEntries(path, 40)
	if len(tail) != 40 || tail[39]["n"] != 999.0 || tail[0]["n"] != 960.0 {
		t.Fatalf("tail = %d entries, first %v last %v", len(tail), tail[0]["n"], tail[len(tail)-1]["n"])
	}
	if LastEntries(filepath.Join(dir, "missing.jsonl"), 40) != nil {
		t.Fatal("unreadable transcript must yield no entries")
	}
}

func TestDecodeProjectDir(t *testing.T) {
	if got := DecodeProjectDir("-a-b-c"); got != "/a/b/c" {
		t.Fatalf("got %q", got)
	}
	if got := DecodeProjectDir("--home-u"); got != "/home/u" {
		t.Fatalf("got %q", got)
	}
}

func TestPriorityOrdersBlockedWorkFirstAndIdlePausesLast(t *testing.T) {
	items := []StoppedItem{
		{Name: "idle-pause", Cause: "queue_paused", StoppedAt: 50},
		{Name: "old-native", Cause: "usage_limit", StoppedAt: 10},
		{Name: "pending", Cause: "queue_paused", Pending: 2, StoppedAt: 1},
		{Name: "new-native", Cause: "usage_limit", StoppedAt: 20},
	}
	SortByPriority(items)
	var names []string
	for _, item := range items {
		names = append(names, item.Name)
	}
	if got := strings.Join(names, ","); got != "pending,new-native,old-native,idle-pause" {
		t.Fatalf("order %s", got)
	}
}

func TestFamilyAndServableBy(t *testing.T) {
	if Family("claude-fable-5-1") != "fable" || Family("Claude-Opus-4") != "opus" || Family("gpt-5") != "" {
		t.Fatal("family detection")
	}
	accounts := []AccountQuota{
		{Alias: "a", Usage: &Quota{}},
		{Alias: "b", Usage: &Quota{Limited: true}},
		{Alias: "c", Usage: &Quota{BlockedModels: []string{"claude-opus-4"}}},
		{Alias: "", Usage: &Quota{}},
		{Alias: "d"},
	}
	if got := ServableBy("claude-opus-4-1", accounts); strings.Join(got, ",") != "a" {
		t.Fatalf("opus servable by %v", got)
	}
	if got := ServableBy("claude-sonnet-4", accounts); strings.Join(got, ",") != "a,c" {
		t.Fatalf("sonnet servable by %v", got)
	}
	if got := ServableBy("", accounts); strings.Join(got, ",") != "a,c" {
		t.Fatalf("unknown model servable by %v", got)
	}
}

func TestStarvingModelsNamesTheParkedAgents(t *testing.T) {
	items := []StoppedItem{
		{Name: "one", Cause: "waiting_for_account", Model: "claude-opus-4"},
		{Name: "two", Cause: "waiting_for_account", Model: "claude-opus-4"},
		{Name: "three", Cause: "waiting_for_account", Model: "claude-sonnet-4"},
		{Name: "four", Cause: "waiting_for_account"},
		{Name: "five", Cause: "usage_limit", Model: "claude-opus-4"},
	}
	accounts := []AccountQuota{{Alias: "a", Usage: &Quota{BlockedModels: []string{"claude-opus-4"}}}}
	got := StarvingModels(items, accounts)
	if len(got) != 1 || strings.Join(got["claude-opus-4"], ",") != "one,two" {
		t.Fatalf("starving %v", got)
	}
	got = StarvingModels(items, nil)
	if strings.Join(got["unknown"], ",") != "four" || len(got) != 3 {
		t.Fatalf("starving with no accounts %v", got)
	}
}

func TestReadinessKeepsHardBlocksAndWarningsApart(t *testing.T) {
	accounts := []AccountQuota{
		{Alias: "work", Usage: &Quota{BlockedModels: []string{"claude-opus-4"}}},
		{Alias: "limited", Usage: &Quota{Limited: true}},
		{Alias: "unknown"},
	}
	cases := []struct {
		name    string
		item    StoppedItem
		def     string
		ready   bool
		hard    string
		warning string
	}{
		{"parked servable", StoppedItem{Cause: "waiting_for_account", Model: "claude-sonnet-4", Backend: "claude"}, "work", true, "", "parked; work could serve it"},
		{"parked starving", StoppedItem{Cause: "waiting_for_account", Model: "claude-opus-4", Backend: "claude"}, "work", false, "no account can serve claude-opus-4", ""},
		{"parked no model", StoppedItem{Cause: "waiting_for_account", Backend: "claude"}, "", true, "", "parked; work could serve it"},
		{"paused with pending", StoppedItem{Cause: "queue_paused", Pending: 3, Backend: "claude"}, "work", false, "queue is paused, 3 turn(s) already waiting; a prompt would only queue", ""},
		{"paused idle", StoppedItem{Cause: "queue_paused", Backend: "claude"}, "work", false, "queue is paused; a prompt would only queue", ""},
		{"codex", StoppedItem{Cause: "usage_limit", Backend: "codex"}, "work", true, "", "OpenAI quota is not visible from here"},
		{"missing account", StoppedItem{Backend: "claude", Account: "nope"}, "work", false, "account nope is not saved here", ""},
		{"no default", StoppedItem{Backend: "claude"}, "", false, "account unknown is not saved here", ""},
		{"unknown quota", StoppedItem{Backend: "claude"}, "unknown", false, "unknown quota is unknown", ""},
		{"rate limited", StoppedItem{Backend: "claude"}, "limited", false, "limited is still rate limited", ""},
		{"blocked family warns", StoppedItem{Backend: "claude"}, "work", true, "", "work has no claude-opus-4 quota left"},
		{"clean", StoppedItem{Backend: "claude"}, "work", true, "", ""},
	}
	for _, c := range cases {
		accs := accounts
		if c.name == "clean" {
			accs = []AccountQuota{{Alias: "work", Usage: &Quota{}}}
		}
		got := ReadinessOf(c.item, accs, c.def)
		if got.Ready != c.ready || deref(got.Hard) != c.hard || deref(got.Warning) != c.warning {
			t.Errorf("%s: got ready=%v hard=%q warning=%q", c.name, got.Ready, deref(got.Hard), deref(got.Warning))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestReadinessJSONEmitsNullForAbsentReasons(t *testing.T) {
	encoded, err := json.Marshal(ReadinessOf(StoppedItem{Backend: "claude"}, []AccountQuota{{Alias: "w", Usage: &Quota{}}}, "w"))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"ready":true,"hard":null,"warning":null}` {
		t.Fatalf("json %s", encoded)
	}
}

// Ported from tests/test_regressions.py: a weekly exhaustion or a lock is a
// rate limit, and a rate-limited account blocks resuming.
func TestWeeklyExhaustionAndLockBlockResume(t *testing.T) {
	for _, quota := range []*Quota{{Limited: true}, {Limited: true, BlockedModels: nil}} {
		state := ReadinessOf(StoppedItem{Backend: "claude"}, []AccountQuota{{Alias: "work", Usage: quota}}, "work")
		if state.Ready {
			t.Fatalf("limited account must block: %+v", state)
		}
	}
}

func TestStoppedItemJSONShape(t *testing.T) {
	encoded, err := json.Marshal(StoppedItem{Kind: "native", Cause: "usage_limit", ID: "s1", Name: "s1", Backend: "claude", CWD: "/x", StoppedAt: 1.5})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"native","cause":"usage_limit","pending":0,"id":"s1","name":"s1","backend":"claude","cwd":"/x","last_message":"","stopped_at":1.5}`
	if string(encoded) != want {
		t.Fatalf("json %s", encoded)
	}
}

func TestContinueItemReturnsTheResumeCommand(t *testing.T) {
	got, err := ContinueItem(StoppedItem{Kind: "native", ID: "abc", CWD: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "claude --resume abc" || got.CWD != "/w" || got.Continued || got.Kind != "native" || got.ID != "abc" {
		t.Fatalf("continuation %+v", got)
	}
	if _, err := ContinueItem(StoppedItem{Kind: "clarp", ID: "x"}); err == nil || err.Error() != "This source requires its plugin" {
		t.Fatalf("non-native must be refused, got %v", err)
	}
	var re *ResumeError
	if _, err := ContinueItem(StoppedItem{}); !errorsAs(err, &re) {
		t.Fatalf("expected ResumeError, got %T", err)
	}
}

func TestServiceStoppedOrdersByPriority(t *testing.T) {
	f := newNativeFixture(t, "-home-u")
	older := writeTranscript(t, f.dir, "older", limitError())
	writeTranscript(t, f.dir, "newer", limitError())
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	s := &Service{ProjectsDir: f.projects}
	items := s.Stopped(DefaultWindowDays, true)
	if len(items) != 2 || items[0].ID != "newer" || items[1].ID != "older" {
		t.Fatalf("items %+v", items)
	}
}

func TestDefaultProjectsDirHonoursConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	if got := DefaultProjectsDir(); got != "/custom/claude/projects" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, _ := os.UserHomeDir()
	if got := DefaultProjectsDir(); got != filepath.Join(home, ".claude", "projects") {
		t.Fatalf("got %q", got)
	}
}
