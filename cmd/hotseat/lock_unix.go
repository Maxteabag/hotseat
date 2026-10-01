//go:build unix

package main

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive advisory lock without waiting. False means another
// process holds it.
func tryLock(handle *os.File) bool {
	return syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
