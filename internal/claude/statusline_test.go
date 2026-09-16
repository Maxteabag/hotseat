package claude

// The in-session account indicator.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func statusConfig(t *testing.T, path, email string) string {
	t.Helper()
	payload := map[string]any{}
	if email != "" {
		payload["oauthAccount"] = map[string]any{"emailAddress": email}
	}
	writeJSON(t, filepath.Join(path, ".claude.json"), payload)
	return path
}

func TestPinnedSessionNamesItsAccount(t *testing.T) {
	home := isolateHome(t)
	d := statusConfig(t, filepath.Join(home, ".claude-accounts", "work"), "user@example.com")
	if got := Describe(d); got != "work · user@example.com" {
		t.Fatalf("describe = %q", got)
	}
}

func TestSharedConfigIsMarkedAsTheDefault(t *testing.T) {
	home := isolateHome(t)
	d := statusConfig(t, filepath.Join(home, ".claude"), "user@example.com")
	if got := Describe(d); got != "default · user@example.com" {
		t.Fatalf("describe = %q", got)
	}
}

func TestPinnedSessionWithoutIdentityStillNamesTheAlias(t *testing.T) {
	home := isolateHome(t)
	d := statusConfig(t, filepath.Join(home, ".claude-accounts", "work"), "")
	if got := Describe(d); got != "work" {
		t.Fatalf("describe = %q", got)
	}
}

func TestDefaultIdentityIsFoundBesideTheConfigDirectory(t *testing.T) {
	// The shared configuration keeps .claude.json in the home folder.
	home := isolateHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "default@example.com"}})
	if got := Describe(filepath.Join(home, ".claude")); got != "default · default@example.com" {
		t.Fatalf("describe = %q", got)
	}
	// Without an explicit directory the session's own configuration is used.
	if got := Describe(""); got != "default · default@example.com" {
		t.Fatalf("describe() = %q", got)
	}
}

func TestUnreadableConfigPrintsNothingRatherThanBreaking(t *testing.T) {
	home := isolateHome(t)
	missing := filepath.Join(home, "nowhere")
	if got := Describe(missing); got != "" {
		t.Fatalf("describe = %q", got)
	}
	if got := Render(missing); got != "" {
		t.Fatalf("render = %q", got)
	}
}

func TestDamagedConfigIsSurvivable(t *testing.T) {
	home := isolateHome(t)
	d := filepath.Join(home, ".claude-accounts", "work")
	writeFile(t, filepath.Join(d, ".claude.json"), "{not json")
	if got := Describe(d); got != "work" {
		t.Fatalf("describe = %q", got)
	}
}

func TestRenderAddsAMarkerOnlyWhenThereIsSomethingToShow(t *testing.T) {
	home := isolateHome(t)
	d := statusConfig(t, filepath.Join(home, ".claude-accounts", "work"), "user@example.com")
	got := Render(d)
	if !strings.HasSuffix(got, "work · user@example.com") || !strings.HasPrefix(got, "◆ ") {
		t.Fatalf("render = %q", got)
	}
}

func TestPinnedDetectionFollowsSymlinks(t *testing.T) {
	home := isolateHome(t)
	real := filepath.Join(home, "elsewhere", "accounts")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".claude-accounts")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	d := statusConfig(t, filepath.Join(real, "work"), "")
	if !IsPinned(d) {
		t.Fatal("a directory reached through the symlinked accounts root is pinned")
	}
	if IsPinned(filepath.Join(home, ".claude")) {
		t.Fatal("the shared config is not pinned")
	}
}
