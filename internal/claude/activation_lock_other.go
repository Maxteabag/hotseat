//go:build !linux

package claude

import "time"

// Machine-default activation is supported by the Linux file backend. Do not
// claim shared-file exclusion on another platform using a no-op lock.
type activationFileLock struct{}

func lockSwitchFile(string, string, time.Time) (*activationFileLock, error) {
	return nil, &ActivationLockError{Code: "activation_lock_unsupported"}
}
func (*activationFileLock) check() error {
	return &ActivationLockError{Code: "activation_lock_unsupported"}
}
func (*activationFileLock) release() {}
