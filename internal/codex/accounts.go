package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This file ports codex_accounts: the multi-account profile manager for the
// Codex CLI.
//
// Codex keeps exactly one credential set in ~/.codex/auth.json. This stores a
// copy per named profile and swaps the live file, so several ChatGPT accounts
// can share one machine without re-running the browser login each time.
//
// Identity and plan are read out of the id_token stored in the profile, so
// `list` works offline and never contacts OpenAI.

// ProfileError is what the Python reported through `_fail`: the message is
// shown to the user and the command exits 1.
type ProfileError struct{ Msg string }

func (e *ProfileError) Error() string { return e.Msg }

func fail(msg string) error { return &ProfileError{msg} }

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Profiles manages the saved profiles under one CODEX_HOME. Output that the
// Python printed goes to Out; nil discards it.
type Profiles struct {
	Home string
	// Run runs `codex login status` and the exec probe; nil uses os/exec.
	Run Runner
	// Call runs the interactive `codex login`; nil uses os/exec with the
	// user's terminal attached.
	Call Caller
	// HTTP fetches reset credits for the probe; nil uses http.DefaultClient.
	HTTP *http.Client
	// Out receives the messages the Python printed to stdout.
	Out io.Writer
	// TempDir hosts the isolated login and verify homes; "" uses os.TempDir().
	TempDir string

	now func() time.Time
	pid func() int
}

// NewProfiles returns a manager rooted at home, printing to out.
func NewProfiles(home string, out io.Writer) *Profiles {
	return &Profiles{Home: home, Out: out}
}

func (p *Profiles) AuthFile() string    { return filepath.Join(p.Home, "auth.json") }
func (p *Profiles) ProfilesDir() string { return filepath.Join(p.Home, "profiles") }
func (p *Profiles) CurrentProfileFile() string {
	return filepath.Join(p.ProfilesDir(), ".current_profile")
}
func (p *Profiles) SwitchedAtFile() string { return filepath.Join(p.ProfilesDir(), ".switched_at") }

func (p *Profiles) out() io.Writer {
	if p.Out == nil {
		return io.Discard
	}
	return p.Out
}

func (p *Profiles) printf(format string, args ...any) {
	fmt.Fprintf(p.out(), format, args...)
}

func (p *Profiles) run(ctx context.Context, argv, env []string) (Result, error) {
	if p.Run != nil {
		return p.Run(ctx, argv, env)
	}
	return RunCommand(ctx, argv, env)
}

func (p *Profiles) call(ctx context.Context, argv, env []string) (int, error) {
	if p.Call != nil {
		return p.Call(ctx, argv, env)
	}
	return CallCommand(ctx, argv, env)
}

func (p *Profiles) timeNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Profiles) processID() int {
	if p.pid != nil {
		return p.pid()
	}
	return os.Getpid()
}

func (p *Profiles) tempDir() string {
	if p.TempDir != "" {
		return p.TempDir
	}
	return os.TempDir()
}

// Described is the identity summary of an auth.json. It never carries token
// material. Unknown text fields are "?", as the Python showed them.
type Described struct {
	Email       string     `json:"email"`
	Plan        string     `json:"plan"`
	AccountID   string     `json:"account_id"`
	Mode        string     `json:"mode"`
	Expires     *time.Time `json:"expires"`
	Name        string     `json:"name"`
	Refreshable bool       `json:"refreshable"`
	LastRefresh string     `json:"last_refresh"`
}

