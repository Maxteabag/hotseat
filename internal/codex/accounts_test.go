package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func base64URL(raw []byte) string { return base64.URLEncoding.EncodeToString(raw) }

// credential is the test_codex_accounts fixture: a chatgpt-mode auth.json
// whose id_token carries only an email.
func credential(email, account string) []byte {
	claims, _ := json.Marshal(map[string]any{"email": email})
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token": "x." + base64URL(claims) + ".x", "refresh_token": "test-only", "account_id": account}})
	return raw
}

type profilesFixture struct {
	t        *testing.T
	home     string
	profiles *Profiles
	original []byte
	out      bytes.Buffer
	calls    []string
}

func newProfilesFixture(t *testing.T) *profilesFixture {
	t.Helper()
	f := &profilesFixture{t: t, home: t.TempDir()}
	f.profiles = NewProfiles(f.home, &f.out)
	f.profiles.TempDir = t.TempDir()
	if err := os.WriteFile(f.profiles.AuthFile(), credential("live@example.com", "live"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.original = f.read(f.profiles.AuthFile())
	return f
}

// withAccessToken rewrites the live credential with an access token, so the
// reset-credit lookup has something to send.
func (f *profilesFixture) withAccessToken() {
	f.t.Helper()
	var stored map[string]any
	_ = json.Unmarshal(f.original, &stored)
	stored["tokens"].(map[string]any)["access_token"] = "SENSITIVE"
	raw, _ := json.Marshal(stored)
	f.write(f.profiles.AuthFile(), raw)
	f.original = raw
}

func (f *profilesFixture) read(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *profilesFixture) write(path string, payload []byte) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// statusCodes makes `codex login status` answer the given exit codes in turn.
func (f *profilesFixture) statusCodes(codes ...int) {
	f.profiles.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		if argv[len(argv)-1] != "status" {
			f.t.Fatalf("unexpected command %q", argv)
		}
		if len(codes) == 0 {
			f.t.Fatal("login status called more often than expected")
		}
		code := codes[0]
		codes = codes[1:]
		return Result{Code: code}, nil
	}
}

func (f *profilesFixture) login(email string, rc int, force bool) error {
	f.statusCodes(1, 0)
	f.profiles.Call = func(ctx context.Context, argv, env []string) (int, error) {
		f.calls = append(f.calls, strings.Join(argv, " "))
		if strings.Join(argv[len(argv)-2:], " ") != "login --device-auth" {
			f.t.Fatalf("argv = %q", argv)
		}
		home := envValue(env, "CODEX_HOME")
		if home == f.home || home == "" {
			f.t.Fatalf("login must run in an isolated home, got %q", home)
		}
		f.write(filepath.Join(home, "auth.json"), credential(email, "new"))
		return rc, nil
	}
	return f.profiles.Login(context.Background(), "new", LoginOptions{Email: "new@example.com", Force: force})
}

func isProfileError(t *testing.T, err error) *ProfileError {
	t.Helper()
	var perr *ProfileError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ProfileError", err)
	}
	return perr
}

func TestLoginSavesWithoutSwitching(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.login("new@example.com", 0, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
		t.Fatal("the live credential changed")
	}
	if fileExists(f.profiles.CurrentProfileFile()) {
		t.Fatal("login must not activate the profile")
	}
	auth := filepath.Join(f.profiles.ProfilesDir(), "new", "auth.json")
	info, err := os.Stat(auth)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if Describe(auth).Email != "new@example.com" {
		t.Fatalf("email = %q", Describe(auth).Email)
	}
	if !strings.Contains(f.out.String(), "saved profile 'new'  new@example.com  plan=?") ||
		!strings.Contains(f.out.String(), "activate with: hotseat codex-account switch new") {
		t.Fatalf("out = %q", f.out.String())
	}
}

