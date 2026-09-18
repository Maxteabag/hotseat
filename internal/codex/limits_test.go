package codex

// The rate-limit client against an in-memory app-server that replays the
// JSON-RPC protocol, ported from the fake `codex` in check_packaged_helpers.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProcess is an app-server that reads requests from stdin and answers
// through handler; a nil answer means "stay silent", like an unknown method.
type fakeProcess struct {
	stdinR, stdoutR *io.PipeReader
	stdinW, stdoutW *io.PipeWriter
	terminated      atomic.Bool
	killed          atomic.Bool
	done            chan struct{}
}

func (p *fakeProcess) Stdin() io.Writer  { return p.stdinW }
func (p *fakeProcess) Stdout() io.Reader { return p.stdoutR }
func (p *fakeProcess) Terminate() error {
	p.terminated.Store(true)
	p.stdinR.Close()
	p.stdoutW.Close()
	return nil
}
func (p *fakeProcess) Kill() error { p.killed.Store(true); return p.Terminate() }
func (p *fakeProcess) Wait() error { <-p.done; return nil }

type spawnRecord struct {
	argv []string
	env  []string
}

// fakeServer records spawns and serves each with handler.
type fakeServer struct {
	mu        sync.Mutex
	spawns    []spawnRecord
	processes []*fakeProcess
	lines     [][]byte
	handler   func(env []string, msg map[string]any) any
	// onSpawn, when set, sees each process as it starts.
	onSpawn func(p *fakeProcess)
}

func (s *fakeServer) spawner(ctx context.Context, argv []string, env []string) (Process, error) {
	p := &fakeProcess{done: make(chan struct{})}
	p.stdinR, p.stdinW = io.Pipe()
	p.stdoutR, p.stdoutW = io.Pipe()
	s.mu.Lock()
	s.spawns = append(s.spawns, spawnRecord{argv: argv, env: env})
	s.processes = append(s.processes, p)
	s.mu.Unlock()
	if s.onSpawn != nil {
		s.onSpawn(p)
	}
	go func() {
		defer close(p.done)
		scanner := bufio.NewScanner(p.stdinR)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			s.mu.Lock()
			s.lines = append(s.lines, line)
			s.mu.Unlock()
			var msg map[string]any
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			reply := s.handler(env, msg)
			if reply == nil {
				continue
			}
			encoded, _ := json.Marshal(reply)
			if _, err := p.stdoutW.Write(append(encoded, '\n')); err != nil {
				return
			}
		}
	}()
	return p, nil
}

// packagedHelperPayload is the fixture the wheel check answered with.
func packagedHelperPayload() map[string]any {
	return map[string]any{
		"rateLimits": map[string]any{"planType": "pro"},
		"rateLimitsByLimitId": map[string]any{"codex": map[string]any{
			"primary": map[string]any{"usedPercent": 40, "windowDurationMins": 10080, "resetsAt": 2000000000}}},
	}
}

// replay answers initialize and account/rateLimits/read by id, like the
// fake codex in scripts/check_packaged_helpers.py.
func replay(payload func(env []string) any) func([]string, map[string]any) any {
	return func(env []string, msg map[string]any) any {
		switch msg["method"] {
		case "initialize":
			return map[string]any{"id": msg["id"], "result": map[string]any{}}
		case "account/rateLimits/read":
			return map[string]any{"id": msg["id"], "result": payload(env)}
		}
		return nil
	}
}

func creds(email, account string) []byte {
	claims, _ := json.Marshal(map[string]any{"email": email})
	body := strings.TrimRight(base64URL(claims), "=")
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token": "x." + body + ".x", "access_token": "fixture", "refresh_token": "fixture", "account_id": account}})
	return raw
}

type limitsFixture struct {
	t      *testing.T
	codex  *Codex
	server *fakeServer
}

