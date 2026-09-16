package claude

// A one-line account indicator for the Claude Code status line.
//
// A pinned session is easy to mistake for the default one, which is the whole
// reason the launch behaviour had to change. This makes the answer visible
// inside the session itself.
//
// It runs on every status line refresh, so it only reads local files. No
// network, no subprocesses, and any failure degrades to printing nothing rather
// than breaking the status line.

import (
	"path/filepath"
)

// sessionIdentity finds the identity for this session.
//
// A session with its own config directory keeps `.claude.json` inside it. The
// default configuration keeps it beside the directory, in the home folder, so
// both locations have to be tried.
func sessionIdentity(configDir string) map[string]any {
	for _, candidate := range []string{
		filepath.Join(configDir, ".claude.json"),
		filepath.Join(HomeDir(), ".claude.json"),
	} {
		account := getMap(readObject(candidate), "oauthAccount")
		if len(account) > 0 {
			return account
		}
	}
	return map[string]any{}
}

// resolvePath follows symlinks like Path.resolve(strict=False): a path that
// does not exist yet resolves through its nearest existing ancestor.
func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	parent, base := filepath.Split(abs)
	parent = filepath.Clean(parent)
	if parent == abs {
		return abs
	}
	return filepath.Join(resolvePath(parent), base)
}

// IsPinned reports whether a config directory is one of the per-account
// directories under AccountsRoot.
func IsPinned(configDir string) bool {
	return filepath.Dir(resolvePath(configDir)) == resolvePath(AccountsRoot())
}

// Describe returns a short label for the account this session is using. An
// empty configDir means the session's own (CLAUDE_CONFIG_DIR or ~/.claude).
func Describe(configDir string) string {
	if configDir == "" {
		configDir = ConfigDir()
	}
	email := getString(sessionIdentity(configDir), "emailAddress")

	alias := ""
	if IsPinned(configDir) {
		alias = filepath.Base(configDir)
	}

	switch {
	case alias != "" && email != "":
		return alias + " · " + email
	case alias != "":
		return alias
	case email != "":
		// The shared configuration, so this session follows the machine default.
		return "default · " + email
	}
	return ""
}

// Render is Describe with the status-line marker, or "" when there is nothing
// to show.
func Render(configDir string) string {
	if label := Describe(configDir); label != "" {
		return "◆ " + label
	}
	return ""
}
