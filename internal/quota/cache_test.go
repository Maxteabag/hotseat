package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFetch stands in for the patched usage.for_token: it counts calls and
// returns whatever the test has queued.
type fakeFetch struct {
	calls  int
	result Summary
	err    error
}

func (f *fakeFetch) fetch(context.Context, string) (Summary, error) {
	f.calls++
	return f.result, f.err
}

func ptr(f float64) *float64 { return &f }

// harness is a Cache on a temp root with a settable clock (epoch seconds).
type harness struct {
	cache *Cache
	fetch *fakeFetch
	now   float64
	dir   string
}

func newHarness(t *testing.T, fetch *fakeFetch) *harness {
	t.Helper()
	h := &harness{fetch: fetch, now: 100, dir: t.TempDir()}
	h.cache = &Cache{
		Root:  h.dir,
		Now:   func() time.Time { return time.Unix(0, int64(h.now*1e9)) },
		Fetch: fetch.fetch,
		Warn:  func(err error) { t.Errorf("unexpected cache write failure: %v", err) },
	}
	return h
}

func (h *harness) read(t *testing.T) (Result, error) {
	t.Helper()
	return h.cache.Read(context.Background(), "secret")
}

func (h *harness) mustFail(t *testing.T, contains string) *UsageError {
	t.Helper()
	_, err := h.read(t)
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UsageError, got %v", err)
	}
	if !strings.Contains(ue.Error(), contains) {
		t.Fatalf("error %q does not contain %q", ue.Error(), contains)
	}
	return ue
}

