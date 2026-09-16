package claude

// Linux and other file-based installs: credentials live in the config directory.
//
// Two sources are merged. The signed-in account always appears. Saved profiles,
// if the machine has any, appear alongside it. An account is matched to a
// profile by email *and* organisation: the same address can belong to several
// organisations, and treating those as one account is how credentials get
// overwritten.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SignedInAlias is the alias of the live account when it matches no saved profile.
const SignedInAlias = "(signed in)"

// LinuxBackend reads credentials and saved profiles from a config directory.
type LinuxBackend struct {
	root string
}

// NewLinuxBackend returns a file backend rooted at root, or at ConfigDir when
// root is empty.
func NewLinuxBackend(root string) *LinuxBackend {
	if root == "" {
		root = ConfigDir()
	}
	return &LinuxBackend{root: root}
}

func (b *LinuxBackend) Name() string            { return "file" }
func (b *LinuxBackend) HasProfiles() bool       { return true }
func (b *LinuxBackend) CanSwitch() bool         { return true }
func (b *LinuxBackend) Root() string            { return b.root }
func (b *LinuxBackend) ProfilesDir() string     { return filepath.Join(b.root, "profiles") }
func (b *LinuxBackend) CredentialsFile() string { return filepath.Join(b.root, ".credentials.json") }

func (b *LinuxBackend) Capabilities() Capabilities {
	return Capabilities{Backend: b.Name(), Profiles: b.HasProfiles(), Switch: b.CanSwitch()}
}

// --- pieces ----------------------------------------------------------------

// Identity is the `oauthAccount` object for the signed-in account: read from
// ~/.claude.json for the default config directory, or `<root>/.claude.json`
// for a custom one. Empty (never nil) when there is none.
func (b *LinuxBackend) Identity() (map[string]any, error) {
	data, err := ReadJSON(IdentityFile(b.root))
	if err != nil {
		return nil, err
	}
	identity := getMap(data, "oauthAccount")
	if identity == nil {
		identity = map[string]any{}
	}
	return identity, nil
}

func (b *LinuxBackend) active() (*Account, error) {
	creds, err := ReadJSON(b.CredentialsFile())
	if err != nil {
		return nil, err
	}
	oauth := getMap(creds, "claudeAiOauth")
	if oauth == nil || getString(oauth, "accessToken") == "" {
		return nil, nil
	}
	identity, err := b.Identity()
	if err != nil {
		return nil, err
	}
	account := accountFromOauth(SignedInAlias, oauth)
	account.Email = getString(identity, "emailAddress")
	account.Org = getString(identity, "organizationName")
	account.OrgUUID = getString(identity, "organizationUuid")
	account.IsActive = true
	return &account, nil
}

// TrackedAlias is the profile named by `profiles/.current_profile`, or "".
func (b *LinuxBackend) TrackedAlias() string {
	raw, err := os.ReadFile(filepath.Join(b.ProfilesDir(), ".current_profile"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (b *LinuxBackend) profiles() ([]Account, error) {
	entries, err := os.ReadDir(b.ProfilesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			// Not a directory (or absent): no saved profiles.
			return nil, nil
		}
		// Anything else (an unreadable directory, say) must not read as "no
		// profiles": Switch would then skip snapshotting the outgoing account.
		return nil, &BackendError{Msg: fmt.Sprintf("could not read %s: %v", filepath.Base(b.ProfilesDir()), err), Err: err}
	}
	var found []Account
	for _, entry := range entries {
		dir := filepath.Join(b.ProfilesDir(), entry.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		creds, err := ReadJSON(filepath.Join(dir, "credentials.json"))
		if err != nil {
			return nil, err
		}
		oauth := getMap(creds, "claudeAiOauth")
		if oauth == nil {
			continue
		}
		meta, err := ReadJSON(filepath.Join(dir, "meta.json"))
		if err != nil {
			return nil, err
		}
		account := accountFromOauth(entry.Name(), oauth)
		account.Email = getString(meta, "email")
		account.Org = getString(meta, "org")
		account.OrgUUID = getString(meta, "orgUuid")
		found = append(found, account)
	}
	return found, nil
}

// RawCredentials returns the full credentials from the same source selected by
// account discovery, with the matching identity. (nil, nil, nil) when the alias
// is unknown or holds no credentials.
func (b *LinuxBackend) RawCredentials(alias string) (map[string]any, map[string]any, error) {
	accounts, err := b.Accounts()
	if err != nil {
		return nil, nil, err
	}
	var account *Account
	for i := range accounts {
		if accounts[i].Alias == alias {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		return nil, nil, nil
	}
	if account.IsActive {
		credentials, err := ReadJSON(b.CredentialsFile())
		if err != nil {
			return nil, nil, err
		}
		if len(credentials) > 0 {
			identity, err := b.Identity()
			if err != nil {
				return nil, nil, err
			}
			return credentials, identity, nil
		}
	}
	entry := filepath.Join(b.ProfilesDir(), alias)
	credentials, err := ReadJSON(filepath.Join(entry, "credentials.json"))
	if err != nil {
		return nil, nil, err
	}
	if len(credentials) == 0 {
		return nil, nil, nil
	}
	identity, err := ReadJSON(filepath.Join(entry, "oauthAccount.json"))
	if err != nil {
		return nil, nil, err
	}
	return credentials, identity, nil
}

// --- api -------------------------------------------------------------------

// Accounts lists the signed-in account and the saved profiles. A profile that
// is the live account (same email and organisation) is shown as one row,
// preferring the live token, which is the one actually in use.
func (b *LinuxBackend) Accounts() ([]Account, error) {
	active, err := b.active()
	if err != nil {
		return nil, err
	}
	profiles, err := b.profiles()
	if err != nil {
		return nil, err
	}

	if active != nil && len(profiles) > 0 {
		var same []int
		for i, p := range profiles {
			if p.Email != "" && p.Email == active.Email && p.OrgUUID == active.OrgUUID {
				same = append(same, i)
			}
		}
		chosen := -1
		if len(same) > 0 {
			tracked := b.TrackedAlias()
			chosen = same[0]
			for _, i := range same {
				if profiles[i].Alias == tracked {
					chosen = i
					break
				}
			}
		}
		if chosen >= 0 {
			// The profile and the live file are the same account, so show one row,
			// preferring the live token which is the one actually in use.
			p := &profiles[chosen]
			p.IsActive = true
			if active.Plan != "" {
				p.Plan = active.Plan
			}
			if active.RateLimitTier != "" {
				p.RateLimitTier = active.RateLimitTier
			}
			if active.Token != "" {
				p.Token = active.Token
			}
			p.AccessExpiresAt = active.AccessExpiresAt
			p.RefreshExpiresAt = active.RefreshExpiresAt
			return profiles, nil
		}
		return append([]Account{*active}, profiles...), nil
	}

	if active != nil {
		return []Account{*active}, nil
	}
	if profiles == nil {
		profiles = []Account{}
	}
	return profiles, nil
}
