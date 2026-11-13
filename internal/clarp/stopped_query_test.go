package clarp

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// formerStoppedQuery is how stopped agents were once found: the whole state log
// grouped by agent, twice.
const formerStoppedQuery = `
        WITH stopped AS (
            SELECT agent_id, MAX(ts) AS fail_ts
              FROM state_log
             WHERE kind IN ('interrupted', 'error')
               AND json_extract(detail, '$.reason') = 'usage_limit'
             GROUP BY agent_id),
        finished AS (
            SELECT agent_id, MAX(ts) AS ok_ts FROM state_log
             WHERE kind = 'done' GROUP BY agent_id)
        SELECT a.session, s.fail_ts
          FROM stopped s
          JOIN agents a USING (agent_id)
          LEFT JOIN finished f USING (agent_id)
         WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
           AND (f.ok_ts IS NULL OR f.ok_ts < s.fail_ts)
           AND (? IS NULL OR s.fail_ts >= ?)
    `

// The state log of a real installation is indexed like this.
const stateLogIndex = `CREATE INDEX idx_state_log_agent_ts ON state_log(agent_id, ts DESC)`

func (f *fixture) formerStopped(cutoff any) map[string]int64 {
	f.t.Helper()
	rows, err := f.db.Query(formerStoppedQuery, cutoff, cutoff)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]int64{}
	for rows.Next() {
		var session string
		var ts int64
		if err := rows.Scan(&session, &ts); err != nil {
			f.t.Fatal(err)
		}
		found[session] = ts
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return found
}

func TestStoppedAgentsMatchTheWholeLogQuery(t *testing.T) {
	r := rand.New(rand.NewSource(20261113))
	listed := 0
	for round := 0; round < 60; round++ {
		schema := resumeSchema
		if round%2 == 0 {
			schema = append(append([]string{}, resumeSchema...), stateLogIndex)
		}
		f := newFixture(t, schema)
		nowMillis := f.svc.cutoffMillis(0)
		for a := 0; a < 12; a++ {
			var deleted, archived any
			switch r.Intn(8) {
			case 0:
				deleted = 1
			case 1:
				archived = 1
			}
			id := fmt.Sprintf("a%d", a)
			f.agent(id, fmt.Sprintf("agent%d", a), "claude", deleted, archived)
			// Few distinct times, so stops and finished turns often share one,
			// and a span that crosses every window the listing is asked for.
			for e := r.Intn(14); e > 0; e-- {
				ts := nowMillis - int64(r.Intn(12))*86400_000 - int64(r.Intn(3))
				kind := []string{"thinking", "tool", "done", "done", "interrupted", "interrupted", "error", "spawned"}[r.Intn(8)]
				var detail any
				switch r.Intn(5) {
				case 0, 1:
					detail = `{"reason":"usage_limit"}`
				case 2:
					detail = `{"reason":"user"}`
				case 3:
					detail = `{}`
				}
				f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)", id, ts, kind, detail)
			}
		}
		for _, days := range []int{0, 1, 3, 7} {
			var cutoff any
			if days != 0 {
				cutoff = f.svc.cutoffMillis(days)
			}
			want := f.formerStopped(cutoff)
			found := items(t)(f.svc.StoppedClarpAgents(context.Background(), days))
			got := map[string]int64{}
			for i, item := range found {
				got[item.ID] = int64(math.Round(item.StoppedAt * 1000))
				if i > 0 && item.StoppedAt > found[i-1].StoppedAt {
					t.Fatalf("round %d: not newest first: %+v", round, found)
				}
			}
			if len(got) != len(found) {
				t.Fatalf("round %d: an agent was listed twice: %+v", round, found)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d, %d days:\n got %v\nwant %v", round, days, got, want)
			}
			listed += len(want)
		}
	}
	if listed < 200 {
		t.Fatalf("only %d stops across all rounds; the comparison proved little", listed)
	}
}

