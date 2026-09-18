package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file ports compare_limits: compare live Codex rate limits across saved
// accounts, isolated per profile.
//
// It answers "which saved account has usable quota, and which resets soonest?"
// without switching the live account. For each profile it copies auth.json into
// a private mode-700 temporary CODEX_HOME and reads `account/rateLimits/read`
// through an ephemeral `codex app-server --stdio`. Refreshed tokens are written
// back atomically.
//
// This reports the server's own numbers, unlike `codex-usage`, whose cached
// rollouts do not record account attribution. It does not switch accounts,
// redeem reset credits, or wake agents.

// rpcTimeout bounds one request/response exchange with the app-server.
const defaultRPCTimeout = 60 * time.Second

// Process is a running app-server the RPC client talks to over stdio.
type Process interface {
	Stdin() io.Writer
	Stdout() io.Reader
	// Terminate asks the process to exit (SIGTERM); Kill forces it.
	Terminate() error
	Kill() error
	// Wait blocks until the process has exited.
	Wait() error
}

// Spawner starts argv with the given environment and returns its stdio.
type Spawner func(ctx context.Context, argv []string, env []string) (Process, error)

// appServerArgv is the exact command the Python spawned.
func appServerArgv() []string {
	return []string{"codex", "-c", `cli_auth_credentials_store="file"`, "app-server", "--stdio"}
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	done   chan error
}

func (p *execProcess) Stdin() io.Writer  { return p.stdin }
func (p *execProcess) Stdout() io.Reader { return p.stdout }
func (p *execProcess) Terminate() error  { return p.cmd.Process.Signal(syscall.SIGTERM) }
func (p *execProcess) Kill() error       { return p.cmd.Process.Kill() }
func (p *execProcess) Wait() error       { return <-p.done }

// SpawnCommand is the Spawner used when none is injected.
func SpawnCommand(ctx context.Context, argv []string, env []string) (Process, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		done <- err
		close(done)
	}()
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, done: done}, nil
}

// rpc is the minimal JSON-RPC client the app-server needs: numbered requests,
// responses matched by id, notifications ignored.
type rpc struct {
	process Process
	lines   chan []byte
	quit    chan struct{}
	seq     int
	timeout time.Duration
}

type rpcRequest struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcResponse struct {
	ID     *float64        `json:"id"`
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
}

func openRPC(ctx context.Context, spawn Spawner, env []string, timeout time.Duration) (*rpc, error) {
	process, err := spawn(ctx, appServerArgv(), env)
	if err != nil {
		return nil, err
	}
	r := &rpc{process: process, lines: make(chan []byte, 64), quit: make(chan struct{}), timeout: timeout}
	go r.read()
	if _, err := r.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "codex_compare_limits", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		r.close()
		return nil, err
	}
	if _, err := io.WriteString(process.Stdin(), "{\"method\":\"initialized\"}\n"); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}

func (r *rpc) read() {
	scanner := bufio.NewScanner(r.process.Stdout())
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	defer close(r.lines)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case r.lines <- line:
		case <-r.quit:
			return
		}
	}
}

