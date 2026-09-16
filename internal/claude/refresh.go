package claude

// Refresh a saved Claude profile's expired access token, through the official CLI.
//
// An access token lives about eight hours. A pinned session rotates its own as
// it works, but a profile nobody has used since yesterday just sits there
// expired, and quota cannot be read with a dead token. This brings it back
// without a browser, for as long as the refresh token is valid (roughly a month).
//
// The refresh is delegated to `claude` itself rather than reimplemented: a
// throwaway config directory is seeded with only this profile's credentials,
// the copy is backdated so the CLI considers it expired, and one minimal request
// drives the CLI's own refresh-and-rotate path. The shared config directory and
// every running session are never touched. Rotation issues a new refresh token
// as well, so the rotated copy is rescued to disk before anything else can fail.
//
// Before spending that request, a cheaper source is checked: the pinned session
// directory for the same alias may already hold a newer token rotated by a
// session.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRefreshMarginS is the margin used when HOTSEAT_REFRESH_MARGIN_S is
	// unset: refresh when less than this remains, so a launched session does not
	// expire moments after it starts.
	DefaultRefreshMarginS = 30 * 60
	// RetryCooldownS: after a failed automatic refresh, leave the account alone
	// for this long rather than spending a CLI call on every quota cycle.
	RetryCooldownS = 60 * 60
	// BackupKeep is how many dated copies of each kind are kept under backups/.
	BackupKeep = 10
	// CLITimeout bounds the one CLI request a refresh makes.
	CLITimeout = 120 * time.Second
)

// RefreshMarginS is REFRESH_MARGIN_S: seconds of remaining life below which a
// profile is refreshed pre-emptively. HOTSEAT_REFRESH_MARGIN_S overrides the
// default; an unparsable value falls back to it.
func RefreshMarginS() float64 {
	if raw := os.Getenv("HOTSEAT_REFRESH_MARGIN_S"); raw != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
			return float64(n)
		}
	}
	return DefaultRefreshMarginS
}

// RefreshError means the profile could not be refreshed. The stored credentials
// are unchanged.
type RefreshError struct {
	Msg string
	Err error
}

func (e *RefreshError) Error() string { return e.Msg }

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *RefreshError) Unwrap() error { return e.Err }

func refreshErrorf(err error, format string, args ...any) error {
	return &RefreshError{Msg: fmt.Sprintf(format, args...), Err: err}
}

// RunCLI runs a prepared `claude` command to completion, like (*exec.Cmd).Run.
// Tests inject one that rewrites the seeded credentials instead of spawning.
type RunCLI func(*exec.Cmd) error

// RefreshOptions tunes one refresh. The zero value is the default behaviour:
// honour the margin, run the real `claude` on PATH, use the wall clock and the
// real pinned-session root.
type RefreshOptions struct {
	// Force refreshes even when the token is still comfortably valid.
	Force bool
	// Margin overrides RefreshMarginS (seconds) when non-nil.
	Margin *float64
	// Run executes the CLI; nil means (*exec.Cmd).Run.
	Run RunCLI
	// Claude is the executable to run; "" means "claude".
	Claude string
	// Now is the current time in epoch seconds; 0 means time.Now.
	Now float64
	// SessionRoot is where pinned session directories live; "" means AccountsRoot.
	SessionRoot string
}

func (o RefreshOptions) margin() float64 {
	if o.Margin != nil {
		return *o.Margin
	}
	return RefreshMarginS()
}

