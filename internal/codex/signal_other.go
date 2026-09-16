//go:build !unix

package codex

import (
	"errors"
	"syscall"
)

// signalProcess is the Signaller used when none is injected. Writer locks are
// only inspectable through /proc and fuser, so there is nothing to signal here.
func signalProcess(pid int, sig syscall.Signal) error {
	return errors.New("signalling processes is not supported on this platform")
}
