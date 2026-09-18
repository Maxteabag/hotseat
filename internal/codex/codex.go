// Package codex ports Hotseat's Codex-side modules: accounts, saved profiles,
// the app-server rate-limit client, reset credits, thread history and pinned
// launches.
//
// Codex stores one live credential at ~/.codex/auth.json and keeps saved accounts
// under ~/.codex/profiles/<name>/. Identity is not recorded separately: it lives in
// the claims of the stored id_token, which is read locally and never sent anywhere.
//
// Quota is a different shape from Claude's. An account can carry several named limit
// windows at once, primary and secondary, and being blocked on one does not mean the
// account is unusable. Rather than reimplement that, this reads the server's own
// numbers through an isolated app-server per profile and never switches the live
// account.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ProbeTimeout bounds one full quota probe. Each profile is probed through
	// its own app-server, one at a time, so a probe spawns one node process per
	// saved account. That is far too heavy to run on a refresh timer, hence the
	// cache below.
	ProbeTimeout = 300 * time.Second
	// LimitsTTL: Codex quota windows are hourly and weekly, so a stale reading
	// costs nothing and re-probing every few minutes costs a process per account.
	LimitsTTL = 1800 * time.Second
	// LiveName labels the credential in auth.json when no saved profile holds it.
	LiveName = "(live)"
	// UnsavedTag is shown beside an account that exists only in the live
	// credential file.
	UnsavedTag = "unsaved"

	authClaim = "https://api.openai.com/auth"
)

// Error mirrors CodexError: Codex is not installed here, or its accounts could
// not be read.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// DefaultHome resolves CODEX_HOME the way the Python module constants did:
// the environment variable, else ~/.codex. It is the only place the package
// reads the environment for a root.
func DefaultHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".codex")
}

// Codex reads the accounts under one CODEX_HOME. Every subprocess and HTTP
// dependency is a field so tests can replace it; nil fields use the real
// thing.
type Codex struct {
	// Home is CODEX_HOME.
	Home string
	// Spawn starts `codex app-server`; nil uses os/exec.
	Spawn Spawner
	// HTTP fetches reset-credit balances; nil uses http.DefaultClient.
	HTTP *http.Client
	// Which locates executables; nil uses exec.LookPath.
	Which func(string) (string, error)
	// TempDir hosts the per-profile probe homes; "" uses os.TempDir().
	TempDir string

	now        func() time.Time
	rpcTimeout time.Duration
	// limitsFn stands in for Limits in tests of the cache.
	limitsFn func(context.Context) map[string]Usage

	mu    sync.Mutex
	cache *limitsCache
}

// New returns a Codex rooted at home.
func New(home string) *Codex {
	return &Codex{Home: home}
}

// LiveAuth is the path of the live credential file.
func (c *Codex) LiveAuth() string { return filepath.Join(c.Home, "auth.json") }

// ProfilesDir holds one directory per saved profile.
func (c *Codex) ProfilesDir() string { return filepath.Join(c.Home, "profiles") }

// Available reports whether anything Codex-shaped exists under Home.
func (c *Codex) Available() bool {
	return fileExists(c.LiveAuth()) || isDir(c.ProfilesDir())
}

func (c *Codex) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Codex) which(name string) (string, error) {
	if c.Which != nil {
		return c.Which(name)
	}
	return exec.LookPath(name)
}

func (c *Codex) tempDir() string {
	if c.TempDir != "" {
		return c.TempDir
	}
	return os.TempDir()
}

