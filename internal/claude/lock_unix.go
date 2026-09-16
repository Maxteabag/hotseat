//go:build unix

package claude

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock, blocking until it is free.
func flockExclusive(handle *os.File) error {
	return syscall.Flock(int(handle.Fd()), syscall.LOCK_EX)
}

func funlock(handle *os.File) error {
	return syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
}