// decodeIDToken returns the JWT payload. The signature is not verified: this
// is a local read of our own stored token, only ever used for display.
func decodeIDToken(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload := decodeSegment(parts[1])
	if payload == nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

// Describe summarises an auth.json for display.
func Describe(authPath string) Described {
	out := Described{Email: "?", Plan: "?", Mode: "?"}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return out
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return out
	}
	if mode := stringOf(data["auth_mode"]); mode != "" {
		out.Mode = mode
	} else if truthy(data["OPENAI_API_KEY"]) {
		out.Mode = "apikey"
	}
	tokens, _ := data["tokens"].(map[string]any)
	out.AccountID = stringOf(tokens["account_id"])
	// The id_token expires hourly by design and Codex refreshes it silently,
	// so its expiry says nothing about whether the profile works. The
	// refresh_token is what makes a stored profile reusable.
	out.Refreshable = truthy(tokens["refresh_token"])
	out.LastRefresh = stringOf(data["last_refresh"])
	claims := decodeIDToken(stringOf(tokens["id_token"]))
	if email, ok := claims["email"].(string); ok {
		out.Email = email
	}
	out.Name = stringOf(claims["name"])
	auth, _ := claims[authClaim].(map[string]any)
	if plan, ok := auth["chatgpt_plan_type"].(string); ok {
		out.Plan = plan
	}
	if exp, ok := number(claims["exp"]); ok && exp != 0 {
		expires := time.Unix(0, int64(exp*1e9)).UTC()
		out.Expires = &expires
	}
	return out
}

// Current is the profile last activated by this tool, or "".
func (p *Profiles) Current() string {
	raw, err := os.ReadFile(p.CurrentProfileFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ProfileDir validates a profile name and returns its directory.
func (p *Profiles) ProfileDir(name string) (string, error) {
	if !profileName.MatchString(name) {
		return "", fail("profile names must contain only letters, numbers, underscores or hyphens")
	}
	return filepath.Join(p.ProfilesDir(), name), nil
}

func loginStatusArgv() []string {
	return []string{"codex", "-c", `cli_auth_credentials_store="file"`, "login", "status"}
}

// status runs `codex login status` against an isolated home and returns its
// exit code. Exit code 1 means logged out.
func (p *Profiles) status(ctx context.Context, home string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := p.run(ctx, loginStatusArgv(), isolatedEnv(home))
	if err != nil {
		return -1, err
	}
	return result.Code, nil
}

// Verify checks a saved profile locally without switching to it.
func (p *Profiles) Verify(ctx context.Context, name string) error {
	dir, err := p.ProfileDir(name)
	if err != nil {
		return err
	}
	source := filepath.Join(dir, "auth.json")
	info := Describe(source)
	if !fileExists(source) || !info.Refreshable {
		return fail("saved ChatGPT profile missing or has no refresh token")
	}
	home, err := os.MkdirTemp(p.tempDir(), "codex-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	code, err := p.status(ctx, home)
	if err != nil {
		return err
	}
	if code != 1 {
		return fail("empty CODEX_HOME was not reported as logged out; isolation unverified")
	}
	if err := atomicCopy(source, filepath.Join(home, "auth.json")); err != nil {
		return err
	}
	code, err = p.status(ctx, home)
	if err != nil {
		return err
	}
	if code != 0 {
		return fail("Codex did not accept the saved profile")
	}
	p.printf("%s: %s — local login status accepted; server token validity and quota not tested\n", name, info.Email)
	return nil
}

// markSwitch stamps only a real change of account, so codex-usage can tell
// that an older reading belongs to someone else. Re-saving or re-activating
// the same account must not raise a warning, or the warning becomes noise.
func (p *Profiles) markSwitch(previousAccount, newAccount string) error {
	if previousAccount != "" && previousAccount == newAccount {
		return nil
	}
	if err := os.MkdirAll(p.ProfilesDir(), 0o777); err != nil {
		return err
	}
	return os.WriteFile(p.SwitchedAtFile(), []byte(isoZ(p.timeNow().UTC())), 0o644)
}

// isoZ is datetime.isoformat() with "+00:00" replaced by "Z": microseconds
// are shown only when non-zero.
func isoZ(t time.Time) string {
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05Z")
	}
	return t.Format("2006-01-02T15:04:05.000000Z")
}

// snapshot copies the live auth.json into the named profile.
func (p *Profiles) snapshot(name string) error {
	if !fileExists(p.AuthFile()) {
		return fail("no ~/.codex/auth.json to save - run `codex login` first")
	}
	target, err := p.ProfileDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o777); err != nil {
		return err
	}
	return atomicCopy(p.AuthFile(), filepath.Join(target, "auth.json"))
}

// Save snapshots the live login under a name and marks it current.
func (p *Profiles) Save(name string) error {
	if err := p.snapshot(name); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.CurrentProfileFile()), 0o777); err != nil {
		return err
	}
	if err := os.WriteFile(p.CurrentProfileFile(), []byte(name), 0o644); err != nil {
		return err
	}
	dir, _ := p.ProfileDir(name)
	info := Describe(filepath.Join(dir, "auth.json"))
	p.printf("saved profile '%s'  %s  plan=%s\n", name, info.Email, info.Plan)
	return nil
}

