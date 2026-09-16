package work

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func inspectEntry(kind, text string, extra map[string]any) map[string]any {
	entry := map[string]any{"type": kind, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
	for k, v := range extra {
		entry[k] = v
	}
	return entry
}

func TestPromptReplyAndLocationAreRecovered(t *testing.T) {
	f := newNativeFixture(t, "-work")
	writeTranscript(t, f.dir, "abc123",
		inspectEntry("user", "build the report", map[string]any{"cwd": "/work", "gitBranch": "main"}),
		inspectEntry("assistant", "starting now", map[string]any{"cwd": "/work"}))
	detail, err := Inspect(f.projects, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LastUser != "build the report" || detail.LastAssistant != "starting now" || deref(detail.CWD) != "/work" || deref(detail.Branch) != "main" {
		t.Fatalf("detail %+v", detail)
	}
	if detail.Turns != 1 || len(detail.Recent) != 2 || detail.Recent[1].Role != "assistant" || detail.Name != "abc123" || detail.ID != "abc123" {
		t.Fatalf("detail %+v", detail)
	}
}

func TestTheErrorThatStoppedItIsNotQuotedAsItsLastWords(t *testing.T) {
	f := newNativeFixture(t, "-work")
	writeTranscript(t, f.dir, "abc123",
		inspectEntry("user", "go", nil),
		inspectEntry("assistant", "on it", nil),
		map[string]any{"type": "assistant", "isApiErrorMessage": true,
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "You've hit your session limit"}}}})
	detail, err := Inspect(f.projects, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LastAssistant != "on it" {
		t.Fatalf("last_assistant %q", detail.LastAssistant)
	}
}

func TestAShortIdResolves(t *testing.T) {
	f := newNativeFixture(t, "-work")
	writeTranscript(t, f.dir, "abc123def", inspectEntry("user", "hello", nil))
	detail, err := Inspect(f.projects, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LastUser != "hello" || detail.ID != "abc123def" || detail.Name != "abc123de" {
		t.Fatalf("detail %+v", detail)
	}
}

func TestDamagedLinesAreSkipped(t *testing.T) {
	f := newNativeFixture(t, "-work")
	path := writeTranscript(t, f.dir, "abc123", inspectEntry("user", "hello", nil))
	prepend(t, path, "{broken\n")
	detail, err := Inspect(f.projects, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if detail.LastUser != "hello" {
		t.Fatalf("last_user %q", detail.LastUser)
	}
}

func TestNothingFoundIsAnInspectError(t *testing.T) {
	f := newNativeFixture(t, "-work")
	_, err := Inspect(f.projects, "missing")
	var ie *InspectError
	if !errors.As(err, &ie) || err.Error() != "nothing found for 'missing'" {
		t.Fatalf("err %v", err)
	}
	if found, err := NativeDetail(f.projects, "missing"); found != nil || err != nil {
		t.Fatalf("NativeDetail must return nil, nil: %v %v", found, err)
	}
	writeTranscript(t, f.dir, "empty")
	if found, err := NativeDetail(f.projects, "empty"); found != nil || err != nil {
		t.Fatalf("an empty transcript is nothing: %v %v", found, err)
	}
}

func TestSummaryModelRecentAndScaffoldingAreHandled(t *testing.T) {
	f := newNativeFixture(t, "-work")
	var entries []any
	entries = append(entries, inspectEntry("user", ContextPreamble+". Summary: Port the work module to Go", nil))
	entries = append(entries, inspectEntry("user", "<system-reminder>ignored</system-reminder>", nil))
	for i := 0; i < 5; i++ {
		entries = append(entries, inspectEntry("user", "prompt "+string(rune('a'+i)), nil))
		entries = append(entries, map[string]any{"type": "assistant", "timestamp": "2026-09-16T10:00:0" + string(rune('0'+i)) + "Z",
			"message": map[string]any{"model": "claude-fable-5-1", "content": []any{map[string]any{"type": "text", "text": "reply <speak>" + string(rune('a'+i)) + "</speak>"}}}})
	}
	writeTranscript(t, f.dir, "sess", entries...)
	detail, err := Inspect(f.projects, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Summary != "Port the work module to Go" || deref(detail.Model) != "claude-fable-5-1" || deref(detail.LastActivity) != "2026-09-16T10:00:04Z" {
		t.Fatalf("detail %+v", detail)
	}
	if detail.Turns != 5 || len(detail.Recent) != 6 || detail.Recent[0].Text != "prompt c" || detail.Recent[5].Text != "reply e" || detail.LastAssistant != "reply e" {
		t.Fatalf("recent %+v", detail.Recent)
	}
	if detail.CWD != nil || detail.Branch != nil {
		t.Fatalf("cwd/branch must be null when unknown: %+v", detail)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"kind":"native"`, `"queued":[]`, `"cwd":null`, `"branch":null`, `"backend":"claude"`, `"turns":5`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("json missing %s: %s", key, encoded)
		}
	}
}

func TestSummaryOf(t *testing.T) {
	if summaryOf("plain text") != "" {
		t.Fatal("non-preamble text has no summary")
	}
	if got := summaryOf(ContextPreamble + " that ran out of context. Primary Request and Intent: Fix the tests"); got != "Fix the tests" {
		t.Fatalf("got %q", got)
	}
	if got := summaryOf(ContextPreamble + " with no marker"); got != "with no marker" {
		t.Fatalf("got %q", got)
	}
}
