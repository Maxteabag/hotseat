package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- the scan as it was before it learned to stop early ------------------------

// formerLastEntries is the tail reader the scan used to rely on: fixed 64 KiB
// blocks, always collecting the requested number of records.
func formerLastEntries(path string, keep int) []Entry {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	pos := info.Size()
	var pending []byte
	var entries []Entry
	for pos > 0 {
		size := int64(64 * 1024)
		if pos < size {
			size = pos
		}
		pos -= size
		block := make([]byte, int(size))
		if _, err := file.ReadAt(block, pos); err != nil {
			return nil
		}
		block = append(block, pending...)
		end := len(block)
		for {
			i := bytes.LastIndexByte(block[:end], '\n')
			if i < 0 {
				break
			}
			if entry, ok := parseLine(block[i+1 : end]); ok {
				entries = append(entries, entry)
				if len(entries) == keep {
					slices.Reverse(entries)
					return entries
				}
			}
			end = i
		}
		pending = block[:end]
	}
	if entry, ok := parseLine(pending); ok {
		entries = append(entries, entry)
	}
	slices.Reverse(entries)
	return entries
}

// formerStopped is the scan as it was: the last 40 records of every recent
// transcript, read on every listing.
func formerStopped(projectsDir string, windowDays int, now time.Time) []StoppedItem {
	var cutoff float64
	if windowDays != 0 {
		cutoff = epochSeconds(now) - float64(windowDays)*86400
	}
	var found []StoppedItem
	for _, transcript := range transcripts(projectsDir) {
		info, err := os.Stat(transcript)
		if err != nil {
			continue
		}
		modified := epochSeconds(info.ModTime())
		if modified < cutoff {
			continue
		}
		entries := formerLastEntries(transcript, tailKeep)
		if len(entries) == 0 {
			continue
		}
		said := ""
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.str("type") == "assistant" && !truthy(e["isApiErrorMessage"]) && strings.TrimSpace(EntryText(e)) != "" {
				said = EntryText(e)
				break
			}
		}
		last := lastTurn(entries)
		if last == nil || !truthy(last["isApiErrorMessage"]) {
			continue
		}
		message, _ := last["message"].(map[string]any)
		text, ok := message["content"].(string)
		if !ok {
			if encoded, err := json.Marshal(message["content"]); err == nil {
				text = string(encoded)
			}
		}
		if !limitText.MatchString(text) {
			continue
		}
		cwd := last.str("cwd")
		if cwd == "" {
			cwd = DecodeProjectDir(filepath.Base(filepath.Dir(transcript)))
		}
		id := stem(transcript)
		found = append(found, StoppedItem{
			Kind: "native", Cause: "usage_limit", ID: id, Name: shortName(id), Backend: "claude",
			CWD: cwd, LastMessage: Trim(said, 150), StoppedAt: modified,
		})
	}
	slices.SortStableFunc(found, func(a, b StoppedItem) int {
		return compareFloatDesc(a.StoppedAt, b.StoppedAt)
	})
	return found
}

// --- transcripts built from every kind of line the scan can meet ---------------

func entryAssistant(text string) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}

// randomLine returns one transcript line: a turn, a limit stop, another API
// error, bookkeeping that is not a turn, JSON that is not an object, or damage.
func randomLine(r *rand.Rand) string {
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	pad := func() string {
		switch r.Intn(12) {
		case 0:
			return strings.Repeat("y", 3000+r.Intn(20000))
		case 1:
			return strings.Repeat("z", 60000+r.Intn(250000))
		}
		return strings.Repeat("w", r.Intn(200))
	}
	switch r.Intn(16) {
	case 0, 1, 2:
		return encode(entryUser("ask " + pad()))
	case 3, 4, 5:
		return encode(entryAssistant("reply " + pad()))
	case 6:
		return encode(entryAssistant("  "))
	case 7, 8:
		stop := limitError()
		if r.Intn(2) == 0 {
			stop["cwd"] = "/work/here"
		}
		if r.Intn(3) == 0 {
			stop["message"] = map[string]any{"content": "You've hit your usage limit"}
		}
		return encode(stop)
	case 9:
		return encode(map[string]any{"type": "assistant", "isApiErrorMessage": true,
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "API Error: 529 overloaded"}}}})
	case 10, 11, 12:
		return encode(map[string]any{"type": "system", "note": pad()})
	case 13:
		return encode(map[string]any{"type": "file-history-snapshot", "n": r.Intn(100)})
	case 14:
		return []string{"17", "null", `"text"`, "[1,2]"}[r.Intn(4)]
	}
	return []string{"not json", "", "{\"type\":\"user\"", "   "}[r.Intn(4)]
}