// ProfileInfo is one row of ListProfiles.
type ProfileInfo struct {
	Name string `json:"name"`
	Described
	// IsLive: the profile matches the live auth.json.
	IsLive bool `json:"is_live"`
	// IsCurrent: the profile was last activated by this tool.
	IsCurrent bool `json:"is_current"`
	// Auth is "api-key", "ok" or "no-refresh".
	Auth string `json:"auth"`
}

// Marker is "*" for a live profile, "~" for the current one, else " ".
func (i ProfileInfo) Marker() string {
	switch {
	case i.IsLive:
		return "*"
	case i.IsCurrent:
		return "~"
	}
	return " "
}

// ListProfiles describes every saved profile, sorted by name.
func (p *Profiles) ListProfiles() ([]ProfileInfo, error) {
	if err := os.MkdirAll(p.ProfilesDir(), 0o777); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p.ProfilesDir())
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		dir := filepath.Join(p.ProfilesDir(), entry.Name())
		if isDir(dir) && fileExists(filepath.Join(dir, "auth.json")) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	active := p.Current()
	var live *Described
	if fileExists(p.AuthFile()) {
		described := Describe(p.AuthFile())
		live = &described
	}
	found := []ProfileInfo{}
	for _, name := range names {
		info := Describe(filepath.Join(p.ProfilesDir(), name, "auth.json"))
		// A profile is live if its account_id matches the active auth.json;
		// the .current_profile marker alone can go stale if codex re-logs in.
		isLive := live != nil && ((live.AccountID != "" && live.AccountID == info.AccountID &&
			live.Email != "?" && live.Email == info.Email) ||
			(name == active && live.Mode == "apikey" && info.Mode == "apikey"))
		auth := "no-refresh"
		switch {
		case info.Mode == "apikey":
			auth = "api-key"
		case info.Refreshable:
			auth = "ok"
		}
		found = append(found, ProfileInfo{Name: name, Described: info, IsLive: isLive,
			IsCurrent: name == active, Auth: auth})
	}
	return found, nil
}

// List prints the saved profiles as a table.
func (p *Profiles) List() error {
	profiles, err := p.ListProfiles()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		p.printf("no saved profiles yet - run: hotseat codex-account save <name>\n")
		return nil
	}
	p.printf("%-2s %-16s %-32s %-8s %-8s AUTH\n", "", "PROFILE", "EMAIL", "PLAN", "MODE")
	for _, info := range profiles {
		p.printf("%-2s %-16s %-32s %-8s %-8s %s\n", info.Marker(), info.Name, info.Email,
			info.Plan, info.Mode, info.Auth)
	}
	p.printf("\n* = matches the live auth.json   ~ = last activated by this tool\n")
	return nil
}

