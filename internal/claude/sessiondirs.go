package claude

// Per-account config directories, so a pinned session knows who it is.
//
// Passing CLAUDE_CODE_OAUTH_TOKEN authenticates a session correctly but leaves
// it anonymous: `claude auth status` reports no email and no organisation, so
// every pinned session looks identical to the default one. Identity is only
// reported when the session reads credentials from a file in its own config
// directory.
//
// These directories are per account and persistent, never throwaway. A session
// refreshes its own access token, and refresh rotates the refresh token; a
// temporary directory would discard the rotated value and leave the stored copy
// superseded, which locks the account out until someone signs in through a
// browser. One stable directory per account means one writer per account.
//
// Everything that is not identity or credentials is symlinked back to the
// shared config, so settings, skills, hooks and project history stay in one
// place.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SharedEntries are linked rather than copied, so edits in the shared config
// apply everywhere.
var SharedEntries = []string{
	"settings.json", "settings.local.json", "CLAUDE.md", "commands", "hooks",
	"skills", "plugins", "agents", "projects", "sessions", "history.jsonl",
	"file-history", "statsig", "ide", "todos",
}

// ErrPinnedIdentity is returned by Ensure when the directory already belongs to
// a different account. Nothing has been written when it is returned.
var ErrPinnedIdentity = errors.New("Pinned session directory belongs to another identity; restore it before launching")

// AccountsRoot is where the pinned session directories live: ~/.claude-accounts.
func AccountsRoot() string {
	return filepath.Join(HomeDir(), ".claude-accounts")
}

// SharedRoot is the shared configuration every pinned directory links back to.
func SharedRoot() string {
	return ConfigDir()
}

// readObject reads a JSON object leniently: absent, damaged or non-object input
// all yield an empty map.
func readObject(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	data, err := DecodeObject(raw)
	if err != nil || data == nil {
		return map[string]any{}
	}
	return data
}

// WritePrivate replaces a file atomically with mode 0600, never leaving a
// readable partial behind: temp file in the same directory, fsync, chmod, rename.
func WritePrivate(path string, payload []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hotseat-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// WritePrivateJSON marshals v with two-space indentation and writes it with
// WritePrivate, matching json.dumps(indent=2).
func WritePrivateJSON(path string, v any) error {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(path, payload)
}

// LinkWarning receives the message for a shared entry that could not be linked.
// The Python printed it; the Go binary runs a TUI on stdout, so it goes to
// stderr by default. Replaceable for tests or quieter callers.
var LinkWarning = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

func linkShared(target, shared string) {
	for _, name := range SharedEntries {
		source := filepath.Join(shared, name)
		link := filepath.Join(target, name)
		if _, err := os.Stat(source); err != nil {
			continue
		}
		if _, err := os.Lstat(link); err == nil {
			continue // a symlink, or a real file put there deliberately; leave it
		}
		if err := os.Symlink(source, link); err != nil {
			LinkWarning(fmt.Sprintf("hotseat: could not link %s into the session directory: %v", name, err))
		}
	}
}

// ExpiresAt is the access-token expiry (milliseconds) stored in a credentials
// object, or 0 when it is absent or unreadable.
func ExpiresAt(credentials map[string]any) int64 {
	oauth := getMap(credentials, "claudeAiOauth")
	if v, ok := oauth["expiresAt"]; ok && v != nil {
		if n, ok := asInt64(v); ok {
			return n
		}
	}
	return 0
}

// Ensure creates or updates the config directory for one account and returns
// its path. shared and root default to SharedRoot and AccountsRoot when empty;
// identity may be nil to leave the stored identity alone.
func Ensure(alias string, credentials, identity map[string]any, shared, root string) (string, error) {
	if shared == "" {
		shared = SharedRoot()
	}
	if root == "" {
		root = AccountsRoot()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}

	target := filepath.Join(root, alias)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return "", err
	}

	// Check the old identity before changing either the metadata or credentials.
	// A future expiry on an unrelated/dummy token must not win over this account.
	configPath := filepath.Join(target, ".claude.json")
	credsPath := filepath.Join(target, ".credentials.json")
	oldIdentity := getMap(readObject(configPath), "oauthAccount")
	oldCredentials := readObject(credsPath)
	if len(oldCredentials) > 0 && len(identity) > 0 {
		for _, field := range []string{"emailAddress", "organizationUuid"} {
			old, new := getString(oldIdentity, field), getString(identity, field)
			if old != "" && new != "" && old != new {
				return "", ErrPinnedIdentity
			}
		}
	}

	linkShared(target, shared)

	// Start from the shared configuration so servers and project settings carry
	// over, then replace only the identity.
	config := readObject(configPath)
	if len(config) == 0 {
		config = readObject(IdentityFile(shared))
	}
	if len(identity) > 0 {
		config["oauthAccount"] = identity
	}
	if err := WritePrivateJSON(configPath, config); err != nil {
		return "", err
	}

	// Never move an account backwards: the copy already here may have been
	// rotated by a running session, which supersedes whatever the caller holds.
	if ExpiresAt(readObject(credsPath)) <= ExpiresAt(credentials) {
		if err := WritePrivateJSON(credsPath, credentials); err != nil {
			return "", err
		}
	}
	return target, nil
}
