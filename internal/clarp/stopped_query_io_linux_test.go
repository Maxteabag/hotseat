package clarp

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

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

// A listing runs every minute. Grouping the whole state log by agent read all of
// it, twice: hundreds of megabytes on a small host and gigabytes on a busy one.
func TestFindingStoppedAgentsDoesNotReadTheWholeStateLog(t *testing.T) {
	f := bulkyLog(t, 20, 1500)
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 25<<20 {
		t.Fatalf("state log fixture is only %d bytes", info.Size())
	}
	before := readChars(t)
	found := items(t)(f.svc.StoppedClarpAgents(context.Background(), 7))
	used := readChars(t) - before
	if got := ids(found); !reflect.DeepEqual(got, []string{"a0"}) {
		t.Fatalf("got %v", got)
	}
	if used > 2<<20 {
		t.Fatalf("read %d bytes of a %d byte database; budget 2 MiB", used, info.Size())
	}
}

// Listing a stopped or paused agent quotes its last reply. Finding it by reading
// the agent's whole conversation cost megabytes per listed agent, every minute.
func TestTheLastReplyIsFoundWithoutReadingTheConversation(t *testing.T) {
	// Messages as a real installation indexes them: by agent, then by backend
	// session, so no index hands over one agent's messages in sequence order.
	var schema []string
	for _, statement := range resumeSchema {
		if !strings.Contains(statement, "CREATE TABLE messages") {
			schema = append(schema, statement)
		}
	}
	schema = append(schema,
		`CREATE TABLE messages (agent_id TEXT, backend_session_id TEXT, seq INT, role TEXT, text TEXT)`,
		`CREATE INDEX idx_messages_agent_seq ON messages(agent_id, backend_session_id, seq)`,
		`CREATE INDEX idx_messages_agent_role ON messages(agent_id, role)`)
	f := newFixture(t, schema)
	body := strings.Repeat("m", 2000)
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare("INSERT INTO messages (agent_id, backend_session_id, seq, role, text) VALUES (?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	const agents, messages = 5, 3000
	for a := 0; a < agents; a++ {
		id := fmt.Sprintf("a%d", a)
		if _, err := tx.Exec("INSERT INTO agents VALUES (?,?,?,?,?,?,?,?)", id, id, "claude", "/tmp", id, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO queue_state_revisions VALUES (?,?,?)", id, 1, 1); err != nil {
			t.Fatal(err)
		}
		for m := 0; m < messages; m++ {
			role := []string{"user", "assistant", "tool"}[m%3]
			if _, err := insert.Exec(id, fmt.Sprintf("session%d", m%2), m, role, fmt.Sprintf("%s %d %s", role, m, body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	before := readChars(t)
	found := items(t)(f.svc.PausedClarpAgents(context.Background()))
	used := readChars(t) - before
	if len(found) != agents {
		t.Fatalf("got %v", ids(found))
	}
	for _, item := range found {
		if !strings.HasPrefix(item.LastMessage, fmt.Sprintf("assistant %d ", messages-2)) {
			t.Fatalf("%s quoted %.40q", item.ID, item.LastMessage)
		}
	}
	if used > 2<<20 {
		t.Fatalf("read %d bytes of a %d byte database; budget 2 MiB", used, info.Size())
	}
}