func (r *rpc) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.seq++
	request, err := json.Marshal(rpcRequest{ID: r.seq, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := r.process.Stdin().Write(append(request, '\n')); err != nil {
		return nil, fmt.Errorf("codex app-server exited")
	}
	deadline, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	for {
		select {
		case <-deadline.Done():
			return nil, fmt.Errorf("timed out after %s waiting for codex app-server %s", r.timeout, method)
		case line, ok := <-r.lines:
			if !ok {
				return nil, errors.New("codex app-server exited")
			}
			var msg rpcResponse
			if err := json.Unmarshal(line, &msg); err != nil {
				return nil, fmt.Errorf("codex app-server sent invalid JSON: %v", err)
			}
			if msg.ID == nil || int(*msg.ID) != r.seq || float64(int(*msg.ID)) != *msg.ID {
				continue
			}
			if msg.Error != nil {
				decoded, err := decodeOrdered(msg.Error)
				if err != nil {
					return nil, errors.New(string(msg.Error))
				}
				return nil, errors.New(pyRepr(decoded))
			}
			return msg.Result, nil
		}
	}
}

// close terminates the app-server, giving it five seconds to exit cleanly
// before killing it.
func (r *rpc) close() {
	close(r.quit)
	_ = r.process.Terminate()
	waited := make(chan struct{})
	go func() {
		_ = r.process.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		_ = r.process.Kill()
		<-waited
	}
}

// readProfile returns the rate-limit payload for one profile in an isolated
// home. The profile's auth.json is copied into a fresh mode-700 CODEX_HOME;
// if the app-server refreshed the token, the refreshed file is copied back.
func (c *Codex) readProfile(ctx context.Context, auth string) (json.RawMessage, error) {
	probeDir, err := os.MkdirTemp(c.tempDir(), "codex-limits-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(probeDir)
	if err := os.Chmod(probeDir, 0o700); err != nil {
		return nil, err
	}
	tempAuth := filepath.Join(probeDir, "auth.json")
	if err := copyFile(auth, tempAuth); err != nil {
		return nil, err
	}
	spawn := c.Spawn
	if spawn == nil {
		spawn = SpawnCommand
	}
	timeout := c.rpcTimeout
	if timeout == 0 {
		timeout = defaultRPCTimeout
	}
	client, err := openRPC(ctx, spawn, isolatedEnv(probeDir), timeout)
	if err != nil {
		// Opening the server is enough to refresh the token, so write back first.
		preserveRefreshed(tempAuth, auth)
		return nil, err
	}
	result, callErr := client.call(ctx, "account/rateLimits/read", nil)
	client.close()
	// Refreshed tokens are written back so the profile stays usable, including
	// when the reading failed: the rotation already happened and the old token is
	// already revoked. Failing to compare or copy is not a probe failure.
	preserveRefreshed(tempAuth, auth)
	if callErr != nil {
		return nil, callErr
	}
	return result, nil
}

// Window is one rate-limit window as the server reports it.
type Window struct {
	Limit         string   `json:"limit"`
	Tier          string   `json:"tier"`
	Label         string   `json:"label"`
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *float64 `json:"window_minutes"`
	ResetsAt      *float64 `json:"resets_at"`
}

// Row is one account's comparison result. A row whose Error is set carries only
// Name, Email and Error, as the Python error rows did.
type Row struct {
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	Error         *string  `json:"error,omitempty"`
	Plan          *string  `json:"plan"`
	Blocked       bool     `json:"blocked"`
	Usable        bool     `json:"usable"`
	AccountID     *string  `json:"account_id"`
	ResetsAt      *float64 `json:"resets_at"`
	ResetDisplay  string   `json:"reset_display"`
	SoonestWindow string   `json:"soonest_window"`
	Windows       []Window `json:"windows"`
	ResetCredits  float64  `json:"reset_credits"`
	// NoResetReason is set when the reset above will not unblock the account.
	NoResetReason string `json:"no_reset_reason,omitempty"`
}

// NoResetReason names why a blocked account will not recover when its window
// resets, or "" when waiting for the reset is the right advice.
//
// The server reports two independent things: a used-percent window with a
// resetsAt, and why usage was actually refused. A workspace whose credits are
// depleted still has a weekly window that rolls over, and rolling over does not
// add credits. Showing only the reset tells the user to wait for a moment that
// will not unblock them.
//
// Keyed on rateLimitReachedType alone. The credits object is not a usable
// signal: an ordinary rate-limited Pro account also reports hasCredits false.
func NoResetReason(reachedType string) string {
	// Kept short: this is rendered in a fixed-width status column.
	switch reachedType {
	case "workspace_owner_credits_depleted", "credits_depleted", "account_credits_depleted":
		return "no credits"
	case "spend_control_reached":
		return "spend cap"
	}
	return ""
}

func window(value any) *Window {
	obj, ok := value.(*orderedObject)
	if !ok {
		return nil
	}
	return &Window{
		UsedPercent:   numberPtr(obj.get("usedPercent")),
		WindowMinutes: numberPtr(obj.get("windowDurationMins")),
		ResetsAt:      numberPtr(obj.get("resetsAt")),
	}
}

// windowLabel names a window by its duration: weekly, N-day, N-hour, N-min.
func windowLabel(minutes *float64) string {
	if minutes == nil || *minutes == 0 {
		return "window"
	}
	m := int(*minutes)
	switch {
	case m%10080 == 0:
		return "weekly"
	case m%1440 == 0:
		return fmt.Sprintf("%d-day", m/1440)
	case m%60 == 0:
		return fmt.Sprintf("%d-hour", m/60)
	}
	return fmt.Sprintf("%d-min", m)
}

// FormatReset renders an epoch in local time; "-" when there is none.
func FormatReset(epoch *float64) string {
	if epoch == nil || *epoch == 0 {
		return "-"
	}
	return time.Unix(int64(*epoch), 0).Local().Format("2006-01-02 15:04 MST")
}

// summarize shapes an account/rateLimits/read payload into a Row.
func summarize(name, email string, payload json.RawMessage) Row {
	decoded, _ := decodeOrdered(payload)
	top, _ := decoded.(*orderedObject)
	bucket, _ := top.get("rateLimits").(*orderedObject)
	byID, _ := top.get("rateLimitsByLimitId").(*orderedObject)
	windows := []Window{}
	if byID != nil {
		for _, limitID := range byID.keys {
			lim, _ := byID.values[limitID].(*orderedObject)
			for _, tier := range []string{"primary", "secondary"} {
				w := window(lim.get(tier))
				if w != nil && w.UsedPercent != nil {
					w.Limit, w.Tier, w.Label = limitID, tier, windowLabel(w.WindowMinutes)
					windows = append(windows, *w)
				}
			}
		}
	}
	reached := stringOf(bucket.get("rateLimitReachedType"))
	blocked := truthy(bucket.get("rateLimitReachedType")) || truthy(bucket.get("spendControlReached"))
	noReset := NoResetReason(reached)
	if noReset == "" && truthy(bucket.get("spendControlReached")) {
		noReset = NoResetReason("spend_control_reached")
	}
	usable := false
	for _, w := range windows {
		if *w.UsedPercent < 100 {
			usable = true
			break
		}
	}
	usable = usable && !blocked
	// Server object key order is not stable, so pick the minimum explicitly.
	var soonest *Window
	for i := range windows {
		w := &windows[i]
		if w.ResetsAt == nil || *w.ResetsAt == 0 {
			continue
		}
		if soonest == nil || *w.ResetsAt < *soonest.ResetsAt {
			soonest = w
		}
	}
	row := Row{
		Name:          name,
		Email:         email,
		Plan:          stringPtr(bucket.get("planType")),
		Blocked:       blocked,
		Usable:        usable,
		AccountID:     stringPtr(top.get("accountId")),
		ResetDisplay:  "-",
		SoonestWindow: "-",
		Windows:       windows,
		NoResetReason: noReset,
	}
	if soonest != nil {
		row.ResetsAt = soonest.ResetsAt
		row.ResetDisplay = FormatReset(soonest.ResetsAt)
		row.SoonestWindow = fmt.Sprintf("%s/%s %s", soonest.Limit, soonest.Tier, soonest.Label)
	}
	if credits, ok := top.get("rateLimitResetCredits").(*orderedObject); ok {
		if count, ok := number(credits.get("availableCount")); ok {
			row.ResetCredits = count
		}
	}
	return row
}

// Compare probes the named profiles (all saved profiles when names is empty),
// with the live credential first when one exists. Profiles without a
// credential are skipped; a profile whose probe fails is an error row. Rows
// are sorted with errors last, then by soonest reset.
func (c *Codex) Compare(ctx context.Context, names []string) []Row {
	explicit := len(names) > 0
	if !explicit {
		names = c.SavedProfiles()
	}
	var live *Identity
	if fileExists(c.LiveAuth()) {
		live = c.identity(c.LiveAuth())
		names = append([]string{LiveName}, names...)
	}

	// The live credential and the profile that holds it are one account. Probing
	// both spawns two app-servers for it and refreshes it twice in one run,
	// which is exactly how one copy ends up holding a token the other rotated
	// away. Probe once and report the reading under both names.
	type target struct {
		name    string
		auth    string
		aliasOf string
	}
	targets := []target{}
	for _, name := range names {
		auth := filepath.Join(c.ProfilesDir(), name, "auth.json")
		if name == LiveName {
			auth = c.LiveAuth()
		}
		if !fileExists(auth) {
			continue
		}
		if !explicit && name != LiveName && live != nil && sameAccount(c.identity(auth), live) {
			targets = append(targets, target{name: name, auth: auth, aliasOf: LiveName})
			continue
		}
		targets = append(targets, target{name: name, auth: auth})
	}

	readings := make([]Row, len(targets))
	var wg sync.WaitGroup
	slots := make(chan struct{}, ProbeConcurrency)
	for i := range targets {
		if targets[i].aliasOf != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			readings[i] = c.read(ctx, targets[i].name, targets[i].auth)
		}(i)
	}
	wg.Wait()

	byName := map[string]Row{}
	for i, t := range targets {
		if t.aliasOf == "" {
			byName[t.name] = readings[i]
		}
	}
	rows := []Row{}
	for i, t := range targets {
		row := readings[i]
		if t.aliasOf != "" {
			row = byName[t.aliasOf]
			row.Name = t.name
			if identity := c.identity(t.auth); identity != nil && identity.Email != nil {
				row.Email = *identity.Email
			}
		}
		c.recordVerdict(t.name, t.auth, row)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Error != nil) != (b.Error != nil) {
			return a.Error == nil
		}
		return resetKey(a) < resetKey(b)
	})
	return rows
}