func (c *Codex) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Claims decodes the identity claims of a JWT without verifying it.
//
// Verification would need OpenAI's signing keys and a network call. This is only
// used to label an account the user is already signed into, so the claims are
// read as-is and never trusted for authorisation. Anything malformed is an
// empty map, never an error.
func Claims(idToken string) map[string]any {
	if idToken == "" || strings.Count(idToken, ".") != 2 {
		return map[string]any{}
	}
	payload := decodeSegment(strings.Split(idToken, ".")[1])
	if payload == nil {
		return map[string]any{}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

// decodeSegment is base64.urlsafe_b64decode with the padding restored; the
// standard alphabet is accepted too, as Python's decoder does.
func decodeSegment(segment string) []byte {
	segment += strings.Repeat("=", (4-len(segment)%4)%4)
	if out, err := base64.URLEncoding.DecodeString(segment); err == nil {
		return out
	}
	if out, err := base64.StdEncoding.DecodeString(segment); err == nil {
		return out
	}
	return nil
}

// Identity is what the stored token says about an account. Pointers are nil
// where the Python emitted null.
type Identity struct {
	Email          *string  `json:"email"`
	Plan           *string  `json:"plan"`
	AccountID      *string  `json:"account_id"`
	TokenExpiresAt *float64 `json:"token_expires_at"`
	TokenHoursLeft *float64 `json:"token_hours_left"`
	APIKeyPresent  bool     `json:"api_key_present"`
}

func (c *Codex) identity(authFile string) *Identity {
	raw, err := os.ReadFile(authFile)
	if err != nil {
		return nil
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		return nil
	}
	tokens, _ := stored["tokens"].(map[string]any)
	claims := Claims(stringOf(tokens["id_token"]))
	auth, _ := claims[authClaim].(map[string]any)
	identity := &Identity{
		Email:         stringPtr(claims["email"]),
		Plan:          stringPtr(auth["chatgpt_plan_type"]),
		APIKeyPresent: truthy(stored["OPENAI_API_KEY"]),
	}
	if id := stringOf(auth["chatgpt_account_id"]); id != "" {
		identity.AccountID = &id
	} else {
		identity.AccountID = stringPtr(tokens["account_id"])
	}
	if expires, ok := number(claims["exp"]); ok {
		identity.TokenExpiresAt = &expires
		if expires != 0 {
			hours := (expires - float64(c.timeNow().UnixNano())/1e9) / 3600
			identity.TokenHoursLeft = &hours
		}
	}
	return identity
}

// SavedProfiles lists the profile names that hold a credential, sorted.
func (c *Codex) SavedProfiles() []string {
	entries, err := os.ReadDir(c.ProfilesDir())
	if err != nil {
		return []string{}
	}
	names := []string{}
	for _, entry := range entries {
		if fileExists(filepath.Join(c.ProfilesDir(), entry.Name(), "auth.json")) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Account is one row of Accounts: a saved profile or the live credential.
// Usage is filled by Overview and nil otherwise.
type Account struct {
	Provider string `json:"provider"`
	Alias    string `json:"alias"`
	IsActive bool   `json:"is_active"`
	Saved    bool   `json:"saved"`
	Identity
	Usage *Usage `json:"usage"`
}

// Accounts is every Codex account on this machine, live one included.
//
// The live credential is reported as its own entry when no saved profile holds
// the same account. An unsaved live account is easy to lose, so it is named
// rather than quietly folded into whichever profile happens to be listed first.
func (c *Codex) Accounts() []Account {
	var live *Identity
	if fileExists(c.LiveAuth()) {
		live = c.identity(c.LiveAuth())
	}
	found := []Account{}
	anyActive := false
	for _, name := range c.SavedProfiles() {
		identity := c.identity(filepath.Join(c.ProfilesDir(), name, "auth.json"))
		if identity == nil {
			continue
		}
		isLive := live != nil && identity.AccountID != nil && *identity.AccountID != "" &&
			ptrEqual(identity.AccountID, live.AccountID) && ptrEqual(identity.Email, live.Email)
		anyActive = anyActive || isLive
		reported := *identity
		if isLive {
			// Codex refreshes auth.json in place, so the saved copy of the account
			// in use can hold a superseded token. It is the same account either
			// way; report the credential actually being used rather than a stale
			// expiry for an account that is signed in.
			reported = *live
		}
		found = append(found, Account{Provider: "codex", Alias: name, IsActive: isLive,
			Saved: true, Identity: reported})
	}
	if live != nil && !anyActive {
		found = append([]Account{{Provider: "codex", Alias: LiveName, IsActive: true,
			Saved: false, Identity: *live}}, found...)
	}
	return found
}

// UsageWindow is one quota window as the TUI shows it: used is a 0..1 fraction.
type UsageWindow struct {
	Label string   `json:"label"`
	Used  float64  `json:"used"`
	Reset *float64 `json:"reset"`
}

// Usage is the shaped quota of one account, keyed by profile name in Limits.
type Usage struct {
	Usable  bool    `json:"usable"`
	Blocked bool    `json:"blocked"`
	Error   *string `json:"error"`
	// WorstUsed: one account can hold several named windows at once; the
	// highest is what actually constrains it.
	WorstUsed   *float64      `json:"worst_used"`
	Reset       *float64      `json:"reset"`
	ResetWindow *string       `json:"reset_window"`
	Windows     []UsageWindow `json:"windows"`
}

// limitsCache is (fetched_at, result). Process-local, so a CLI run never
// reuses a server's copy.
type limitsCache struct {
	fetchedAt time.Time
	readings  map[string]Usage
}

// CachedLimits returns quota from cache when it is fresh enough, otherwise a
// new probe.
//
// It returns the readings and the age of what was returned (known is false when
// there is no reading at all), so a caller can say how old the numbers are
// rather than implying they are current.
func (c *Codex) CachedLimits(ctx context.Context, maxAge time.Duration) (readings map[string]Usage, age float64, known bool) {
	now := c.timeNow()
	c.mu.Lock()
	cached := c.cache
	c.mu.Unlock()
	if cached != nil && now.Sub(cached.fetchedAt) <= maxAge {
		return cached.readings, now.Sub(cached.fetchedAt).Seconds(), true
	}
	fresh := c.limits(ctx)
	// Only cache a real reading. Caching a failure would hide a recovery for the
	// rest of the window.
	if len(fresh) > 0 {
		c.mu.Lock()
		c.cache = &limitsCache{fetchedAt: now, readings: fresh}
		c.mu.Unlock()
		return fresh, 0, true
	}
	if cached != nil {
		return cached.readings, now.Sub(cached.fetchedAt).Seconds(), true
	}
	return map[string]Usage{}, 0, false
}

func (c *Codex) limits(ctx context.Context) map[string]Usage {
	if c.limitsFn != nil {
		return c.limitsFn(ctx)
	}
	return c.Limits(ctx)
}

// Limits is the live quota per account, keyed by profile name.
//
// It runs the comparison in-process (the Python shelled out to compare_limits)
// and returns an empty mapping when Codex is unavailable: quota is an enrichment
// here, and missing it must not hide the accounts themselves.
func (c *Codex) Limits(ctx context.Context) map[string]Usage {
	if _, err := c.which("codex"); err != nil {
		return map[string]Usage{}
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	return shapeLimits(c.Compare(ctx, nil))
}

// shapeLimits turns comparison rows into the per-account Usage the TUI reads.
func shapeLimits(rows []Row) map[string]Usage {
	keyed := map[string]Usage{}
	for _, row := range rows {
		if row.Name == "" {
			continue
		}
		usage := Usage{Usable: row.Usable, Blocked: row.Blocked, Windows: []UsageWindow{}}
		if row.Error != nil {
			// An error row carries only name, email and error.
			usage.Error = row.Error
			usage.Usable, usage.Blocked = false, false
			keyed[row.Name] = usage
			continue
		}
		usage.Reset = row.ResetsAt
		window := row.SoonestWindow
		usage.ResetWindow = &window
		var worst *float64
		for _, w := range row.Windows {
			if w.UsedPercent == nil {
				continue
			}
			used := *w.UsedPercent
			if worst == nil || used > *worst {
				value := used
				worst = &value
			}
			label := strings.TrimSpace(fmt.Sprintf("%s %s %s", w.Limit, w.Tier, w.Label))
			usage.Windows = append(usage.Windows, UsageWindow{Label: label, Used: used / 100, Reset: w.ResetsAt})
		}
		if worst != nil {
			fraction := *worst / 100
			usage.WorstUsed = &fraction
		}
		keyed[row.Name] = usage
	}
	return keyed
}

// Overview is the `hotseat codex --json` shape: accounts with quota attached.
type Overview struct {
	Accounts    []Account `json:"accounts"`
	UnsavedLive *string   `json:"unsaved_live"`
	QuotaRead   bool      `json:"quota_read"`
	QuotaAgeS   *float64  `json:"quota_age_s"`
}

// Overview lists Codex accounts, with quota when it can be read.
//
// maxAge accepts a cached reading up to that old. Background callers should
// pass a generous value: probing spawns a process per account. Zero always
// probes when withLimits is set.
func (c *Codex) Overview(ctx context.Context, withLimits bool, maxAge time.Duration) (*Overview, error) {
	if !c.Available() {
		return nil, &Error{"No Codex installation found at ~/.codex."}
	}
	entries := c.Accounts()
	quota := map[string]Usage{}
	var age *float64
	if withLimits {
		if maxAge > 0 {
			readings, seconds, known := c.CachedLimits(ctx, maxAge)
			quota = readings
			if known {
				age = &seconds
			}
		} else {
			quota = c.limits(ctx)
			zero := 0.0
			age = &zero
		}
	}
	var unsaved *Account
	for i := range entries {
		entry := &entries[i]
		// The helper reports the live credential under its own name too.
		key := LiveName
		if entry.Saved {
			key = entry.Alias
		}
		usage, ok := quota[key]
		// Same reason: a reading taken from a superseded copy of the active
		// account reports a revoked token for an account that is signed in.
		if entry.Saved && entry.IsActive {
			if fromLive, found := quota[LiveName]; found {
				usage, ok = fromLive, true
			}
		}
		if ok {
			copied := usage
			entry.Usage = &copied
		}
		if !entry.Saved && unsaved == nil {
			unsaved = entry
		}
	}
	overview := &Overview{Accounts: entries, QuotaRead: len(quota) > 0, QuotaAgeS: age}
	if unsaved != nil {
		overview.UnsavedLive = unsaved.Email
	}
	return overview, nil
}