// Thinking and tool states carry free-form detail; a stop must still be found
// when one of them is not JSON.
func TestDetailThatIsNotJSONOnOtherStatesDoesNotBreakTheListing(t *testing.T) {
	f := resumeFixture(t)
	f.agent("a1", "adam", "claude", nil, nil)
	f.event("a1", "interrupted", nowSeconds()-3600, "usage_limit")
	for i, kind := range []string{"thinking", "tool", "spawned"} {
		f.exec("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
			"a1", int64((nowSeconds()-1800)*1000)+int64(i), kind, "{not json")
	}
	if got := ids(items(t)(f.svc.StoppedClarpAgents(context.Background(), 7))); !reflect.DeepEqual(got, []string{"adam"}) {
		t.Fatalf("got %v", got)
	}
}

// bulkyLog fills the state log the way a busy installation does: every agent
// with a long run of bulky thinking and tool states, a finished turn near the
// end, and one agent stopped by a usage limit.
func bulkyLog(t *testing.T, agents, states int) *fixture {
	t.Helper()
	f := newFixture(t, append(append([]string{}, resumeSchema...), stateLogIndex))
	detail := `{"text":"` + strings.Repeat("d", 1000) + `"}`
	base := f.svc.cutoffMillis(6)
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare("INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	for a := 0; a < agents; a++ {
		id := fmt.Sprintf("a%d", a)
		if _, err := tx.Exec("INSERT INTO agents VALUES (?,?,?,?,?,?,?,?)", id, id, "claude", "/tmp", id, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		for s := 0; s < states; s++ {
			kind, body := []string{"thinking", "tool"}[s%2], detail
			switch {
			case s == states-5:
				kind, body = "done", "{}"
			case s == states-2 && a == 0:
				kind, body = "interrupted", `{"reason":"usage_limit"}`
			}
			if _, err := insert.Exec(id, base+int64(s), kind, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

// formerLastSaid is how the last reply was once found: every message of the
// agent read and sorted.
const formerLastSaid = `(SELECT m.text FROM messages m
                  WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                    AND length(trim(coalesce(m.text,''))) > 0
                  ORDER BY m.seq DESC LIMIT 1) AS said`

func (f *fixture) said(expression string) map[string]string {
	f.t.Helper()
	rows, err := f.db.Query("SELECT a.agent_id, " + expression + " FROM agents a")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var id string
		var said *string
		if err := rows.Scan(&id, &said); err != nil {
			f.t.Fatal(err)
		}
		found[id] = "<none>"
		if said != nil {
			found[id] = *said
		}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return found
}

func TestTheLastReplyMatchesTheWholeHistorySearch(t *testing.T) {
	r := rand.New(rand.NewSource(64))
	beyondTheTail, none := 0, 0
	for round := 0; round < 20; round++ {
		f := resumeFixture(t)
		for a := 0; a < 10; a++ {
			id := fmt.Sprintf("a%d", a)
			f.agent(id, fmt.Sprintf("agent%d", a), "claude", nil, nil)
			// A history, then a run of rows with nothing an assistant said: tool
			// rows, user turns, empty replies. The run is often longer than the
			// tail the lookup tries first.
			history, run := r.Intn(90), r.Intn(140)
			if r.Intn(6) == 0 {
				history = 0
			}
			order := r.Perm(history + run)
			for _, seq := range order {
				role, text := "assistant", fmt.Sprintf("reply %d", seq)
				if seq >= history {
					role, text = []string{"user", "tool", "assistant", "assistant"}[r.Intn(4)], []string{"", "   ", "words"}[r.Intn(3)]
					if role == "assistant" && text == "words" {
						text = ""
					}
				} else if r.Intn(3) == 0 {
					role = "user"
				}
				var storedRole any = role
				if r.Intn(25) == 0 {
					storedRole = nil
				}
				f.exec("INSERT INTO messages (agent_id, seq, role, text) VALUES (?,?,?,?)", id, seq, storedRole, text)
			}
			if run > 64 {
				beyondTheTail++
			}
		}
		want, got := f.said(formerLastSaid), f.said(lastSaid)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d:\n got %v\nwant %v", round, got, want)
		}
		for _, said := range want {
			if said == "<none>" {
				none++
			}
		}
	}
	if beyondTheTail < 30 || none < 3 {
		t.Fatalf("%d agents with a reply beyond the tail, %d with none; the comparison proved little", beyondTheTail, none)
	}
}
