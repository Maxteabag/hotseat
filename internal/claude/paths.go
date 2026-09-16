package claude

// Where the things Hotseat reads live on disk.

import (
	"os"
	"os/user"
	"path/filepath"
)

// HomeDir mirrors Python's Path.home(): $HOME first, the password database
// second, and "." only when neither is usable so callers never see an empty path.
func HomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "."
}

// ConfigDir is Claude Code's configuration directory: CLAUDE_CONFIG_DIR when
// set, otherwise ~/.claude.
func ConfigDir() string {
	if override := os.Getenv("CLAUDE_CONFIG_DIR"); override != "" {
		return override
	}
	return filepath.Join(HomeDir(), ".claude")
}

// ProjectsDir holds the per-project transcript folders. It follows ConfigDir,
// so CLAUDE_CONFIG_DIR is honoured (design decision 1).
func ProjectsDir() string {
	return filepath.Join(ConfigDir(), "projects")
}

// DefaultConfigDir is ~/.claude regardless of CLAUDE_CONFIG_DIR. The identity
// file for the default configuration lives beside it, not inside it.
func DefaultConfigDir() string {
	return filepath.Join(HomeDir(), ".claude")
}

// IdentityFile returns the `.claude.json` that belongs to a config directory.
// The default configuration keeps it in the home folder; any other directory
// keeps it inside itself.
func IdentityFile(configDir string) string {
	if filepath.Clean(configDir) == filepath.Clean(DefaultConfigDir()) {
		return filepath.Join(HomeDir(), ".claude.json")
	}
	return filepath.Join(configDir, ".claude.json")
}
