package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// unsetenv clears a variable for the test and restores it afterwards; t.Setenv
// alone cannot express "absent", and absent is a distinct state for the
// Keychain service rules.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// isolateHome points HOME at a fresh directory so the default-identity
// fallbacks never see the real one, and clears the config overrides.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetenv(t, "CLAUDE_CONFIG_DIR")
	unsetenv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	return home
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw))
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := ReadJSON(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func aliases(accounts []Account) []string {
	out := []string{}
	for _, a := range accounts {
		out = append(out, a.Alias)
	}
	return out
}

func activeAliases(accounts []Account) []string {
	out := []string{}
	for _, a := range accounts {
		if a.IsActive {
			out = append(out, a.Alias)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