// Switch activates a saved profile, snapshotting the live login first so an
// unsaved refresh is never lost.
func (p *Profiles) Switch(name string) error {
	target, err := p.ProfileDir(name)
	if err != nil {
		return err
	}
	if !fileExists(filepath.Join(target, "auth.json")) {
		return fail(fmt.Sprintf("no saved profile '%s' - try: hotseat codex-account list", name))
	}
	before := ""
	if fileExists(p.AuthFile()) {
		before = Describe(p.AuthFile()).AccountID
	}
	active := p.Current()
	if fileExists(p.AuthFile()) && active == "" {
		return fail("save the current login with hotseat codex-account save <name> before switching")
	}
	if fileExists(p.AuthFile()) && active != "" {
		activeDir, err := p.ProfileDir(active)
		if err != nil {
			return err
		}
		saved := Describe(filepath.Join(activeDir, "auth.json"))
		live := Describe(p.AuthFile())
		if saved.AccountID != before ||
			(live.Mode != "apikey" && (live.Email == "?" || saved.Email != live.Email)) {
			return fail("live account differs from the profile marker; save it under the correct name first")
		}
		if err := p.snapshot(active); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(p.ProfilesDir(), 0o777); err != nil {
		return err
	}
	if err := atomicCopy(filepath.Join(target, "auth.json"), p.AuthFile()); err != nil {
		return err
	}
	if err := os.WriteFile(p.CurrentProfileFile(), []byte(name), 0o644); err != nil {
		return err
	}
	info := Describe(p.AuthFile())
	if err := p.markSwitch(before, info.AccountID); err != nil {
		return err
	}
	p.printf("switched to '%s'  %s  plan=%s\n", name, info.Email, info.Plan)
	if !info.Refreshable && info.Mode != "apikey" {
		p.printf("warning: this profile has no refresh token; if Codex rejects it run: hotseat codex-account login %s\n", name)
	}
	return nil
}

// ShowCurrent prints the live account.
func (p *Profiles) ShowCurrent() error {
	if !fileExists(p.AuthFile()) {
		p.printf("not logged in\n")
		return nil
	}
	info := Describe(p.AuthFile())
	active := p.Current()
	if active == "" {
		active = "(unsaved)"
	}
	p.printf("email:   %s\n", info.Email)
	p.printf("name:    %s\n", info.Name)
	p.printf("plan:    %s\n", info.Plan)
	p.printf("mode:    %s\n", info.Mode)
	p.printf("profile: %s\n", active)
	return nil
}

// LoginOptions steer Login. Browser uses the standard OAuth flow instead of a
// device code; Force overwrites an existing saved profile.
type LoginOptions struct {
	Email   string
	Browser bool
	Force   bool
}

// Login approves a login in the user's browser and saves it without
// activation. It refuses to save if a different account signs in.
func (p *Profiles) Login(ctx context.Context, name string, opts LoginOptions) error {
	dir, err := p.ProfileDir(name)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "auth.json")
	if fileExists(target) && !opts.Force {
		return fail("profile already exists; use --force to overwrite, or choose a new name")
	}
	home, err := os.MkdirTemp(p.tempDir(), "codex-login-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	code, err := p.status(ctx, home)
	if err != nil {
		return err
	}
	if code != 1 {
		return fail("empty CODEX_HOME was not reported as logged out; refusing login")
	}
	var argv []string
	if opts.Browser {
		p.printf("Sign in via the browser as %s. The active profile will not be switched.\n", opts.Email)
		argv = []string{"codex", "-c", `cli_auth_credentials_store="file"`, "login"}
	} else {
		p.printf("Approve the device code in your own browser as %s. The active profile will not be switched.\n", opts.Email)
		argv = []string{"codex", "-c", `cli_auth_credentials_store="file"`, "login", "--device-auth"}
	}
	rc, err := p.call(ctx, argv, isolatedEnv(home))
	if err != nil {
		if ctx.Err() != nil {
			return fail("login cancelled; no profile saved")
		}
		return err
	}
	if rc != 0 {
		return fail(fmt.Sprintf("codex login exited %d; no profile saved", rc))
	}
	source := filepath.Join(home, "auth.json")
	info := Describe(source)
	if !strings.EqualFold(info.Email, opts.Email) || !info.Refreshable {
		return fail("login identity mismatch or missing refresh token; no profile saved")
	}
	code, err = p.status(ctx, home)
	if err != nil {
		return err
	}
	if code != 0 {
		return fail("Codex did not accept the new credentials; no profile saved")
	}
	if fileExists(target) && !opts.Force {
		return fail("profile was created during login; refusing to overwrite it")
	}
	if err := atomicCopy(source, target); err != nil {
		return err
	}
	p.printf("saved profile '%s'  %s  plan=%s\n", name, info.Email, info.Plan)
	p.printf("activate with: hotseat codex-account switch %s\n", name)
	return nil
}

// ProbeResult is the outcome of one isolated test request. Status is one of
// "ok", "revoked", "limit_reached", "unknown" or "error"; the other fields are
// present only when the Python emitted them.
type ProbeResult struct {
	Status       string          `json:"status"`
	Email        *string         `json:"email,omitempty"`
	Error        string          `json:"error,omitempty"`
	RateLimits   json.RawMessage `json:"rate_limits,omitempty"`
	Note         string          `json:"note,omitempty"`
	Stdout       string          `json:"stdout,omitempty"`
	Stderr       string          `json:"stderr,omitempty"`
	ResetCredits map[string]any  `json:"reset_credits,omitempty"`
}

func probeArgv() []string {
	return []string{"codex", "-c", `cli_auth_credentials_store="file"`, "exec", "--json",
		"--skip-git-repo-check", "respond with 1"}
}

// ProbeCredentials runs an isolated test request against OpenAI to check token
// validity and live quota.
func (p *Profiles) ProbeCredentials(ctx context.Context, authPath, profileName string) ProbeResult {
	if !fileExists(authPath) {
		return ProbeResult{Status: "error", Error: "credentials not found: " + authPath}
	}
	info := Describe(authPath)
	email := info.Email
	if profileName == "" {
		profileName = "live"
	}
	probeDir := filepath.Join(p.Home, fmt.Sprintf(".probe_%s_%d", profileName, p.processID()))
	if fileExists(probeDir) || isSymlink(probeDir) {
		_ = os.RemoveAll(probeDir)
	}
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		return ProbeResult{Status: "error", Email: &email, Error: err.Error()}
	}
	defer os.RemoveAll(probeDir)
	tempAuth := filepath.Join(probeDir, "auth.json")
	if err := copyFile(authPath, tempAuth); err != nil {
		return ProbeResult{Status: "error", Email: &email, Error: err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	proc, runErr := p.run(runCtx, probeArgv(), isolatedEnv(probeDir))

	// Preserve refreshed tokens if any, including after a failed run: the probe
	// may have rotated the credential before it failed, and the token it replaced
	// is already revoked.
	preserveRefreshed(tempAuth, authPath)

	if runErr != nil {
		return ProbeResult{Status: "error", Email: &email, Error: runErr.Error()}
	}

	credits := func() map[string]any { return FetchResetCredits(ctx, p.HTTP, authPath) }
	var errorMsg *string
	var rateLimits json.RawMessage
	for _, line := range strings.Split(proc.Stdout, "\n") {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(rec["type"], &kind)
		if kind == "error" {
			var message *string
			_ = json.Unmarshal(rec["message"], &message)
			errorMsg = message
		}
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(rec["payload"], &payload)
		if rl, ok := payload["rate_limits"]; ok {
			var decoded any
			if json.Unmarshal(rl, &decoded) == nil && truthy(decoded) {
				rateLimits = rl
			}
		}
	}

	if errorMsg != nil && *errorMsg != "" {
		lower := strings.ToLower(*errorMsg)
		if strings.Contains(lower, "revoked") || strings.Contains(lower, "refresh token") {
			return ProbeResult{Status: "revoked", Email: &email, Error: *errorMsg}
		}
		if strings.Contains(lower, "usage limit") || strings.Contains(lower, "out of credits") {
			return ProbeResult{Status: "limit_reached", Email: &email, Error: *errorMsg, ResetCredits: credits()}
		}
		return ProbeResult{Status: "error", Email: &email, Error: *errorMsg, ResetCredits: credits()}
	}
	if rateLimits != nil {
		return ProbeResult{Status: "ok", Email: &email, RateLimits: rateLimits, ResetCredits: credits()}
	}
	// Some Codex versions complete a turn without emitting a rate_limits
	// window. A successful turn with no error still proves the account is
	// accepted and has quota; do not mislabel that as UNKNOWN.
	if proc.Code == 0 && strings.Contains(proc.Stdout, `"turn.completed"`) {
		return ProbeResult{Status: "ok", Email: &email, RateLimits: json.RawMessage("null"),
			Note:         "turn completed; no rate-limit window reported by this Codex version",
			ResetCredits: credits()}
	}
	return ProbeResult{Status: "unknown", Email: &email, Stdout: proc.Stdout, Stderr: proc.Stderr,
		ResetCredits: credits()}
}

// Probe tests a profile (the live one when name is "") and prints the report,
// as JSON when asJSON is set.
func (p *Profiles) Probe(ctx context.Context, name string, asJSON bool) error {
	target := p.AuthFile()
	label := p.Current()
	if label == "" {
		label = "live"
	}
	if name != "" {
		dir, err := p.ProfileDir(name)
		if err != nil {
			return err
		}
		target = filepath.Join(dir, "auth.json")
		label = name
	}
	if !fileExists(target) {
		return fail(fmt.Sprintf("no credentials found for '%s'", label))
	}
	res := p.ProbeCredentials(ctx, target, label)
	rc := res.ResetCredits
	if rc == nil {
		rc = FetchResetCredits(ctx, p.HTTP, target)
	}
	if asJSON {
		res.ResetCredits = rc
		encoded, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		p.printf("%s\n", encoded)
		return nil
	}
	email := "?"
	if res.Email != nil {
		email = *res.Email
	}
	p.printf("Codex live probe - %s (%s)\n", label, email)
	switch res.Status {
	case "revoked":
		p.printf("  STATUS: ❌ REVOKED / EXPIRED\n")
		p.printf("  %s\n", res.Error)
		p.printf("  Fix: codex-login %s --email %s --force\n", label, email)
	case "limit_reached":
		p.printf("  STATUS: ⛔ USAGE LIMIT REACHED\n")
		p.printf("  %s\n", res.Error)
	case "ok":
		p.printf("  STATUS: ✅ ACTIVE\n")
		var rl map[string]map[string]any
		_ = json.Unmarshal(res.RateLimits, &rl)
		pri, sec := rl["primary"], rl["secondary"]
		if len(pri) > 0 {
			p.printf("  Primary (5-hour):    %5.1f%% used  (resets: %s)\n",
				derefOrZero(numberPtr(pri["used_percent"])), localClock(numberPtr(pri["resets_at"])))
		}
		if len(sec) > 0 {
			p.printf("  Secondary (weekly):  %5.1f%% used  (resets: %s)\n",
				derefOrZero(numberPtr(sec["used_percent"])), localClock(numberPtr(sec["resets_at"])))
		}
		if len(pri) == 0 && len(sec) == 0 {
			p.printf("  (no window data reported; account accepted a live test turn)\n")
		}
	default:
		status := res.Status
		if status == "" {
			status = "unknown"
		}
		p.printf("  STATUS: ⚠️ %s\n", strings.ToUpper(status))
		detail := res.Error
		if detail == "" {
			detail = res.Stderr
		}
		if detail == "" {
			detail = res.Stdout
		}
		p.printf("  %s\n", detail)
	}

	avail, _ := number(rc["available_count"])
	if avail > 0 {
		p.printf("\n  Banked resets:       %s available\n", formatNumber(avail))
		now := p.timeNow().UTC()
		creditRows, _ := rc["credits"].([]any)
		for _, item := range creditRows {
			credit, _ := item.(map[string]any)
			if stringOf(credit["status"]) != "available" {
				continue
			}
			title := stringOf(credit["title"])
			if title == "" {
				title = "Full reset"
			}
			expStr := "unknown"
			if expISO := stringOf(credit["expires_at"]); expISO != "" {
				expStr = expISO
				if expDT, err := time.Parse(time.RFC3339Nano, strings.Replace(expISO, "Z", "+00:00", 1)); err == nil {
					secs := int(expDT.Sub(now).Seconds())
					local := expDT.Local().Format("2006-01-02 15:04")
					if secs <= 0 {
						expStr = local + " (expired)"
					} else {
						expStr = fmt.Sprintf("%s (in %dd %dh)", local, secs/86400, secs%86400/3600)
					}
				}
			}
			p.printf("    - %s: expires %s\n", title, expStr)
		}
	}
	return nil
}

func localClock(epoch *float64) string {
	return time.Unix(int64(derefOrZero(epoch)), 0).Local().Format("2006-01-02 15:04")
}

// Remove deletes a saved profile; the live session is untouched.
func (p *Profiles) Remove(name string) error {
	target, err := p.ProfileDir(name)
	if err != nil {
		return err
	}
	if !fileExists(target) {
		return fail(fmt.Sprintf("no saved profile '%s'", name))
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if p.Current() == name {
		if err := os.WriteFile(p.CurrentProfileFile(), nil, 0o644); err != nil {
			return err
		}
	}
	p.printf("removed profile '%s' (the live session is untouched)\n", name)
	return nil
}