func newLimitsFixture(t *testing.T) *limitsFixture {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"old", "work"} {
		dir := filepath.Join(home, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), creds(name+"@example.com", name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), creds("old@example.com", "old"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{handler: replay(func([]string) any { return packagedHelperPayload() })}
	c := New(home)
	c.Spawn = server.spawner
	c.Which = func(string) (string, error) { return "/usr/bin/codex", nil }
	c.TempDir = t.TempDir()
	return &limitsFixture{t: t, codex: c, server: server}
}

func TestCompareReadsEveryProfileThroughItsOwnAppServer(t *testing.T) {
	f := newLimitsFixture(t)
	rows := f.codex.Compare(context.Background(), nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	names := []string{rows[0].Name, rows[1].Name, rows[2].Name}
	if strings.Join(names, ",") != "(live),old,work" {
		t.Fatalf("names = %v", names)
	}
	for _, row := range rows {
		if row.Error != nil {
			t.Fatalf("row %s errored: %s", row.Name, *row.Error)
		}
		if !row.Usable || row.Blocked || *row.Plan != "pro" {
			t.Fatalf("row = %+v", row)
		}
		if len(row.Windows) != 1 || *row.Windows[0].UsedPercent != 40 || row.Windows[0].Label != "weekly" ||
			row.Windows[0].Limit != "codex" || row.Windows[0].Tier != "primary" {
			t.Fatalf("windows = %+v", row.Windows)
		}
		if *row.ResetsAt != 2000000000 || row.SoonestWindow != "codex/primary weekly" || row.ResetDisplay == "-" {
			t.Fatalf("row = %+v", row)
		}
	}
	if rows[1].Email != "old@example.com" || rows[2].Email != "work@example.com" {
		t.Fatalf("emails = %q %q", rows[1].Email, rows[2].Email)
	}
	if len(f.server.spawns) != 3 {
		t.Fatalf("spawns = %d", len(f.server.spawns))
	}
	usage := shapeLimits(rows)
	for _, name := range []string{LiveName, "old", "work"} {
		if usage[name].Windows[0].Used != .4 {
			t.Fatalf("%s used = %v", name, usage[name].Windows[0].Used)
		}
	}
}

func TestTheProtocolMatchesTheAppServer(t *testing.T) {
	f := newLimitsFixture(t)
	f.codex.Compare(context.Background(), []string{"work"})
	if len(f.server.spawns) != 2 {
		t.Fatalf("live plus work expected: %d spawns", len(f.server.spawns))
	}
	spawn := f.server.spawns[0]
	if strings.Join(spawn.argv, " ") != `codex -c cli_auth_credentials_store="file" app-server --stdio` {
		t.Fatalf("argv = %q", spawn.argv)
	}
	if len(f.server.lines) != 6 {
		t.Fatalf("lines = %s", f.server.lines)
	}
	var first map[string]any
	_ = json.Unmarshal(f.server.lines[0], &first)
	if first["id"] != 1.0 || first["method"] != "initialize" {
		t.Fatalf("first = %s", f.server.lines[0])
	}
	params := first["params"].(map[string]any)
	client := params["clientInfo"].(map[string]any)
	if client["name"] != "codex_compare_limits" || client["version"] != "1" {
		t.Fatalf("clientInfo = %v", client)
	}
	if params["capabilities"].(map[string]any)["experimentalApi"] != true {
		t.Fatalf("capabilities = %v", params["capabilities"])
	}
	if string(f.server.lines[1]) != `{"method":"initialized"}` {
		t.Fatalf("second = %s", f.server.lines[1])
	}
	if string(f.server.lines[2]) != `{"id":2,"method":"account/rateLimits/read","params":null}` {
		t.Fatalf("third = %s", f.server.lines[2])
	}
	for _, p := range f.server.processes {
		if !p.terminated.Load() {
			t.Fatal("the app-server must be terminated after the read")
		}
		if p.killed.Load() {
			t.Fatal("a cooperative app-server must not be killed")
		}
	}
}

func TestEachProbeRunsInAPrivateCopyOfTheHome(t *testing.T) {
	f := newLimitsFixture(t)
	var seen []string
	f.server.handler = func(env []string, msg map[string]any) any {
		home := envValue(env, "CODEX_HOME")
		if msg["method"] == "initialize" {
			seen = append(seen, home)
			info, err := os.Stat(home)
			if err != nil {
				t.Errorf("probe home missing: %v", err)
			} else if info.Mode().Perm() != 0o700 {
				t.Errorf("probe home mode = %o", info.Mode().Perm())
			}
			if home == f.codex.Home || strings.HasPrefix(home, f.codex.Home+string(os.PathSeparator)) {
				t.Errorf("probe ran inside the real home: %s", home)
			}
			if !strings.HasPrefix(filepath.Base(home), "codex-limits-") {
				t.Errorf("probe home = %s", home)
			}
			if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
				t.Errorf("auth.json not copied: %v", err)
			}
		}
		return replay(func([]string) any { return packagedHelperPayload() })(env, msg)
	}
	f.codex.Compare(context.Background(), []string{"work"})
	if len(seen) != 2 {
		t.Fatalf("homes = %v", seen)
	}
	for _, home := range seen {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("probe home %s was not removed", home)
		}
	}
}