// sameAccount is the Accounts() rule: email and workspace together, because
// several users share one workspace id.
func sameAccount(a, b *Identity) bool {
	return a != nil && b != nil && a.AccountID != nil && *a.AccountID != "" &&
		ptrEqual(a.AccountID, b.AccountID) && ptrEqual(a.Email, b.Email)
}

// read probes one credential under its own time budget, so one unreachable
// account cannot consume the budget of the rest and leave them looking broken.
func (c *Codex) read(ctx context.Context, name, auth string) Row {
	email := Describe(auth).Email
	probeCtx, cancel := context.WithTimeout(ctx, ProfileProbeTimeout)
	defer cancel()
	payload, err := c.readProfile(probeCtx, auth)
	if err != nil {
		msg := err.Error()
		if probeCtx.Err() != nil && ctx.Err() == nil {
			msg = fmt.Sprintf("timed out after %s reading quota", ProfileProbeTimeout)
		}
		return Row{Name: name, Email: email, Error: &msg}
	}
	return summarize(name, email, payload)
}

// recordVerdict keeps the profile listing honest: this run just asked the
// server, so `list` need not go on calling a revoked credential "ok".
func (c *Codex) recordVerdict(name, auth string, row Row) {
	if name == LiveName || auth != filepath.Join(c.ProfilesDir(), name, "auth.json") {
		return
	}
	switch {
	case row.Error == nil:
		RecordVerdict(c.ProfilesDir(), name, "live", auth)
	case strings.Contains(*row.Error, "token_revoked"), strings.Contains(strings.ToLower(*row.Error), "revoked"):
		RecordVerdict(c.ProfilesDir(), name, "revoked", auth)
	}
}