func TestMismatchAndFailureDoNotSave(t *testing.T) {
	for _, tc := range []struct {
		email string
		rc    int
		want  string
	}{
		{"wrong@example.com", 0, "login identity mismatch or missing refresh token; no profile saved"},
		{"new@example.com", 1, "codex login exited 1; no profile saved"},
	} {
		f := newProfilesFixture(t)
		err := f.login(tc.email, tc.rc, false)
		if perr := isProfileError(t, err); perr.Msg != tc.want {
			t.Fatalf("message = %q", perr.Msg)
		}
		if fileExists(filepath.Join(f.profiles.ProfilesDir(), "new", "auth.json")) {
			t.Fatal("profile saved despite failure")
		}
		if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
			t.Fatal("the live credential changed")
		}
	}
}

func TestExistingProfilePreserved(t *testing.T) {
	f := newProfilesFixture(t)
	target := filepath.Join(f.profiles.ProfilesDir(), "new", "auth.json")
	if err := atomicCopy(f.profiles.AuthFile(), target); err != nil {
		t.Fatal(err)
	}
	f.profiles.Call = func(context.Context, []string, []string) (int, error) {
		t.Fatal("login must not start")
		return 0, nil
	}
	f.profiles.Run = func(context.Context, []string, []string) (Result, error) {
		t.Fatal("no preflight before the existence check")
		return Result{}, nil
	}
	err := f.profiles.Login(context.Background(), "new", LoginOptions{Email: "new@example.com"})
	if perr := isProfileError(t, err); perr.Msg != "profile already exists; use --force to overwrite, or choose a new name" {
		t.Fatalf("message = %q", perr.Msg)
	}
	if !bytes.Equal(f.read(target), f.original) {
		t.Fatal("existing profile changed")
	}

	// But with force, it allows re-login and overwriting.
	if err := f.login("new@example.com", 0, true); err != nil {
		t.Fatal(err)
	}
	if Describe(target).Email != "new@example.com" {
		t.Fatalf("email = %q", Describe(target).Email)
	}
}

func TestFailedIsolationPreflightNeverStartsLogin(t *testing.T) {
	f := newProfilesFixture(t)
	f.statusCodes(0)
	f.profiles.Call = func(context.Context, []string, []string) (int, error) {
		t.Fatal("login must not start")
		return 0, nil
	}
	err := f.profiles.Login(context.Background(), "new", LoginOptions{Email: "new@example.com"})
	if perr := isProfileError(t, err); perr.Msg != "empty CODEX_HOME was not reported as logged out; refusing login" {
		t.Fatalf("message = %q", perr.Msg)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
		t.Fatal("the live credential changed")
	}
}

