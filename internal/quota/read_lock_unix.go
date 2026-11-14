//go:build unix

package quota

import (
	"os"
	"syscall"
)

func tryReadLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return err == nil, err
}
func releaseReadLock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
