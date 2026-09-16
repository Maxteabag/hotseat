//go:build !darwin

package claude

import (
	"errors"
	"os/exec"
	"runtime"
)

// defaultKeychainRun never runs anything off macOS: there is no login Keychain
// to ask, so the backend falls through to its BackendError path.
func defaultKeychainRun(*exec.Cmd) ([]byte, error) {
	return nil, errors.New("the login Keychain is only available on macOS, not " + runtime.GOOS)
}