func (o RefreshOptions) now() float64 {
	if o.Now != 0 {
		return o.Now
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func (o RefreshOptions) claude() string {
	if o.Claude != "" {
		return o.Claude
	}
	return "claude"
}

func (o RefreshOptions) run() RunCLI {
	if o.Run != nil {
		return o.Run
	}
	return func(cmd *exec.Cmd) error { return cmd.Run() }
}

// RefreshResult summarises one refresh; field names match the Python dict and
// the `hotseat refresh --json` output.
type RefreshResult struct {
	Alias     string `json:"alias"`
	Refreshed bool   `json:"refreshed"`
	// Source is "fresh", "session", "cli" or "unchanged".
	Source string `json:"source"`
	// ExpiresAt is the access-token expiry in epoch seconds.
	ExpiresAt float64 `json:"expires_at"`
	HoursLeft float64 `json:"hours_left"`
}

// --- storage ---------------------------------------------------------------

// ProfileDir is the saved profile behind an alias, or a RefreshError explaining
// why there is none.
func ProfileDir(backend Backend, alias string) (string, error) {
	store, ok := backend.(ProfileStore)
	if !ok {
		return "", &RefreshError{Msg: "this platform keeps credentials in the Keychain; run `claude` to refresh"}
	}
	if strings.HasPrefix(alias, "(") {
		return "", &RefreshError{Msg: "the signed-in account is not a saved profile; it refreshes when a session uses it"}
	}
	directory := filepath.Join(store.ProfilesDir(), alias)
	info, err := os.Stat(filepath.Join(directory, "credentials.json"))
	if err != nil || !info.Mode().IsRegular() {
		return "", &RefreshError{Msg: fmt.Sprintf("no saved profile named %s", pyRepr(alias))}
	}
	return directory, nil
}

// pyRepr renders a string the way Python's repr does for the common case, so
// error messages match the originals ('work').
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// readCredentials reads a credentials file, insisting it holds Claude OAuth
// credentials.
func readCredentials(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, refreshErrorf(err, "unreadable credentials at %s: %v", path, err)
	}
	var data any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, refreshErrorf(err, "unreadable credentials at %s: %v", path, err)
	}
	object, ok := data.(map[string]any)
	if !ok || getMap(object, "claudeAiOauth") == nil {
		return nil, &RefreshError{Msg: fmt.Sprintf("%s does not hold Claude OAuth credentials", path)}
	}
	return object, nil
}

// expiresAtSeconds is the access-token expiry in seconds since the epoch; 0
// when unknown.
func expiresAtSeconds(credentials map[string]any) float64 {
	return float64(ExpiresAt(credentials)) / 1000
}

// backup keeps a dated copy under backups/ and prunes old ones.
//
// Protects against a bad local write only. Once a newer refresh token exists,
// older copies are dead server-side no matter what is on disk.
func backup(directory, name string, data map[string]any, now float64) (string, error) {
	backups := filepath.Join(directory, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(backups, fmt.Sprintf("%s.%d.json", name, int64(now)))
	var payload []byte
	var err error
	if data == nil {
		payload, err = os.ReadFile(filepath.Join(directory, "credentials.json"))
	} else {
		payload, err = json.MarshalIndent(data, "", "  ")
	}
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, payload, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return "", err
	}
	for _, prefix := range []string{"credentials", "rotated"} {
		stale, err := filepath.Glob(filepath.Join(backups, prefix+".*.json"))
		if err != nil {
			continue
		}
		sort.Strings(stale)
		if len(stale) > BackupKeep {
			for _, path := range stale[:len(stale)-BackupKeep] {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return "", err
				}
			}
		}
	}
	return dest, nil
}

func store(directory string, credentials map[string]any, now float64) error {
	if _, err := backup(directory, "credentials", nil, now); err != nil {
		return err
	}
	return WritePrivateJSON(filepath.Join(directory, "credentials.json"), credentials)
}

// profileLock serialises refreshes so only one process ever rotates a
// profile's token. It is an exclusive flock on `<profile>/.lock`.
type profileLock struct {
	handle *os.File
}

func lockProfile(directory string) (*profileLock, error) {
	handle, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err := flockExclusive(handle); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return &profileLock{handle: handle}, nil
}

func (l *profileLock) release() {
	_ = funlock(l.handle)
	_ = l.handle.Close()
}