func TestARefreshedTokenIsWrittenBackToTheProfile(t *testing.T) {
	f := newLimitsFixture(t)
	refreshed := creds("work@example.com", "work-refreshed")
	f.server.handler = func(env []string, msg map[string]any) any {
		if msg["method"] == "account/rateLimits/read" {
			_ = os.WriteFile(filepath.Join(envValue(env, "CODEX_HOME"), "auth.json"), refreshed, 0o600)
		}
		return replay(func([]string) any { return packagedHelperPayload() })(env, msg)
	}
	f.codex.Compare(context.Background(), []string{"work"})
	got, _ := os.ReadFile(filepath.Join(f.codex.ProfilesDir(), "work", "auth.json"))
	if string(got) != string(refreshed) {
		t.Fatalf("profile auth = %s", got)
	}
	info, _ := os.Stat(filepath.Join(f.codex.ProfilesDir(), "work", "auth.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestARefreshedTokenIsWrittenBackEvenWhenTheReadingFails(t *testing.T) {
	// Codex refreshes the token before it answers. If the answer then fails and
	// the rotated credential is dropped, the profile keeps a refresh token the
	// server has already revoked and the account is locked out.
	f := newLimitsFixture(t)
	refreshed := creds("work@example.com", "work-refreshed")
	f.server.handler = func(env []string, msg map[string]any) any {
		if msg["method"] == "account/rateLimits/read" {
			_ = os.WriteFile(filepath.Join(envValue(env, "CODEX_HOME"), "auth.json"), refreshed, 0o600)
			return map[string]any{"id": msg["id"], "error": map[string]any{"code": -32603, "message": "401 Unauthorized"}}
		}
		return replay(func([]string) any { return packagedHelperPayload() })(env, msg)
	}
	for _, row := range f.codex.Compare(context.Background(), []string{"work"}) {
		if row.Name == "work" && row.Error == nil {
			t.Fatalf("work row = %+v, want an error row", row)
		}
	}
	got, _ := os.ReadFile(filepath.Join(f.codex.ProfilesDir(), "work", "auth.json"))
	if string(got) != string(refreshed) {
		t.Fatalf("profile auth = %s, want the rotated credential", got)
	}
}

func TestAnRPCErrorBecomesAnErrorRow(t *testing.T) {
	f := newLimitsFixture(t)
	f.server.handler = func(env []string, msg map[string]any) any {
		if msg["method"] == "initialize" {
			return map[string]any{"id": msg["id"], "result": map[string]any{}}
		}
		if msg["method"] == "account/rateLimits/read" {
			return map[string]any{"id": msg["id"], "error": json.RawMessage(`{"code": -32600, "message": "not signed in"}`)}
		}
		return nil
	}
	rows := f.codex.Compare(context.Background(), []string{"work"})
	if len(rows) != 2 || rows[0].Error == nil {
		t.Fatalf("rows = %+v", rows)
	}
	if *rows[0].Error != "{'code': -32600, 'message': 'not signed in'}" {
		t.Fatalf("error = %q", *rows[0].Error)
	}
	if rows[0].Email != "old@example.com" || rows[0].Name != LiveName {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestAnExitingAppServerIsReported(t *testing.T) {
	f := newLimitsFixture(t)
	var current *fakeProcess
	f.server.onSpawn = func(p *fakeProcess) { current = p }
	f.server.handler = func(env []string, msg map[string]any) any {
		if msg["method"] == "initialize" {
			go current.stdoutW.Close()
		}
		return nil
	}
	rows := f.codex.Compare(context.Background(), []string{"work"})
	for _, row := range rows {
		if row.Error == nil || *row.Error != "codex app-server exited" {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestASilentAppServerTimesOut(t *testing.T) {
	f := newLimitsFixture(t)
	f.codex.rpcTimeout = 50 * time.Millisecond
	f.server.handler = func([]string, map[string]any) any { return nil }
	start := time.Now()
	rows := f.codex.Compare(context.Background(), []string{"work"})
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied")
	}
	for _, row := range rows {
		if row.Error == nil || !strings.Contains(*row.Error, "timed out") {
			t.Fatalf("row = %+v", row)
		}
	}
	for _, p := range f.server.processes {
		if !p.terminated.Load() {
			t.Fatal("a silent app-server must still be terminated")
		}
	}
}

func TestResponsesAreMatchedByID(t *testing.T) {
	f := newLimitsFixture(t)
	var current *fakeProcess
	f.server.onSpawn = func(p *fakeProcess) { current = p }
	f.server.handler = func(env []string, msg map[string]any) any {
		p := current
		// A notification and a stale response precede the real answer.
		_, _ = p.stdoutW.Write([]byte(`{"method":"account/updated","params":{}}` + "\n"))
		_, _ = p.stdoutW.Write([]byte(`{"id":99,"result":{"rateLimits":{"rateLimitReachedType":"x"}}}` + "\n"))
		return replay(func([]string) any { return packagedHelperPayload() })(env, msg)
	}
	rows := f.codex.Compare(context.Background(), []string{"work"})
	for _, row := range rows {
		if row.Error != nil || !row.Usable {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestAProfileWithoutCredentialsIsSkipped(t *testing.T) {
	f := newLimitsFixture(t)
	rows := f.codex.Compare(context.Background(), []string{"missing", "work"})
	if len(rows) != 2 || rows[0].Name != LiveName || rows[1].Name != "work" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestErrorRowsSortLastAndSoonestResetFirst(t *testing.T) {
	f := newLimitsFixture(t)
	f.server.handler = replay(func(env []string) any {
		home := envValue(env, "CODEX_HOME")
		raw, _ := os.ReadFile(filepath.Join(home, "auth.json"))
		payload := packagedHelperPayload()
		switch {
		case strings.Contains(string(raw), `"work"`):
			payload["rateLimitsByLimitId"] = map[string]any{"codex": map[string]any{
				"primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": 1500000000}}}
		}
		return payload
	})
	rows := f.codex.Compare(context.Background(), nil)
	if rows[0].Name != "work" {
		t.Fatalf("soonest reset must come first: %v", []string{rows[0].Name, rows[1].Name, rows[2].Name})
	}
}

func TestWindowLabels(t *testing.T) {
	cases := map[string]*float64{"window": nil, "weekly": fl(10080), "2-day": fl(2880), "5-hour": fl(300),
		"90-min": fl(90), "1-day": fl(1440)}
	for want, minutes := range cases {
		if got := windowLabel(minutes); got != want {
			t.Errorf("windowLabel(%v) = %q, want %q", minutes, got, want)
		}
	}
	if windowLabel(fl(0)) != "window" {
		t.Error("zero minutes must be a plain window")
	}
}

func TestSummarizeReadsBothTiersAndPicksTheSoonestReset(t *testing.T) {
	payload := json.RawMessage(`{
		"accountId": "acc-9",
		"rateLimits": {"planType": "plus", "rateLimitReachedType": null, "spendControlReached": false},
		"rateLimitsByLimitId": {
			"codex": {"primary": {"usedPercent": 100, "windowDurationMins": 300, "resetsAt": 1700003600},
			          "secondary": {"usedPercent": 55.5, "windowDurationMins": 10080, "resetsAt": 1700000000}},
			"spark": {"primary": {"usedPercent": null, "windowDurationMins": 10080, "resetsAt": 1}}
		},
		"rateLimitResetCredits": {"availableCount": 2}
	}`)
	row := summarize("work", "w@example.com", payload)
	if len(row.Windows) != 2 {
		t.Fatalf("windows = %+v", row.Windows)
	}
	if row.Windows[0].Tier != "primary" || row.Windows[1].Tier != "secondary" || row.Windows[1].Label != "weekly" {
		t.Fatalf("windows = %+v", row.Windows)
	}
	if !row.Usable || row.Blocked || *row.Plan != "plus" || *row.AccountID != "acc-9" || row.ResetCredits != 2 {
		t.Fatalf("row = %+v", row)
	}
	if *row.ResetsAt != 1700000000 || row.SoonestWindow != "codex/secondary weekly" {
		t.Fatalf("soonest = %v %q", *row.ResetsAt, row.SoonestWindow)
	}
}

func TestSummarizeMarksBlockedAccounts(t *testing.T) {
	payload := json.RawMessage(`{"rateLimits": {"rateLimitReachedType": "primary"},
		"rateLimitsByLimitId": {"codex": {"primary": {"usedPercent": 5, "windowDurationMins": 300}}}}`)
	row := summarize("work", "w@example.com", payload)
	if !row.Blocked || row.Usable || row.ResetsAt != nil || row.ResetDisplay != "-" || row.SoonestWindow != "-" {
		t.Fatalf("row = %+v", row)
	}
	exhausted := summarize("work", "w", json.RawMessage(`{"rateLimits": {},
		"rateLimitsByLimitId": {"codex": {"primary": {"usedPercent": 100}}}}`))
	if exhausted.Usable || exhausted.Blocked {
		t.Fatalf("exhausted = %+v", exhausted)
	}
	empty := summarize("work", "w", json.RawMessage(`{}`))
	if empty.Usable || len(empty.Windows) != 0 || empty.Plan != nil {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestSummarizeKeepsTheServersKeyOrder(t *testing.T) {
	payload := json.RawMessage(`{"rateLimitsByLimitId": {
		"zeta": {"primary": {"usedPercent": 1}},
		"alpha": {"primary": {"usedPercent": 2}}}}`)
	row := summarize("w", "e", payload)
	if row.Windows[0].Limit != "zeta" || row.Windows[1].Limit != "alpha" {
		t.Fatalf("windows = %+v", row.Windows)
	}
}

func TestPyReprMatchesPythonStr(t *testing.T) {
	decoded, err := decodeOrdered([]byte(`{"b": [1, 2.5, true, null, "it's"], "a": {"x": "y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{'b': [1, 2.5, True, None, "it's"], 'a': {'x': 'y'}}`
	if got := pyRepr(decoded); got != want {
		t.Fatalf("pyRepr = %s, want %s", got, want)
	}
}

func TestFormatRowsListsWindows(t *testing.T) {
	out := FormatRows([]Row{
		{Name: "work", Email: "w@example.com", Plan: str("pro"), Usable: true, ResetCredits: 1,
			Windows: []Window{{Limit: "codex", Tier: "primary", Label: "weekly", UsedPercent: fl(40)}}},
		{Name: "old", Email: "o@example.com", Error: str(strings.Repeat("x", 80))},
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("out = %q", out)
	}
	if !strings.HasPrefix(lines[0], "work         usable   pro") || !strings.Contains(lines[1], "banked resets: 1") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(lines[2], "codex                  primary   weekly    40% used  resets -") {
		t.Fatalf("out = %q", out)
	}
	if lines[3] != "old          ERROR  "+strings.Repeat("x", 60) {
		t.Fatalf("out = %q", out)
	}
}

// rateLimitPayload is the shape the server returns, reduced to the fields that
// decide whether the reset is worth waiting for.
func rateLimitPayload(reachedType string, hasCredits bool) json.RawMessage {
	bucket := map[string]any{
		"limitId":              "codex",
		"primary":              map[string]any{"usedPercent": 100, "windowDurationMins": 10080, "resetsAt": 1789822714},
		"secondary":            nil,
		"credits":              map[string]any{"hasCredits": hasCredits, "unlimited": false},
		"spendControlReached":  false,
		"planType":             "self_serve_business_prolite",
		"rateLimitReachedType": reachedType,
	}
	raw, _ := json.Marshal(map[string]any{
		"ordinaryUsageAllowed": false,
		"rateLimits":           bucket,
		"rateLimitsByLimitId":  map[string]any{"codex": bucket},
		"accountId":            "acc-1",
	})
	return raw
}

func TestDepletedCreditsAreNotReportedAsWaitingForTheReset(t *testing.T) {
	// A workspace out of credits still has a weekly window that rolls over, and
	// rolling over does not add credits. Showing only the reset tells the user to
	// wait for a moment that will not unblock them.
	row := summarize("ecit", "work@example.com", rateLimitPayload("workspace_owner_credits_depleted", false))
	if !row.Blocked {
		t.Fatal("row should be blocked")
	}
	if row.NoResetReason != "no credits" {
		t.Fatalf("NoResetReason = %q", row.NoResetReason)
	}
	if row.ResetsAt == nil {
		t.Fatal("the window reset is still reported, it just is not the answer")
	}
}

func TestAnOrdinaryRateLimitKeepsItsResetAdvice(t *testing.T) {
	// hasCredits is false here too: a rate-limited Pro account reports it that
	// way, so only rateLimitReachedType can tell the two states apart.
	row := summarize("personal", "me@example.com", rateLimitPayload("rate_limit_reached", false))
	if !row.Blocked {
		t.Fatal("row should be blocked")
	}
	if row.NoResetReason != "" {
		t.Fatalf("NoResetReason = %q, want empty: this account does come back at the reset", row.NoResetReason)
	}
}

func TestAnUnknownBlockReasonDoesNotClaimTheResetIsUseless(t *testing.T) {
	row := summarize("new", "me@example.com", rateLimitPayload("some_future_reason", false))
	if row.NoResetReason != "" {
		t.Fatalf("NoResetReason = %q, want empty for an unrecognised reason", row.NoResetReason)
	}
}