func writeRandomTranscript(t *testing.T, r *rand.Rand, path string) {
	t.Helper()
	count := r.Intn(70)
	// A long run of bookkeeping after the last turn is what pushes a stop out of
	// the tail the scan looks at, so make such endings common.
	trailing := 0
	if r.Intn(3) == 0 {
		trailing = 30 + r.Intn(25)
	}
	var lines []string
	for i := 0; i < count; i++ {
		lines = append(lines, randomLine(r))
	}
	for i := 0; i < trailing; i++ {
		lines = append(lines, `{"type":"system","n":1}`)
	}
	body := strings.Join(lines, "\n")
	if r.Intn(2) == 0 {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheScanListsExactlyWhatTheFullTailScanListed(t *testing.T) {
	r := rand.New(rand.NewSource(20261113))
	f := newNativeFixture(t, "-work-project")
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "scan.json")}
	now := time.Now()
	stops := 0
	for round := 0; round < 8; round++ {
		// Each round rewrites some transcripts, adds some and leaves the rest, so
		// the remembered verdicts are exercised as well as fresh ones.
		for i := 0; i < 60; i++ {
			path := filepath.Join(f.dir, fmt.Sprintf("s%03d.jsonl", i))
			if _, err := os.Stat(path); err == nil && r.Intn(3) != 0 {
				continue
			}
			writeRandomTranscript(t, r, path)
		}
		want := formerStopped(f.projects, 7, now)
		stops += len(want)
		if got := StoppedNativeSessions(f.projects, 7, now); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d, no cache:\n got %+v\nwant %+v", round, got, want)
		}
		for pass := 0; pass < 2; pass++ {
			if got := ScanStopped(cache, f.projects, 7, now); !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d, cached pass %d:\n got %+v\nwant %+v", round, pass, got, want)
			}
		}
	}
	if stops < 20 {
		t.Fatalf("the generated transcripts held only %d stops; the comparison proved little", stops)
	}
}

func TestLastEntriesMatchesTheFixedBlockReader(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	dir := t.TempDir()
	for i := 0; i < 80; i++ {
		path := filepath.Join(dir, fmt.Sprintf("t%d.jsonl", i))
		writeRandomTranscript(t, r, path)
		for _, keep := range []int{1, 2, 7, 40, 500} {
			if got, want := LastEntries(path, keep), formerLastEntries(path, keep); !reflect.DeepEqual(got, want) {
				t.Fatalf("transcript %d, keep %d: %d records, want %d", i, keep, len(got), len(want))
			}
		}
	}
}

// --- the cache ----------------------------------------------------------------

func readScanFile(t *testing.T, path string) scanFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored scanFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func appendLines(t *testing.T, path string, entries ...any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append([]byte("\n"), b...)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnUnchangedTranscriptIsAnsweredFromTheCache(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "hotseat", "scan.json")}
	stopped := writeTranscript(t, f.dir, "stopped", entryUser("go"), entryAssistant("half done"), limitError())
	quiet := writeTranscript(t, f.dir, "quiet", entryUser("go"), entryAssistant("done"))
	now := time.Now()
	first := ScanStopped(cache, f.projects, 7, now)
	if len(first) != 1 || first[0].ID != "stopped" || first[0].LastMessage != "half done" {
		t.Fatalf("first listing = %+v", first)
	}
	info, err := os.Stat(cache.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v, want 0600", info.Mode().Perm())
	}
	if stored := readScanFile(t, cache.Path); len(stored.Files) != 2 || !stored.Files["-work-project/stopped.jsonl"].Stopped || stored.Files["-work-project/quiet.jsonl"].Stopped {
		t.Fatalf("cache = %+v", stored)
	}
	// Same size and time, different bytes: only a listing that does not read the
	// transcripts again still gives the first answer.
	for _, path := range []string{stopped, quiet} {
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(before.Size())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	if second := ScanStopped(cache, f.projects, 7, now); !reflect.DeepEqual(second, first) {
		t.Fatalf("second listing read the transcripts again: %+v", second)
	}
	if uncached := StoppedNativeSessions(f.projects, 7, now); len(uncached) != 0 {
		t.Fatalf("a listing without a cache must read the transcripts: %+v", uncached)
	}
}

func TestAGrownTranscriptIsReadAgain(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "scan.json")}
	resumed := writeTranscript(t, f.dir, "resumed", entryUser("go"), limitError())
	hit := writeTranscript(t, f.dir, "hit", entryUser("go"), entryAssistant("working"))
	now := time.Now()
	if found := ScanStopped(cache, f.projects, 7, now); len(found) != 1 || found[0].ID != "resumed" {
		t.Fatalf("first listing = %+v", found)
	}
	appendLines(t, resumed, entryUser("continue"), entryAssistant("finished"))
	appendLines(t, hit, limitError())
	found := ScanStopped(cache, f.projects, 7, now)
	if len(found) != 1 || found[0].ID != "hit" || found[0].LastMessage != "working" {
		t.Fatalf("after both transcripts grew = %+v", found)
	}
}