func TestBrowserLoginUsesTheOAuthFlow(t *testing.T) {
	f := newProfilesFixture(t)
	f.statusCodes(1, 0)
	f.profiles.Call = func(ctx context.Context, argv, env []string) (int, error) {
		if strings.Join(argv, " ") != `codex -c cli_auth_credentials_store="file" login` {
			t.Fatalf("argv = %q", argv)
		}
		f.write(filepath.Join(envValue(env, "CODEX_HOME"), "auth.json"), credential("New@Example.com", "new"))
		return 0, nil
	}
	if err := f.profiles.Login(context.Background(), "new", LoginOptions{Email: "new@example.com", Browser: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "Sign in via the browser as new@example.com. The active profile will not be switched.") {
		t.Fatalf("out = %q", f.out.String())
	}
}

func TestRejectPathTraversal(t *testing.T) {
	f := newProfilesFixture(t)
	for _, name := range []string{"../personal", "/tmp/account", ".", "", "-lead", strings.Repeat("a", 65), "name\n"} {
		_, err := f.profiles.ProfileDir(name)
		if perr := isProfileError(t, err); perr.Msg != "profile names must contain only letters, numbers, underscores or hyphens" {
			t.Fatalf("%q: message = %q", name, perr.Msg)
		}
	}
	if _, err := f.profiles.ProfileDir(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchRoundtripPreservesRefreshedCredentials(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.snapshot("live"); err != nil {
		t.Fatal(err)
	}
	f.write(f.profiles.CurrentProfileFile(), []byte("live"))
	f.write(filepath.Join(f.profiles.ProfilesDir(), "new", "auth.json"), credential("new@example.com", "new"))
	if err := f.profiles.Switch("new"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), credential("new@example.com", "new")) {
		t.Fatal("live credential not switched")
	}
	if f.profiles.Current() != "new" {
		t.Fatalf("current = %q", f.profiles.Current())
	}
	if err := f.profiles.Switch("live"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
		t.Fatal("roundtrip lost the original credential")
	}
	if !strings.Contains(f.out.String(), "switched to 'new'  new@example.com  plan=?") {
		t.Fatalf("out = %q", f.out.String())
	}
}

func TestStaleMarkerCannotOverwriteWrongProfile(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.snapshot("wrong"); err != nil {
		t.Fatal(err)
	}
	f.write(f.profiles.CurrentProfileFile(), []byte("wrong"))
	f.write(f.profiles.AuthFile(), credential("different@example.com", "different"))
	err := f.profiles.Switch("wrong")
	if perr := isProfileError(t, err); perr.Msg != "live account differs from the profile marker; save it under the correct name first" {
		t.Fatalf("message = %q", perr.Msg)
	}
	if !bytes.Equal(f.read(filepath.Join(f.profiles.ProfilesDir(), "wrong", "auth.json")), f.original) {
		t.Fatal("wrong profile overwritten")
	}
}

func TestSameWorkspaceDifferentUserCannotOverwriteProfile(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.snapshot("first"); err != nil {
		t.Fatal(err)
	}
	f.write(f.profiles.CurrentProfileFile(), []byte("first"))
	f.write(f.profiles.AuthFile(), credential("second@example.com", "live"))
	live := f.read(f.profiles.AuthFile())
	isProfileError(t, f.profiles.Switch("first"))
	if !bytes.Equal(f.read(filepath.Join(f.profiles.ProfilesDir(), "first", "auth.json")), f.original) {
		t.Fatal("first profile overwritten")
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), live) {
		t.Fatal("live credential changed")
	}
}

func TestUnsavedLiveLoginIsNotOverwrittenOnSwitch(t *testing.T) {
	f := newProfilesFixture(t)
	f.write(filepath.Join(f.profiles.ProfilesDir(), "target", "auth.json"), credential("target@example.com", "target"))
	err := f.profiles.Switch("target")
	if perr := isProfileError(t, err); perr.Msg != "save the current login with hotseat codex-account save <name> before switching" {
		t.Fatalf("message = %q", perr.Msg)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
		t.Fatal("live credential changed")
	}
}

func TestSwitchToAMissingProfileIsRefused(t *testing.T) {
	f := newProfilesFixture(t)
	err := f.profiles.Switch("ghost")
	if perr := isProfileError(t, err); perr.Msg != "no saved profile 'ghost' - try: hotseat codex-account list" {
		t.Fatalf("message = %q", perr.Msg)
	}
}

func TestSwitchStampsOnlyARealChangeOfAccount(t *testing.T) {
	f := newProfilesFixture(t)
	f.profiles.now = func() time.Time { return time.Date(2026, 9, 16, 12, 30, 5, 123456000, time.UTC) }
	if err := f.profiles.Save("live"); err != nil {
		t.Fatal(err)
	}
	if err := f.profiles.Switch("live"); err != nil {
		t.Fatal(err)
	}
	if fileExists(f.profiles.SwitchedAtFile()) {
		t.Fatal("re-activating the same account must not stamp a switch")
	}
	f.write(filepath.Join(f.profiles.ProfilesDir(), "new", "auth.json"), credential("new@example.com", "new"))
	if err := f.profiles.Switch("new"); err != nil {
		t.Fatal(err)
	}
	if got := string(f.read(f.profiles.SwitchedAtFile())); got != "2026-09-16T12:30:05.123456Z" {
		t.Fatalf("switched_at = %q", got)
	}
	if isoZ(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) != "2026-01-02T03:04:05Z" {
		t.Fatal("whole seconds must omit the fraction")
	}
}

func TestSaveRecordsTheCurrentProfile(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.Save("live"); err != nil {
		t.Fatal(err)
	}
	if f.profiles.Current() != "live" {
		t.Fatalf("current = %q", f.profiles.Current())
	}
	if !bytes.Equal(f.read(filepath.Join(f.profiles.ProfilesDir(), "live", "auth.json")), f.original) {
		t.Fatal("snapshot differs")
	}
	if f.out.String() != "saved profile 'live'  live@example.com  plan=?\n" {
		t.Fatalf("out = %q", f.out.String())
	}
	os.Remove(f.profiles.AuthFile())
	err := f.profiles.Save("other")
	if perr := isProfileError(t, err); perr.Msg != "no ~/.codex/auth.json to save - run `codex login` first" {
		t.Fatalf("message = %q", perr.Msg)
	}
}

func TestDescribeReadsIdentityWithoutTokenMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	claims, _ := json.Marshal(map[string]any{"email": "a@example.com", "name": "Ann", "exp": 1800000000,
		authClaim: map[string]any{"chatgpt_plan_type": "pro"}})
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": "2026-01-01T00:00:00Z",
		"tokens": map[string]any{"id_token": "h." + base64URL(claims) + ".s", "account_id": "acc", "refresh_token": "r"}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	info := Describe(path)
	if info.Email != "a@example.com" || info.Name != "Ann" || info.Plan != "pro" || info.AccountID != "acc" ||
		info.Mode != "chatgpt" || !info.Refreshable || info.LastRefresh != "2026-01-01T00:00:00Z" {
		t.Fatalf("info = %+v", info)
	}
	if info.Expires == nil || info.Expires.Unix() != 1800000000 {
		t.Fatalf("expires = %v", info.Expires)
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), "h.") || strings.Contains(string(encoded), `"r"`) {
		t.Fatalf("token material leaked: %s", encoded)
	}

	missing := Describe(filepath.Join(t.TempDir(), "none.json"))
	if missing.Email != "?" || missing.Plan != "?" || missing.Mode != "?" || missing.Refreshable {
		t.Fatalf("missing = %+v", missing)
	}
	apiKey := filepath.Join(t.TempDir(), "key.json")
	_ = os.WriteFile(apiKey, []byte(`{"OPENAI_API_KEY":"sk-x"}`), 0o600)
	if Describe(apiKey).Mode != "apikey" {
		t.Fatalf("mode = %q", Describe(apiKey).Mode)
	}
}

