//go:build linux

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The only new mutex file is profiles/.activation.lock. Canonical .lock files
// already serialize refresh. Never remove a lock file or infer ownership from it.
type activationFileLock struct {
	handle    *os.File
	directory string
	name      string
	chain     []os.FileInfo
}

func switchDirectory(path string) (*os.File, []os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, &ActivationLockError{Code: "invalid_lock"}
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	chain := []os.FileInfo{}
	for _, part := range strings.Split(filepath.Clean(path), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(int(current.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, nil, err
		}
		current = os.NewFile(uintptr(next), part)
		info, err := current.Stat()
		if err != nil {
			current.Close()
			return nil, nil, err
		}
		chain = append(chain, info)
	}
	return current, chain, nil
}

func lockSwitchFile(directory, name string, deadline time.Time) (*activationFileLock, error) {
	dir, chain, err := switchDirectory(directory)
	if err != nil {
		return nil, &ActivationLockError{Code: "invalid_lock"}
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, &ActivationLockError{Code: "invalid_lock"}
	}
	handle := os.NewFile(uintptr(fd), name)
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, &ActivationLockError{Code: "invalid_lock"}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 ||
		(name == ".activation.lock" && info.Mode().Perm()&0077 != 0) {
		handle.Close()
		return nil, &ActivationLockError{Code: "invalid_lock"}
	}
	for {
		if !time.Now().Before(deadline) {
			handle.Close()
			return nil, &ActivationLockError{Code: "activation_busy"}
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			handle.Close()
			return nil, &ActivationLockError{Code: "invalid_lock"}
		}
		remaining := time.Until(deadline)
		if remaining > 25*time.Millisecond {
			remaining = 25 * time.Millisecond
		}
		if remaining > 0 {
			time.Sleep(remaining)
		}
	}
	lock := &activationFileLock{handle: handle, directory: directory, name: name, chain: chain}
	if err := lock.check(); err != nil {
		lock.release()
		return nil, err
	}
	return lock, nil
}

func (l *activationFileLock) check() error {
	dir, chain, err := switchDirectory(l.directory)
	if err != nil {
		return &ActivationLockError{Code: "lock_changed"}
	}
	defer dir.Close()
	if len(chain) != len(l.chain) {
		return &ActivationLockError{Code: "lock_changed"}
	}
	for i := range chain {
		if !os.SameFile(chain[i], l.chain[i]) {
			return &ActivationLockError{Code: "lock_changed"}
		}
	}
	linked, err := os.Lstat(filepath.Join(l.directory, l.name))
	owned, ownErr := l.handle.Stat()
	if err != nil || ownErr != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, owned) {
		return &ActivationLockError{Code: "lock_changed"}
	}
	return nil
}

func (l *activationFileLock) release() {
	_ = syscall.Flock(int(l.handle.Fd()), syscall.LOCK_UN)
	_ = l.handle.Close()
}
