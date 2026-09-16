package claude

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLabel(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5-1":           "Fable 5.1",
		"claude-haiku-4-5-20251001":  "Haiku 4.5",
		"claude-opus-4-1":            "Opus 4.1",
		"claude-sonnet-4-5-20250929": "Sonnet 4.5",
		"claude-3-7-sonnet-20250219": "3 7.sonnet",
		"fable":                      "Fable",
		"":                           "",
	}
	for model, want := range cases {
		if got := Label(model); got != want {
			t.Errorf("Label(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestFamily(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5-1":          "fable",
		"claude-opus-4-1":           "opus",
		"claude-sonnet-4-5":         "sonnet",
		"claude-haiku-4-5-20251001": "haiku",
		"Claude-Haiku-3":            "haiku",
		"gpt-5":                     "other",
	}
	for model, want := range cases {
		if got := Family(model); got != want {
			t.Errorf("Family(%q) = %q, want %q", model, got, want)
		}
	}
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestRecentByModelAbsentFileIsNil(t *testing.T) {
	if got := RecentByModel(DefaultDays, t.TempDir(), day("2026-01-08")); got != nil {
		t.Fatalf("got %+v", got)
	}
	root := t.TempDir()
	writeFile(t, StatsFile(root), "{not json")
	if got := RecentByModel(DefaultDays, root, day("2026-01-08")); got != nil {
		t.Fatalf("damaged file: got %+v", got)
	}
	writeFile(t, StatsFile(root), `{"lastComputedDate": "2026-01-08"}`)
	if got := RecentByModel(DefaultDays, root, day("2026-01-08")); got != nil {
		t.Fatalf("no dailyModelTokens: got %+v", got)
	}
	writeFile(t, StatsFile(root), `{"dailyModelTokens": {"not": "a list"}}`)
	if got := RecentByModel(DefaultDays, root, day("2026-01-08")); got != nil {
		t.Fatalf("dailyModelTokens not a list: got %+v", got)
	}
}

func TestRecentByModelAggregatesTheWindow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, StatsFile(root), `{
  "lastComputedDate": "2026-01-08",
  "dailyModelTokens": [
    {"date": "2025-12-31", "tokensByModel": {"claude-fable-5-1": 999999}},
    {"date": "2026-01-01", "tokensByModel": {"claude-fable-5-1": 1000000, "claude-haiku-4-5-20251001": 250000}},
    {"date": "2026-01-07", "tokensByModel": {"claude-sonnet-4-5": 250000, "claude-fable-5-1": 500000.7, "ignored": 0, "negative": -5, "text": "x"}},
    "not a row",
    {"date": "2026-01-08"},
    {"date": "2026-01-08", "tokensByModel": ["not", "an", "object"]}
  ]
}`)
	got := RecentByModel(DefaultDays, root, day("2026-01-08"))
	if got == nil {
		t.Fatal("expected a summary")
	}
	asOf := "2026-01-08"
	want := &ModelSummary{Days: 7, AsOf: &asOf, Stale: false, TotalTokens: 2_000_000, Models: []ModelUsage{
		{Model: "claude-fable-5-1", Label: "Fable 5.1", Family: "fable", Tokens: 1_500_000, Share: 0.75},
		{Model: "claude-haiku-4-5-20251001", Label: "Haiku 4.5", Family: "haiku", Tokens: 250_000, Share: 0.125},
		{Model: "claude-sonnet-4-5", Label: "Sonnet 4.5", Family: "sonnet", Tokens: 250_000, Share: 0.125},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRecentByModelJSONShape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, StatsFile(root), `{"lastComputedDate": "2026-01-01",
  "dailyModelTokens": [{"date": "2026-01-01", "tokensByModel": {"claude-fable-5-1": 1500000}}]}`)
	raw, err := json.Marshal(RecentByModel(7, root, day("2026-01-01")))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// The shape tests/test_cli.py feeds to `hotseat models`.
	want := map[string]any{"days": 7.0, "as_of": "2026-01-01", "stale": false, "total_tokens": 1500000.0,
		"models": []any{map[string]any{"model": "claude-fable-5-1", "label": "Fable 5.1",
			"family": "fable", "tokens": 1500000.0, "share": 1.0}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestRecentByModelStaleAndEmpty(t *testing.T) {
	root := t.TempDir()
	writeFile(t, StatsFile(root), `{"lastComputedDate": "2026-01-05", "dailyModelTokens": []}`)
	got := RecentByModel(7, root, day("2026-01-08"))
	if got == nil || !got.Stale || got.TotalTokens != 0 || len(got.Models) != 0 || *got.AsOf != "2026-01-05" {
		t.Fatalf("got %+v", got)
	}
	// Yesterday is not stale; the cache is only recomputed periodically.
	writeFile(t, StatsFile(root), `{"lastComputedDate": "2026-01-07", "dailyModelTokens": []}`)
	if got := RecentByModel(7, root, day("2026-01-08")); got.Stale {
		t.Fatal("yesterday's cache is fresh enough")
	}
	// No lastComputedDate at all: null, not stale.
	writeFile(t, StatsFile(root), `{"dailyModelTokens": []}`)
	if got := RecentByModel(7, root, day("2026-01-08")); got.AsOf != nil || got.Stale {
		t.Fatalf("got %+v", got)
	}
}

func TestRecentByModelTiesKeepDocumentOrder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, StatsFile(root), `{"dailyModelTokens": [{"date": "2026-01-08",
  "tokensByModel": {"zeta": 10, "alpha": 10, "mid": 20}}]}`)
	got := RecentByModel(7, root, day("2026-01-08"))
	if names := []string{got.Models[0].Model, got.Models[1].Model, got.Models[2].Model}; !equalStrings(names, []string{"mid", "zeta", "alpha"}) {
		t.Fatalf("order = %v", names)
	}
}

func TestStatsFileFollowsTheConfigDir(t *testing.T) {
	home := isolateHome(t)
	if got := StatsFile(""); got != filepath.Join(home, ".claude", "stats-cache.json") {
		t.Fatalf("stats file = %s", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/elsewhere")
	if got := StatsFile(""); got != "/elsewhere/stats-cache.json" {
		t.Fatalf("stats file = %s", got)
	}
	if got := ProjectsDir(); got != "/elsewhere/projects" {
		t.Fatalf("projects dir = %s", got)
	}
}