func TestTheCacheForgetsTranscriptsThatAreGoneOrOld(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "scan.json")}
	gone := writeTranscript(t, f.dir, "gone", entryUser("go"), limitError())
	aged := writeTranscript(t, f.dir, "aged", entryUser("go"), limitError())
	recent := writeTranscript(t, f.dir, "recent", entryUser("go"), limitError())
	now := time.Now()
	threeDays := now.Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(recent, threeDays, threeDays); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, f.dir, "kept", entryUser("go"), limitError())
	if found := ScanStopped(cache, f.projects, 7, now); len(found) != 4 {
		t.Fatalf("first listing = %+v", found)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	// A one-day listing leaves out the three-day-old stop but must keep its
	// verdict for the regular seven-day listing.
	if found := ScanStopped(cache, f.projects, 1, now); len(found) != 1 || found[0].ID != "kept" {
		t.Fatalf("one-day listing = %+v", found)
	}
	stored := readScanFile(t, cache.Path)
	var keys []string
	for key := range stored.Files {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"-work-project/kept.jsonl", "-work-project/recent.jsonl"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("cache holds %v, want %v", keys, want)
	}
	if found := ScanStopped(cache, f.projects, 7, now); len(found) != 2 {
		t.Fatalf("seven-day listing = %+v", found)
	}
}

func TestAnUnusableCacheIsRebuilt(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	writeTranscript(t, f.dir, "stopped", entryUser("go"), limitError())
	now := time.Now()
	path := filepath.Join(t.TempDir(), "scan.json")
	cache := &ScanCache{Path: path}
	claims := func(version int, projects string) []byte {
		// A cache that says the stopped transcript is fine; believing it would
		// hide the stop.
		info, err := os.Stat(filepath.Join(f.dir, "stopped.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(scanFile{Version: version, Projects: projects, Files: map[string]scanRecord{
			"-work-project/stopped.jsonl": {Size: info.Size(), ModTime: info.ModTime().UnixNano()},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, content := range map[string][]byte{
		"damaged":            []byte("{not json"),
		"another version":    claims(scanCacheVersion+1, f.projects),
		"another projects":   claims(scanCacheVersion, f.projects+"-elsewhere"),
		"empty":              nil,
		"wrong shape":        []byte(`{"version":1,"projects":7,"files":[]}`),
		"null where records": []byte(`null`),
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if found := ScanStopped(cache, f.projects, 7, now); len(found) != 1 {
			t.Fatalf("%s cache hid the stop: %+v", name, found)
		}
		if stored := readScanFile(t, path); stored.Version != scanCacheVersion || stored.Projects != f.projects || !stored.Files["-work-project/stopped.jsonl"].Stopped {
			t.Fatalf("%s cache was not rebuilt: %+v", name, stored)
		}
	}
	// The same claim under the right version and directory is believed, which
	// proves the cases above were rejected for the stated reason.
	if err := os.WriteFile(path, claims(scanCacheVersion, f.projects), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := ScanStopped(cache, f.projects, 7, now); len(found) != 0 {
		t.Fatalf("a valid cache was not used: %+v", found)
	}
}

func TestACacheThatCannotBeWrittenWarnsAndStillLists(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	writeTranscript(t, f.dir, "stopped", entryUser("go"), limitError())
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var warned []error
	cache := &ScanCache{Path: filepath.Join(blocker, "scan.json"), Warn: func(err error) { warned = append(warned, err) }}
	if found := ScanStopped(cache, f.projects, 7, time.Now()); len(found) != 1 {
		t.Fatalf("listing = %+v", found)
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "stopped-scan cache") {
		t.Fatalf("warnings = %v", warned)
	}
}

func TestTheServiceScanUsesItsCache(t *testing.T) {
	f := newNativeFixture(t, "-work-project")
	writeTranscript(t, f.dir, "stopped", entryUser("go"), limitError())
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "scan.json")}
	service := &Service{ProjectsDir: f.projects, ScanCache: cache}
	if found := service.Stopped(7, true); len(found) != 1 {
		t.Fatalf("listing = %+v", found)
	}
	if stored := readScanFile(t, cache.Path); len(stored.Files) != 1 {
		t.Fatalf("cache = %+v", stored)
	}
	t.Setenv("XDG_CACHE_HOME", "/var/tmp/xdg")
	if got := New(nil, nil).ScanCache; got == nil || got.Path != "/var/tmp/xdg/hotseat/stopped-scan.json" {
		t.Fatalf("default cache = %+v", got)
	}
}
