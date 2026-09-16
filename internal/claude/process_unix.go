//go:build unix

package claude

import (
	"os"
	"os/exec"
	"syscall"
)

// detachedSpawn starts argv in its own session with stdout and stderr on
// /dev/null, so a terminal window outlives whatever launched it. A nil env
// inherits this process's environment.
func detachedSpawn(argv []string, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child when it exits rather than leaving a zombie behind.
	go func() { _ = cmd.Wait() }()
	return nil
}

// execReplace replaces this process with `path argv...` under env.
func execReplace(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}

// ownedByCurrentUser reports whether a file belongs to the running uid.
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == os.Getuid()
}
