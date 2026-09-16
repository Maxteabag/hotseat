//go:build darwin

package claude

import "os/exec"

// defaultKeychainRun runs `security find-generic-password ...` for real.
func defaultKeychainRun(cmd *exec.Cmd) ([]byte, error) {
	return cmd.Output()
}