func resetKey(row Row) float64 {
	if row.ResetsAt == nil || *row.ResetsAt == 0 {
		return 1e18
	}
	return *row.ResetsAt
}

// FormatRows renders the human listing the Python printed without --json.
func FormatRows(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		if r.Error != nil {
			msg := *r.Error
			if len([]rune(msg)) > 60 {
				msg = string([]rune(msg)[:60])
			}
			fmt.Fprintf(&b, "%-12s ERROR  %s\n", r.Name, msg)
			continue
		}
		mark := "BLOCKED"
		if r.Usable {
			mark = "usable"
		}
		plan := "?"
		if r.Plan != nil && *r.Plan != "" {
			plan = *r.Plan
		}
		fmt.Fprintf(&b, "%-12s %-8s %-28s %s\n", r.Name, mark, plan, r.Email)
		if r.ResetCredits != 0 {
			fmt.Fprintf(&b, "             banked resets: %s\n", formatNumber(r.ResetCredits))
		}
		windows := append([]Window(nil), r.Windows...)
		sort.SliceStable(windows, func(i, j int) bool {
			return derefOrZero(windows[i].ResetsAt) < derefOrZero(windows[j].ResetsAt)
		})
		for _, w := range windows {
			fmt.Fprintf(&b, "             %-22s %-9s %-6s %5s%% used  resets %s\n",
				w.Limit, w.Tier, w.Label, formatNumber(*w.UsedPercent), FormatReset(w.ResetsAt))
		}
	}
	return b.String()
}

func derefOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// formatNumber prints whole numbers without a fraction, like Python's str().
func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