func (h *harness) files(t *testing.T) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestThrottledRequestIsNotRepeatedDuringCooldown(t *testing.T) {
	h := newHarness(t, &fakeFetch{err: &UsageError{Message: "usage endpoint returned 429"}})
	for range 2 {
		h.mustFail(t, "429")
	}
	if h.fetch.calls != 1 {
		t.Fatalf("fetch called %d times, want 1", h.fetch.calls)
	}
	var contents []string
	for _, entry := range h.files(t) {
		raw, err := os.ReadFile(filepath.Join(h.dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(raw))
	}
	if len(contents) == 0 || strings.Contains(strings.Join(contents, ""), "secret") {
		t.Fatalf("token must not be written to disk; files: %v", contents)
	}
	h.now = 161
	h.mustFail(t, "429")
	if h.fetch.calls != 2 {
		t.Fatalf("fetch called %d times, want 2", h.fetch.calls)
	}
}

func TestNonThrottleErrorDoesNotCreateCooldown(t *testing.T) {
	h := newHarness(t, &fakeFetch{err: &UsageError{Message: "unauthorized"}})
	h.mustFail(t, "unauthorized")
	if entries := h.files(t); len(entries) != 0 {
		t.Fatalf("cache directory not empty: %v", entries)
	}
}

func TestSuccessIsReusedAndKeepsItsOriginalTimestamp(t *testing.T) {
	h := newHarness(t, &fakeFetch{result: Summary{Used7d: ptr(.25)}})
	first, err := h.read(t)
	if err != nil {
		t.Fatal(err)
	}
	h.now = 115
	second, err := h.read(t)
	if err != nil {
		t.Fatal(err)
	}
	if h.fetch.calls != 1 {
		t.Fatalf("fetch called %d times, want 1", h.fetch.calls)
	}
	if first.Cached || !second.Cached {
		t.Fatalf("first.cached=%v second.cached=%v", first.Cached, second.Cached)
	}
	if first.CheckedAt != 100 || second.CheckedAt != 100 {
		t.Fatalf("checked_at first=%v second=%v", first.CheckedAt, second.CheckedAt)
	}
	if first.Warning != "" || second.Warning != "" {
		t.Fatalf("unexpected warnings %q %q", first.Warning, second.Warning)
	}
	near(t, second.Used7d, .25)
}

func TestThrottlePreservesLastGoodDataAndHonorsRetryAfter(t *testing.T) {
	fetch := &fakeFetch{result: Summary{Used7d: ptr(.25)}}
	h := newHarness(t, fetch)
	if _, err := h.read(t); err != nil {
		t.Fatal(err)
	}
	fetch.err = &UsageError{Message: "usage endpoint returned 429", RetryAfter: ptr(120)}
	h.now = 161
	row, err := h.read(t)
	if err != nil {
		t.Fatal(err)
	}
	near(t, row.Used7d, .25)
	if row.CheckedAt != 100 || !row.Cached {
		t.Fatalf("checked_at=%v cached=%v", row.CheckedAt, row.Cached)
	}
	if !strings.Contains(row.Warning, "429") {
		t.Fatalf("warning = %q", row.Warning)
	}
	// Retry-After (120s) beats the first backoff step (60s): until = 161+120 = 281.
	if want := "Usage API throttled (HTTP 429); retry in 121s"; row.Warning != want {
		t.Fatalf("warning = %q, want %q", row.Warning, want)
	}
	h.now = 250
	if _, err := h.read(t); err != nil {
		t.Fatal(err)
	}
	if fetch.calls != 2 {
		t.Fatalf("fetch called %d times, want 2", fetch.calls)
	}
}

func TestBackoffIncreasesAndDoesNotMaskAuthFailure(t *testing.T) {
	fetch := &fakeFetch{err: &UsageError{Message: "429"}}
	h := newHarness(t, fetch)
	h.mustFail(t, "429")
	h.now = 161
	h.mustFail(t, "429")
	h.now = 240
	h.mustFail(t, "429")
	if fetch.calls != 2 {
		t.Fatalf("fetch called %d times, want 2", fetch.calls)
	}
	fetch.err = &UsageError{Message: "unauthorized"}
	h.now = 300
	h.mustFail(t, "unauthorized")
}

func TestBackoffCapsAtFiveFailures(t *testing.T) {
	fetch := &fakeFetch{err: &UsageError{Message: "usage endpoint returned 429"}}
	h := newHarness(t, fetch)
	// Delays: 60, 120, 240, 480, 960, then 960 again.
	wants := []float64{60, 120, 240, 480, 960, 960, 960}
	for i, want := range wants {
		h.now += 10000
		ue := h.mustFail(t, "429")
		if got := "Usage API throttled (HTTP 429); retry in " + itoa(int(want)+1) + "s"; ue.Error() != got {
			t.Fatalf("attempt %d: %q, want %q", i+1, ue.Error(), got)
		}
		rec := load(filepath.Join(h.dir, Filename("secret")))
		if rec.Failures != float64(min(i+1, 5)) || rec.Until != h.now+want {
			t.Fatalf("attempt %d: failures=%v until=%v (now %v)", i+1, rec.Failures, rec.Until, h.now)
		}
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestPythonWrittenCacheFileIsRead(t *testing.T) {
	h := newHarness(t, &fakeFetch{err: &UsageError{Message: "must not be called"}})
	// Exactly what json.dump wrote from the Python implementation.
	python := `{"data": {"status": "allowed", "available": true, "limited": false, "severity": "normal", ` +
		`"used_5h": 0.36, "reset_5h": 1789404600, "used_7d": 0.53, "reset_7d": 1789916400, "scoped": [], ` +
		`"blocked_models": [], "extra_usage_enabled": false, "extra_usage_reason": null, "plan": "max"}, ` +
		`"checked_at": 90.5, "until": 150.5, "failures": 0}`
	if err := os.WriteFile(filepath.Join(h.dir, Filename("secret")), []byte(python), 0o600); err != nil {
		t.Fatal(err)
	}
	row, err := h.read(t)
	if err != nil {
		t.Fatal(err)
	}
	if h.fetch.calls != 0 || !row.Cached || row.CheckedAt != 90.5 || row.Warning != "" {
		t.Fatalf("calls=%d cached=%v checked_at=%v warning=%q", h.fetch.calls, row.Cached, row.CheckedAt, row.Warning)
	}
	near(t, row.Reset5h, 1789404600)
	if row.Plan != "max" || row.Source != SourceUsage {
		t.Fatalf("plan=%q source=%v", row.Plan, row.Source)
	}
}

func TestCorruptCacheFileIsIgnored(t *testing.T) {
	h := newHarness(t, &fakeFetch{result: Summary{Used7d: ptr(.5)}})
	path := filepath.Join(h.dir, Filename("secret"))
	for _, junk := range []string{"not json", "[1, 2]", `{"until": 1e12, "data": "text"}`} {
		if err := os.WriteFile(path, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		before := h.fetch.calls
		row, err := h.read(t)
		if junk == `{"until": 1e12, "data": "text"}` {
			// A live cooldown without a usable dict is an error, as in Python.
			var ue *UsageError
			if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "429") || h.fetch.calls != before {
				t.Fatalf("junk %q: err=%v calls=%d", junk, err, h.fetch.calls-before)
			}
			continue
		}
		if err != nil || row.Cached || h.fetch.calls != before+1 {
			t.Fatalf("junk %q: err=%v cached=%v calls=%d", junk, err, row.Cached, h.fetch.calls-before)
		}
	}
}

func TestStoreWritesPrivateAtomicFiles(t *testing.T) {
	h := newHarness(t, &fakeFetch{result: Summary{Used7d: ptr(.25), Plan: "pro"}})
	h.cache.Root = filepath.Join(h.dir, "nested", "quota-cooldowns")
	if _, err := h.read(t); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(h.cache.Root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(h.cache.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != Filename("secret") {
		t.Fatalf("entries = %v", entries)
	}
	fi, _ := entries[0].Info()
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(h.cache.Root, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	data, _ := rec["data"].(map[string]any)
	if rec["checked_at"] != 100.0 || rec["until"] != 160.0 || rec["failures"] != 0.0 || data["plan"] != "pro" {
		t.Fatalf("record = %s", raw)
	}
}

func TestWriteFailureIsReportedNotFatal(t *testing.T) {
	h := newHarness(t, &fakeFetch{result: Summary{Used7d: ptr(.25)}})
	blocker := filepath.Join(h.dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cache.Root = blocker // a file where the directory should be
	var warned error
	h.cache.Warn = func(err error) { warned = err }
	row, err := h.read(t)
	if err != nil || row.Cached {
		t.Fatalf("err=%v cached=%v", err, row.Cached)
	}
	if warned == nil || !strings.Contains(warned.Error(), "quota cache") {
		t.Fatalf("warn = %v", warned)
	}
	h.cache.Warn = nil
	if _, err := h.read(t); err != nil {
		t.Fatal(err)
	}
}

func TestResultJSONAddsBookkeepingKeys(t *testing.T) {
	row := Result{Summary: SummariseUsage(nil), CheckedAt: 100, Cached: true, Warning: "w"}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"status":"allowed",`) ||
		!strings.HasSuffix(string(raw), `,"plan":null,"_checked_at":100,"_cached":true,"_warning":"w"}`) {
		t.Fatalf("json = %s", raw)
	}
	var back Result
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.CheckedAt != 100 || !back.Cached || back.Warning != "w" || back.Status != "allowed" {
		t.Fatalf("round trip = %+v", back)
	}
}

func TestFilenameAndDefaultRoot(t *testing.T) {
	if got, want := Filename("secret"), "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b.json"; got != want {
		t.Fatalf("Filename = %s", got)
	}
	t.Setenv("XDG_CACHE_HOME", "/var/tmp/xdg")
	if got := DefaultRoot(); got != "/var/tmp/xdg/hotseat/quota-cooldowns" {
		t.Fatalf("DefaultRoot = %s", got)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got := DefaultRoot(); got != "/home/someone/.cache/hotseat/quota-cooldowns" {
		t.Fatalf("DefaultRoot = %s", got)
	}
}

func TestZeroValueCacheUsesForToken(t *testing.T) {
	// Point the default client at an in-process handler that rejects the token,
	// so the zero-value path is exercised without any network or disk write.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := Client
	Client = clientFor(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	defer func() { Client = old }()
	_, err := Read(context.Background(), "secret")
	var ue *UsageError
	if !errors.As(err, &ue) || ue.Error() != "the account rejected this token" {
		t.Fatalf("err = %v", err)
	}
}