func TestListMarksLiveAndCurrentProfiles(t *testing.T) {
	f := newProfilesFixture(t)
	f.write(filepath.Join(f.profiles.ProfilesDir(), "live", "auth.json"), f.original)
	f.write(filepath.Join(f.profiles.ProfilesDir(), "other", "auth.json"), credential("other@example.com", "other"))
	f.write(filepath.Join(f.profiles.ProfilesDir(), "key", "auth.json"), []byte(`{"OPENAI_API_KEY":"sk"}`))
	f.write(filepath.Join(f.profiles.ProfilesDir(), "stale", "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"s"}}`))
	f.write(filepath.Join(f.profiles.ProfilesDir(), "nofile", "README"), []byte("x"))
	f.write(f.profiles.CurrentProfileFile(), []byte("other"))
	rows, err := f.profiles.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ProfileInfo{}
	order := []string{}
	for _, row := range rows {
		got[row.Name] = row
		order = append(order, row.Name)
	}
	if strings.Join(order, ",") != "key,live,other,stale" {
		t.Fatalf("order = %v", order)
	}
	if got["live"].Marker() != "*" || got["other"].Marker() != "~" || got["key"].Marker() != " " {
		t.Fatalf("markers = %+v", got)
	}
	if got["key"].Auth != "api-key" || got["live"].Auth != "ok" || got["stale"].Auth != "no-refresh" {
		t.Fatalf("auth = %+v", got)
	}
	if err := f.profiles.List(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(f.out.String(), "\n")
	if lines[0] != "   PROFILE          EMAIL                            PLAN     MODE     AUTH" {
		t.Fatalf("header = %q", lines[0])
	}
	if lines[2] != "*  live             live@example.com                 ?        chatgpt  ok" {
		t.Fatalf("row = %q", lines[2])
	}
	if lines[len(lines)-2] != "* = matches the live auth.json   ~ = last activated by this tool" {
		t.Fatalf("footer = %q", lines[len(lines)-2])
	}
}

func TestListWithoutProfilesExplainsHowToSave(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.List(); err != nil {
		t.Fatal(err)
	}
	if f.out.String() != "no saved profiles yet - run: hotseat codex-account save <name>\n" {
		t.Fatalf("out = %q", f.out.String())
	}
	if !isDir(f.profiles.ProfilesDir()) {
		t.Fatal("list must create the profiles directory")
	}
}

func TestCurrentShowsTheLiveAccount(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.ShowCurrent(); err != nil {
		t.Fatal(err)
	}
	want := "email:   live@example.com\nname:    \nplan:    ?\nmode:    chatgpt\nprofile: (unsaved)\n"
	if f.out.String() != want {
		t.Fatalf("out = %q", f.out.String())
	}
	f.out.Reset()
	os.Remove(f.profiles.AuthFile())
	_ = f.profiles.ShowCurrent()
	if f.out.String() != "not logged in\n" {
		t.Fatalf("out = %q", f.out.String())
	}
}

func TestRemoveDeletesTheProfileAndClearsTheMarker(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.Save("live"); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := f.profiles.Remove("live"); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(f.profiles.ProfilesDir(), "live")) {
		t.Fatal("profile directory remains")
	}
	if f.profiles.Current() != "" || !fileExists(f.profiles.CurrentProfileFile()) {
		t.Fatal("marker must be emptied, not removed")
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), f.original) {
		t.Fatal("the live session must be untouched")
	}
	if f.out.String() != "removed profile 'live' (the live session is untouched)\n" {
		t.Fatalf("out = %q", f.out.String())
	}
	err := f.profiles.Remove("live")
	if perr := isProfileError(t, err); perr.Msg != "no saved profile 'live'" {
		t.Fatalf("message = %q", perr.Msg)
	}
}

func TestVerifyChecksIsolationBeforeTheProfile(t *testing.T) {
	f := newProfilesFixture(t)
	if err := f.profiles.Save("live"); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	var homes []string
	f.profiles.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		if strings.Join(argv, " ") != `codex -c cli_auth_credentials_store="file" login status` {
			t.Fatalf("argv = %q", argv)
		}
		home := envValue(env, "CODEX_HOME")
		homes = append(homes, home)
		if fileExists(filepath.Join(home, "auth.json")) {
			return Result{Code: 0}, nil
		}
		return Result{Code: 1}, nil
	}
	if err := f.profiles.Verify(context.Background(), "live"); err != nil {
		t.Fatal(err)
	}
	if len(homes) != 2 || homes[0] != homes[1] || homes[0] == f.home || !strings.Contains(homes[0], "codex-verify-") {
		t.Fatalf("homes = %v", homes)
	}
	if fileExists(homes[0]) {
		t.Fatal("verify home not cleaned up")
	}
	if f.out.String() != "live: live@example.com — local login status accepted; server token validity and quota not tested\n" {
		t.Fatalf("out = %q", f.out.String())
	}

	f.statusCodes(0)
	err := f.profiles.Verify(context.Background(), "live")
	if perr := isProfileError(t, err); perr.Msg != "empty CODEX_HOME was not reported as logged out; isolation unverified" {
		t.Fatalf("message = %q", perr.Msg)
	}
	f.statusCodes(1, 1)
	err = f.profiles.Verify(context.Background(), "live")
	if perr := isProfileError(t, err); perr.Msg != "Codex did not accept the saved profile" {
		t.Fatalf("message = %q", perr.Msg)
	}
	f.write(filepath.Join(f.profiles.ProfilesDir(), "norefresh", "auth.json"), []byte(`{"tokens":{"account_id":"x"}}`))
	err = f.profiles.Verify(context.Background(), "norefresh")
	if perr := isProfileError(t, err); perr.Msg != "saved ChatGPT profile missing or has no refresh token" {
		t.Fatalf("message = %q", perr.Msg)
	}
}