// --- the refresh -----------------------------------------------------------

// pinnedCredentials returns the credentials rotated by a pinned session for
// this alias, if that directory exists. path is "" when there is no such file;
// data is nil when the file exists but is unreadable or not OAuth credentials.
func pinnedCredentials(alias, sessionRoot string) (path string, data map[string]any) {
	if sessionRoot == "" {
		sessionRoot = AccountsRoot()
	}
	path = filepath.Join(sessionRoot, alias, ".credentials.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return path, nil
	}
	object, err := DecodeObject(raw)
	if err != nil || getMap(object, "claudeAiOauth") == nil {
		return path, nil
	}
	return path, object
}

// cloneJSON deep-copies a JSON object through an encode/decode round trip.
func cloneJSON(v map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return DecodeObject(raw)
}

// decodeAny decodes one JSON value of any shape with numbers preserved.
func decodeAny(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// withEnv returns environ with one variable set, replacing any earlier value.
func withEnv(environ []string, key, value string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == key {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

// envValue reads one variable from an os.Environ-style list; the last value wins.
func envValue(environ []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range environ {
		name, v, ok := strings.Cut(kv, "=")
		if ok && name == key {
			value, found = v, true
		}
	}
	return value, found
}

// RefreshCommand is the argv that drives the CLI's own refresh-and-rotate path:
// `auth status` only reports; a minimal request is what triggers a refresh.
func RefreshCommand(claude string) []string {
	return []string{claude, "--safe-mode", "--print", "--model", "haiku", "--tools", "",
		"--no-session-persistence", "--max-budget-usd", "0.02",
		"--output-format", "json", "--system-prompt", "Reply only OK.", "Reply OK."}
}

func refreshViaCLI(directory string, credentials map[string]any, run RunCLI, claude string, now float64) (map[string]any, error) {
	identity := filepath.Join(directory, "oauthAccount.json")
	work, err := os.MkdirTemp("", "hotseat-refresh-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	seeded, err := cloneJSON(credentials)
	if err != nil {
		return nil, err
	}
	// The CLI refreshes only when it considers the token expired, so backdate
	// this disposable copy. The stored profile is untouched.
	getMap(seeded, "claudeAiOauth")["expiresAt"] = json.Number(strconv.FormatInt(int64((now-60)*1000), 10))
	credPath := filepath.Join(work, ".credentials.json")
	if err := WritePrivateJSON(credPath, seeded); err != nil {
		return nil, err
	}
	// Re-read the seeded copy so the later "did anything change" comparison
	// sees exactly what the CLI saw on disk.
	seededRaw, err := os.ReadFile(credPath)
	if err != nil {
		return nil, err
	}
	seeded, err = DecodeObject(seededRaw)
	if err != nil {
		return nil, err
	}
	config := map[string]any{}
	if raw, err := os.ReadFile(identity); err == nil {
		if oauthAccount, err := decodeAny(raw); err == nil {
			config["oauthAccount"] = oauthAccount
		}
	}
	if err := WritePrivateJSON(filepath.Join(work, ".claude.json"), config); err != nil {
		return nil, err
	}

	env := withEnv(StripBlockedEnv(os.Environ()), "CLAUDE_CONFIG_DIR", work)
	ctx, cancel := context.WithTimeout(context.Background(), CLITimeout)
	defer cancel()
	argv := RefreshCommand(claude)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = work
	cmd.Env = env
	// The output is captured and ignored; the side effect on disk is the answer.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := run(cmd); err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil {
			return nil, refreshErrorf(ctx.Err(), "could not run %s: timed out after %d seconds", claude, int(CLITimeout/time.Second))
		}
		if !errors.As(err, &exit) {
			// A non-zero exit is the CLI's business (check=False); failing to
			// run it at all is ours.
			return nil, refreshErrorf(err, "could not run %s: %v", claude, err)
		}
	}

	raw, err := os.ReadFile(credPath)
	if err != nil {
		return nil, refreshErrorf(err, "the CLI left unreadable credentials behind: %v", err)
	}
	rotated, err := DecodeObject(raw)
	if err != nil {
		return nil, refreshErrorf(err, "the CLI left unreadable credentials behind: %v", err)
	}
	if !reflect.DeepEqual(rotated, seeded) {
		// A rotated refresh token exists only here until it is stored. Persist a
		// copy before anything else can fail; a crash now would lock the account out.
		if _, err := backup(directory, "rotated", rotated, now); err != nil {
			return nil, err
		}
	}
	return rotated, nil
}

// round2 mirrors Python's round(x, 2) closely enough for hours.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// Refresh brings a saved profile's access token back to full length.
//
// It returns a summary, or a RefreshError when nothing could be done; the
// stored credentials are then exactly as they were.
func Refresh(backend Backend, alias string, opts RefreshOptions) (RefreshResult, error) {
	now := opts.now()
	margin := opts.margin()
	directory, err := ProfileDir(backend, alias)
	if err != nil {
		return RefreshResult{}, err
	}
	lock, err := lockProfile(directory)
	if err != nil {
		return RefreshResult{}, err
	}
	defer lock.release()

	credentials, err := readCredentials(filepath.Join(directory, "credentials.json"))
	if err != nil {
		return RefreshResult{}, err
	}
	expires := expiresAtSeconds(credentials)
	remaining := expires - now
	result := RefreshResult{Alias: alias, ExpiresAt: expires, HoursLeft: round2(remaining / 3600)}
	if !opts.Force && remaining > margin {
		result.Source = "fresh"
		return result, nil
	}

	pinnedPath, pinned := pinnedCredentials(alias, opts.SessionRoot)
	if pinned != nil && expiresAtSeconds(pinned) > expires && expiresAtSeconds(pinned)-now > margin {
		if err := store(directory, pinned, now); err != nil {
			return RefreshResult{}, err
		}
		result.Refreshed = true
		result.Source = "session"
		result.ExpiresAt = expiresAtSeconds(pinned)
		result.HoursLeft = round2((expiresAtSeconds(pinned) - now) / 3600)
		return result, nil
	}

	rotated, err := refreshViaCLI(directory, credentials, opts.run(), opts.claude(), now)
	if err != nil {
		return RefreshResult{}, err
	}
	oauth := getMap(rotated, "claudeAiOauth")
	if getString(oauth, "accessToken") == "" {
		return RefreshResult{}, &RefreshError{Msg: "the CLI could not refresh this profile; its refresh token is probably " +
			"superseded or revoked, so a browser sign-in is needed"}
	}
	if getString(oauth, "accessToken") == getString(getMap(credentials, "claudeAiOauth"), "accessToken") {
		// The CLI declined to rotate. Report honestly rather than pretend; the
		// copy it saw was backdated on purpose, so its expiry says nothing.
		result.Source = "unchanged"
		return result, nil
	}
	newExpires := expiresAtSeconds(rotated)
	if newExpires <= now {
		return RefreshResult{}, &RefreshError{Msg: "the CLI returned an already-expired token"}
	}
	if err := store(directory, rotated, now); err != nil {
		return RefreshResult{}, err
	}
	if pinnedPath != "" && expiresAtSeconds(pinned) < newExpires {
		// Keep the pinned session directory from lagging behind the profile.
		if err := WritePrivateJSON(pinnedPath, rotated); err != nil {
			return RefreshResult{}, err
		}
	}
	result.Refreshed = true
	result.Source = "cli"
	result.ExpiresAt = newExpires
	result.HoursLeft = round2((newExpires - now) / 3600)
	return result, nil
}

// --- automatic use during quota reads ------------------------------------------

// MemoPath is where automatic-refresh failures are remembered:
// ${XDG_CACHE_HOME:-~/.cache}/hotseat/refresh-failures.json.
func MemoPath() string {
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(HomeDir(), ".cache")
	}
	return filepath.Join(cache, "hotseat", "refresh-failures.json")
}

// ReadMemo returns the remembered failures, {alias: {"at": seconds, "error": message}}.
// Absent or damaged memos read as empty.
func ReadMemo() map[string]any {
	raw, err := os.ReadFile(MemoPath())
	if err != nil {
		return map[string]any{}
	}
	data, err := decodeAny(raw)
	if err != nil {
		return map[string]any{}
	}
	object, ok := data.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

func writeMemo(data map[string]any) {
	path := MemoPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		Warn(fmt.Sprintf("hotseat: could not record refresh outcome: %v", err))
		return
	}
	if err := WritePrivateJSON(path, data); err != nil {
		Warn(fmt.Sprintf("hotseat: could not record refresh outcome: %v", err))
	}
}

// asFloat64 converts a decoded JSON value to a float the way Python's float()
// would for numbers and numeric strings.
func asFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// Auto refreshes an expired account during a quota read and returns the new
// token, updating the account's Token and expiry fields in place.
//
// Disabled with HOTSEAT_NO_REFRESH=1. A failure is remembered for
// RetryCooldownS so the TUI's refresh cycle does not spend a CLI call on the
// same dead profile every fifteen seconds.
func Auto(backend Backend, account *Account, opts RefreshOptions) (string, error) {
	now := opts.now()
	opts.Now = now
	if os.Getenv("HOTSEAT_NO_REFRESH") != "" {
		return "", &RefreshError{Msg: "automatic refresh disabled (HOTSEAT_NO_REFRESH)"}
	}
	memo := ReadMemo()
	last := getMap(memo, account.Alias)
	if last != nil {
		at, _ := asFloat64(last["at"])
		if now-at < RetryCooldownS {
			message := getString(last, "error")
			if message == "" {
				message = "refresh failed"
			}
			minutes := int((RetryCooldownS - (now - at)) / 60)
			return "", &RefreshError{Msg: fmt.Sprintf("%s (not retried for %d min)", message, minutes)}
		}
	}
	outcome, err := Refresh(backend, account.Alias, opts)
	if err != nil {
		var refreshErr *RefreshError
		if errors.As(err, &refreshErr) {
			memo[account.Alias] = map[string]any{"at": now, "error": err.Error()}
			writeMemo(memo)
		}
		return "", err
	}
	if _, remembered := memo[account.Alias]; remembered {
		delete(memo, account.Alias)
		writeMemo(memo)
	}
	if !outcome.Refreshed && outcome.Source != "fresh" {
		return "", &RefreshError{Msg: "the CLI did not rotate the token"}
	}
	directory, err := ProfileDir(backend, account.Alias)
	if err != nil {
		return "", err
	}
	credentials, err := readCredentials(filepath.Join(directory, "credentials.json"))
	if err != nil {
		return "", err
	}
	oauth := getMap(credentials, "claudeAiOauth")
	account.Token = getString(oauth, "accessToken")
	account.AccessExpiresAt = getInt64(oauth, "expiresAt")
	if _, present := oauth["refreshTokenExpiresAt"]; present {
		account.RefreshExpiresAt = getInt64(oauth, "refreshTokenExpiresAt")
	}
	return account.Token, nil
}

// Expired reports whether the stored access token can no longer be used: it
// has less than margin seconds left at now (epoch seconds; 0 means time.Now).
// An account with no known expiry is never reported expired.
func Expired(account Account, now float64, margin float64) bool {
	if now == 0 {
		now = float64(time.Now().UnixNano()) / 1e9
	}
	if account.AccessExpiresAt == nil || *account.AccessExpiresAt == 0 {
		return false
	}
	return float64(*account.AccessExpiresAt)/1000-now <= margin
}
