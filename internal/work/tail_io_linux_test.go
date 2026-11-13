package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A quota listing must not read archived history just to inspect recent work.
func TestLastEntriesDoesNotReadTheWholeHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(f, "{\"type\":\"user\",\"n\":%d}\n", i)
	}
	f.Close()
	readBytes := func() uint64 {
		b, err := os.ReadFile("/proc/self/io")
		if err != nil {
			t.Fatal(err)
		}
		var n uint64
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "rchar:") {
				fmt.Sscanf(l, "rchar: %d", &n)
				return n
			}
		}
		t.Fatal("missing rchar")
		return 0
	}
	before := readBytes()
	got := LastEntries(p, 40)
	used := readBytes() - before
	if len(got) != 40 || got[0]["n"] != float64(0) || got[39]["n"] != float64(39) {
		t.Fatalf("wrong tail: %v", got)
	}
	if used > 4<<20 {
		t.Fatalf("tail read %d bytes for 40 recent records; budget 4 MiB", used)
	}
}

func readChars(t *testing.T) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "rchar:") {
			var n uint64
			fmt.Sscanf(l, "rchar: %d", &n)
			return n
		}
	}
	t.Fatal("missing rchar")
	return 0
}

// A listing runs every minute over every transcript of the week. Reading forty
// records of each cost gigabytes on a busy host; the last turn settles nearly
// all of them, and an unchanged transcript needs no read at all.
func TestTheStoppedScanReadsLittleAndThenNothing(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	dir := filepath.Join(projects, "-work-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const count = 60
	heavy := strings.Repeat("x", 100_000)
	for i := 0; i < count; i++ {
		var b strings.Builder
		for j := 0; j < 12; j++ {
			fmt.Fprintf(&b, "{\"type\":\"user\",\"text\":%q}\n", heavy)
		}
		b.WriteString("{\"type\":\"assistant\",\"message\":{\"content\":\"done\"}}\n")
		b.WriteString("{\"type\":\"system\",\"note\":\"turn ended\"}\n")
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("s%03d.jsonl", i)), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stop := "{\"type\":\"assistant\",\"isApiErrorMessage\":true,\"message\":{\"content\":\"You've hit your session limit\"}}\n"
	if err := os.WriteFile(filepath.Join(dir, "stopped.jsonl"), []byte(stop), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &ScanCache{Path: filepath.Join(t.TempDir(), "scan.json")}
	now := time.Now()

	before := readChars(t)
	first := ScanStopped(cache, projects, 7, now)
	cold := readChars(t) - before
	if len(first) != 1 || first[0].ID != "stopped" {
		t.Fatalf("first listing = %+v", first)
	}
	// 72 MB of transcripts; the full-tail scan read all of it.
	if budget := uint64(count * 16 * 1024); cold > budget {
		t.Fatalf("first listing read %d bytes; budget %d", cold, budget)
	}

	before = readChars(t)
	second := ScanStopped(cache, projects, 7, now)
	warm := readChars(t) - before
	if len(second) != 1 {
		t.Fatalf("second listing = %+v", second)
	}
	info, err := os.Stat(cache.Path)
	if err != nil {
		t.Fatal(err)
	}
	if budget := uint64(info.Size()) + 16*1024; warm > budget {
		t.Fatalf("second listing read %d bytes; budget %d (the cache file plus slack)", warm, budget)
	}
}

// A transcript with no turn near its end is given up on after the same forty
// records the scan always looked at, not read to its first byte.
func TestATranscriptWithoutARecentTurnIsNotReadWhole(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "projects")
	dir := filepath.Join(projects, "-work-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("{\"type\":\"assistant\",\"isApiErrorMessage\":true,\"message\":{\"content\":\"You've hit your session limit\"}}\n")
	note := strings.Repeat("n", 1000)
	for i := 0; i < 20_000; i++ {
		fmt.Fprintf(&b, "{\"type\":\"system\",\"note\":%q}\n", note)
	}
	if err := os.WriteFile(filepath.Join(dir, "bookkeeping.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readChars(t)
	found := StoppedNativeSessions(projects, 7, time.Now())
	used := readChars(t) - before
	if len(found) != 0 {
		t.Fatalf("a stop buried under bookkeeping was listed: %+v", found)
	}
	if used > 1<<20 {
		t.Fatalf("read %d bytes of a 20 MB transcript; budget 1 MiB", used)
	}
}
