//go:build unix

package codex

import "syscall"

// signalProcess is the Signaller used when none is injected.
func signalProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
