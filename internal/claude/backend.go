package claude

// Shared shape for the per-platform credential backends, and the platform
// selection: macOS stores credentials in the Keychain; everything else uses files.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// BackendError means credentials could not be located or decoded on this platform.
type BackendError struct {
	Msg string
	Err error
}

func (e *BackendError) Error() string { return e.Msg }

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *BackendError) Unwrap() error { return e.Err }

// Capabilities describes what a backend can do; it is the `capabilities()` view
// published to the CLI and TUI.
type Capabilities struct {
	Backend  string `json:"backend"`
	Profiles bool   `json:"profiles"`
	Switch   bool   `json:"switch"`
}

// Backend is a source of accounts for one platform.
type Backend interface {
	// Name identifies the backend ("file" or "keychain").
	Name() string
	// HasProfiles reports whether this platform exposes more than the one
	// signed-in account.
	HasProfiles() bool
	// CanSwitch reports whether changing the machine-wide default account is
	// supported here.
	CanSwitch() bool
	Accounts() ([]Account, error)
	Capabilities() Capabilities
}

// ProfileStore is implemented by backends that keep saved profiles on disk.
// The refresh and launch code needs the directory and the full stored
// credentials, not just the Account view.
type ProfileStore interface {
	Backend
	// Root is the configuration directory the backend reads.
	Root() string
	// ProfilesDir is `<root>/profiles`.
	ProfilesDir() string
	// RawCredentials returns the full credentials and identity behind an alias,
	// from the same source account discovery selected. Both are nil when the
	// alias is unknown or has no stored credentials.
	RawCredentials(alias string) (credentials, identity map[string]any, err error)
}

// ForPlatform picks the backend for this machine. An empty platform means the
// running one.
func ForPlatform(platform string) Backend {
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform == "darwin" {
		return &DarwinBackend{}
	}
	return NewLinuxBackend("")
}

// ReadJSON reads a JSON object, returning (nil, nil) when the file is absent and
// a BackendError rather than a bare decoding error on damaged input. Numbers are
// kept as json.Number so credentials round-trip unchanged.
func ReadJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, &BackendError{Msg: fmt.Sprintf("could not read %s: %v", filepath.Base(path), err), Err: err}
	}
	data, err := DecodeObject(raw)
	if err != nil {
		return nil, &BackendError{Msg: fmt.Sprintf("could not read %s: %v", filepath.Base(path), err), Err: err}
	}
	return data, nil
}

// DecodeObject decodes one JSON object with numbers preserved as json.Number.
func DecodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// --- small accessors for the loosely typed JSON we read -------------------

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// getInt64 reads an integer field, accepting the forms a JSON number can take
// after decoding. Nil means absent or not a number.
func getInt64(m map[string]any, key string) *int64 {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	n, ok := asInt64(v)
	if !ok {
		return nil
	}
	return &n
}

// asInt64 converts a decoded JSON value to an integer the way Python's int()
// would, truncating floats. Strings are accepted because int("123") is.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case string:
		if i, err := strconv.ParseInt(t, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}