func readCloser(body string) io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonClient(body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: readCloser(body), Header: http.Header{}}, nil
	})}
}

func TestProbeClassifiesTheExecOutcome(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		code   int
		status string
		check  func(t *testing.T, res ProbeResult)
	}{
		{"revoked", `{"type":"error","message":"Refresh token was revoked"}` + "\n", 1, "revoked",
			func(t *testing.T, res ProbeResult) {
				if res.Error != "Refresh token was revoked" || res.ResetCredits != nil {
					t.Fatalf("res = %+v", res)
				}
			}},
		{"limit", `{"type":"error","message":"You've hit your usage limit"}` + "\n", 1, "limit_reached",
			func(t *testing.T, res ProbeResult) {
				if res.ResetCredits["available_count"] != 3.0 {
					t.Fatalf("credits = %v", res.ResetCredits)
				}
			}},
		{"credits", `{"type":"error","message":"workspace is out of credits"}` + "\n", 1, "limit_reached", nil},
		{"other error", `{"type":"error","message":"boom"}` + "\n", 1, "error",
			func(t *testing.T, res ProbeResult) {
				if res.Error != "boom" || res.ResetCredits == nil {
					t.Fatalf("res = %+v", res)
				}
			}},
		{"ok", "garbage line\n" + `{"type":"item.completed","payload":{"rate_limits":{"primary":{"used_percent":12.5,"resets_at":1800000000}}}}` + "\n", 0, "ok",
			func(t *testing.T, res ProbeResult) {
				if !strings.Contains(string(res.RateLimits), `"used_percent":12.5`) || res.Note != "" {
					t.Fatalf("res = %+v", res)
				}
			}},
		{"turn completed", `{"type":"turn.completed","payload":{}}` + "\n", 0, "ok",
			func(t *testing.T, res ProbeResult) {
				if string(res.RateLimits) != "null" || res.Note != "turn completed; no rate-limit window reported by this Codex version" {
					t.Fatalf("res = %+v", res)
				}
				raw, _ := json.Marshal(res)
				if !strings.Contains(string(raw), `"rate_limits":null`) {
					t.Fatalf("json = %s", raw)
				}
			}},
		{"unknown", "nothing useful\n", 0, "unknown",
			func(t *testing.T, res ProbeResult) {
				if res.Stdout != "nothing useful\n" || res.Stderr != "stderr text" {
					t.Fatalf("res = %+v", res)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfilesFixture(t)
			f.withAccessToken()
			f.profiles.HTTP = jsonClient(`{"available_count":3,"credits":[]}`)
			f.profiles.pid = func() int { return 4242 }
			var probeHome string
			f.profiles.Run = func(ctx context.Context, argv, env []string) (Result, error) {
				if strings.Join(argv, " ") != `codex -c cli_auth_credentials_store="file" exec --json --skip-git-repo-check respond with 1` {
					t.Fatalf("argv = %q", argv)
				}
				probeHome = envValue(env, "CODEX_HOME")
				if probeHome != filepath.Join(f.home, ".probe_live_4242") {
					t.Fatalf("probe home = %q", probeHome)
				}
				info, err := os.Stat(probeHome)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("probe home stat = %v %v", info, err)
				}
				if !bytes.Equal(f.read(filepath.Join(probeHome, "auth.json")), f.original) {
					t.Fatal("credential not copied into the probe home")
				}
				return Result{Code: tc.code, Stdout: tc.stdout, Stderr: "stderr text"}, nil
			}
			res := f.profiles.ProbeCredentials(context.Background(), f.profiles.AuthFile(), "")
			if res.Status != tc.status {
				t.Fatalf("status = %q, res = %+v", res.Status, res)
			}
			if res.Email == nil || *res.Email != "live@example.com" {
				t.Fatalf("email = %v", res.Email)
			}
			if fileExists(probeHome) {
				t.Fatal("probe home not removed")
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

func TestProbePreservesRefreshedTokensAndReportsMissingCredentials(t *testing.T) {
	f := newProfilesFixture(t)
	f.profiles.HTTP = jsonClient(`{"available_count":0,"credits":[]}`)
	refreshed := credential("live@example.com", "live-refreshed")
	f.profiles.Run = func(ctx context.Context, argv, env []string) (Result, error) {
		f.write(filepath.Join(envValue(env, "CODEX_HOME"), "auth.json"), refreshed)
		return Result{Code: 0, Stdout: `{"type":"turn.completed"}` + "\n"}, nil
	}
	res := f.profiles.ProbeCredentials(context.Background(), f.profiles.AuthFile(), "work")
	if res.Status != "ok" {
		t.Fatalf("res = %+v", res)
	}
	if !bytes.Equal(f.read(f.profiles.AuthFile()), refreshed) {
		t.Fatal("refreshed token not written back")
	}
	missing := f.profiles.ProbeCredentials(context.Background(), "/nonexistent/auth.json", "x")
	if missing.Status != "error" || missing.Error != "credentials not found: /nonexistent/auth.json" || missing.Email != nil {
		t.Fatalf("missing = %+v", missing)
	}
	f.profiles.Run = func(context.Context, []string, []string) (Result, error) {
		return Result{}, errors.New("codex timed out")
	}
	failed := f.profiles.ProbeCredentials(context.Background(), f.profiles.AuthFile(), "")
	if failed.Status != "error" || failed.Error != "codex timed out" || *failed.Email != "live@example.com" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestProbePrintsAReport(t *testing.T) {
	f := newProfilesFixture(t)
	f.withAccessToken()
	f.profiles.HTTP = jsonClient(`{"available_count":1,"credits":[{"status":"available","title":"Full reset","expires_at":"2099-01-01T00:00:00Z"},{"status":"used"}]}`)
	f.profiles.Run = func(context.Context, []string, []string) (Result, error) {
		return Result{Code: 0, Stdout: `{"type":"x","payload":{"rate_limits":{"primary":{"used_percent":12.5,"resets_at":1800000000},"secondary":{"used_percent":3,"resets_at":1800000000}}}}` + "\n"}, nil
	}
	if err := f.profiles.Probe(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	for _, want := range []string{"Codex live probe - live (live@example.com)", "STATUS: ✅ ACTIVE",
		"Primary (5-hour):     12.5% used  (resets: ", "Secondary (weekly):    3.0% used",
		"Banked resets:       1 available", "- Full reset: expires 2099-01-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "used: expires") {
		t.Fatalf("used credits must be skipped:\n%s", out)
	}

	f.out.Reset()
	if err := f.profiles.Probe(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(f.out.Bytes(), &decoded); err != nil {
		t.Fatalf("json output: %v\n%s", err, f.out.String())
	}
	if decoded["status"] != "ok" || decoded["reset_credits"].(map[string]any)["available_count"] != 1.0 {
		t.Fatalf("json = %v", decoded)
	}

	err := f.profiles.Probe(context.Background(), "ghost", false)
	if perr := isProfileError(t, err); perr.Msg != "no credentials found for 'ghost'" {
		t.Fatalf("message = %q", perr.Msg)
	}
}
