package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
