package claude

// macOS: Claude Code keeps OAuth credentials in the login Keychain.
//
// There is no profile store to enumerate, so this backend reports the one
// signed-in account and declares itself read-only. Writing Keychain items from a
// dashboard is deliberately out of scope: a mistake there costs the user their
// login.
//
// The Keychain item is addressed exactly as the CLI addresses it. With the
// default config directory the service is "Claude Code-credentials"; a custom
// CLAUDE_CONFIG_DIR appends a short hash of that path.
//
// Only the `security` invocation is macOS-specific (keychain_darwin.go); the
// service-name derivation and output parsing here are portable and tested on
// every platform.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	keychainServiceBase   = "Claude Code"
	keychainServiceSuffix = "-credentials"
	keychainTimeout       = 20 * time.Second
	// fallbackKeychainAccount is used when the user name cannot be trusted as a
	// Keychain account name.
	fallbackKeychainAccount = "claude-code-user"
)

var safeKeychainAccount = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// KeychainService mirrors how the CLI derives its Keychain service name.
func KeychainService() string {
	var scoped bool
	var root string
	if secureDir, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		scoped = secureDir != ""
		root = secureDir
	} else {
		configDir := os.Getenv("CLAUDE_CONFIG_DIR")
		scoped = configDir != ""
		root = configDir
	}
	if root == "" {
		root = filepath.Join(HomeDir(), ".claude")
	}
	service := keychainServiceBase + keychainServiceSuffix
	if scoped {
		sum := sha256.Sum256([]byte(root))
		service = service + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return service
}

// KeychainAccount is the account name the CLI stores its item under: the login
// name when it is plain enough, otherwise a fixed fallback.
func KeychainAccount() string {
	name := os.Getenv("USER")
	if name == "" {
		// getpass.getuser(): environment first, then the password database.
		for _, key := range []string{"LOGNAME", "LNAME", "USERNAME"} {
			if name = os.Getenv(key); name != "" {
				break
			}
		}
	}
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return fallbackKeychainAccount
		}
		name = u.Username
	}
	if !safeKeychainAccount.MatchString(name) {
		return fallbackKeychainAccount
	}
	return name
}

// RunOutput runs a prepared command and returns its standard output, like
// (*exec.Cmd).Output. Tests inject one to avoid spawning anything.
type RunOutput func(*exec.Cmd) ([]byte, error)

// DarwinBackend reads the single signed-in account from the login Keychain,
// falling back to the credentials file the CLI writes when the Keychain is
// unavailable.
type DarwinBackend struct {
	// Run executes the `security` command. Nil means the platform default:
	// the real command on macOS, an error elsewhere.
	Run RunOutput
}

func (b *DarwinBackend) Name() string      { return "keychain" }
func (b *DarwinBackend) HasProfiles() bool { return false }
func (b *DarwinBackend) CanSwitch() bool   { return false }

func (b *DarwinBackend) Capabilities() Capabilities {
	return Capabilities{Backend: b.Name(), Profiles: b.HasProfiles(), Switch: b.CanSwitch()}
}

// KeychainArgs is the exact `security` argv the CLI uses to find its item.
func KeychainArgs() []string {
	return []string{"security", "find-generic-password",
		"-a", KeychainAccount(), "-w", "-s", KeychainService()}
}

// exitCoder is satisfied by *exec.ExitError and by test doubles standing in
// for a command that ran and exited non-zero.
type exitCoder interface{ ExitCode() int }

func (b *DarwinBackend) fromKeychain() (map[string]any, error) {
	run := b.Run
	if run == nil {
		run = defaultKeychainRun
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	args := KeychainArgs()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := run(cmd)
	if ctx.Err() != nil {
		return nil, &BackendError{Msg: fmt.Sprintf("could not query the Keychain: %v", ctx.Err()), Err: ctx.Err()}
	}
	if err != nil {
		var exit exitCoder
		if errors.As(err, &exit) {
			// The item is missing or the Keychain is locked; the caller falls back.
			return nil, nil
		}
		return nil, &BackendError{Msg: fmt.Sprintf("could not query the Keychain: %v", err), Err: err}
	}
	return ParseKeychainOutput(out)
}

// ParseKeychainOutput decodes the password field printed by `security`.
func ParseKeychainOutput(out []byte) (map[string]any, error) {
	data, err := DecodeObject([]byte(strings.TrimSpace(string(out))))
	if err != nil {
		return nil, &BackendError{Msg: "the Keychain entry was not valid JSON", Err: err}
	}
	return data, nil
}

// Accounts returns the one signed-in account, or a BackendError explaining
// that nothing is signed in.
func (b *DarwinBackend) Accounts() ([]Account, error) {
	stored, err := b.fromKeychain()
	if err != nil {
		return nil, err
	}
	if stored == nil {
		// Keychain can be unavailable (locked, or a headless session), and the
		// CLI then falls back to a file. Try that before giving up.
		stored, err = ReadJSON(filepath.Join(HomeDir(), ".claude", ".credentials.json"))
		if err != nil {
			return nil, err
		}
	}
	oauth := getMap(stored, "claudeAiOauth")
	if oauth == nil || getString(oauth, "accessToken") == "" {
		return nil, &BackendError{Msg: "No Claude credentials found in the Keychain or the config directory. " +
			"Sign in with `claude auth login` first."}
	}
	config, err := ReadJSON(filepath.Join(HomeDir(), ".claude.json"))
	if err != nil {
		return nil, err
	}
	identity := getMap(config, "oauthAccount")
	account := accountFromOauth(SignedInAlias, oauth)
	account.Email = getString(identity, "emailAddress")
	account.Org = getString(identity, "organizationName")
	account.OrgUUID = getString(identity, "organizationUuid")
	account.IsActive = true
	return []Account{account}, nil
}
