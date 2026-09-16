package work

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCleanStripsSpeechMarkupAndCollapsesWhitespace(t *testing.T) {
	got := Clean("<speak>Hello <break time=\"1s\"/>  there\n<emphasis level=\"strong\">friend</emphasis></speak>")
	if got != "Hello there friend" {
		t.Fatalf("Clean = %q", got)
	}
	if Clean("") != "" {
		t.Fatal("empty input must stay empty")
	}
}

func TestTrimAddsEllipsisPastTheLimit(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := Trim(long, Snippet)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got[len(got)-10:])
	}
	if n := len([]rune(got)); n > Snippet+1 || strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Fatalf("trimmed length %d or trailing space in %q", n, got)
	}
	if Trim("short", Snippet) != "short" {
		t.Fatal("short text must be untouched")
	}
	if got := Trim("ab cd", 3); got != "ab…" {
		t.Fatalf("rstrip before ellipsis: %q", got)
	}
}

func TestEntryTextReadsStringAndBlockContent(t *testing.T) {
	var entry Entry
	if err := json.Unmarshal([]byte(`{"message":{"content":"plain"}}`), &entry); err != nil {
		t.Fatal(err)
	}
	if EntryText(entry) != "plain" {
		t.Fatalf("string content: %q", EntryText(entry))
	}
	if err := json.Unmarshal([]byte(`{"message":{"content":[{"type":"text","text":"a"},{"type":"tool_use","name":"x"},{"type":"text","text":"b"}]}}`), &entry); err != nil {
		t.Fatal(err)
	}
	if EntryText(entry) != "a b" {
		t.Fatalf("block content: %q", EntryText(entry))
	}
	if EntryText(Entry{"message": map[string]any{"content": 5.0}}) != "" || EntryText(Entry{}) != "" || EntryText(nil) != "" {
		t.Fatal("other content shapes must read as empty")
	}
}
