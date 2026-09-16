//go:build !windows

package work

import (
	"os"
	"syscall"
)

// ownedByCaller says whether a /proc entry belongs to the calling user.
func ownedByCaller(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

// terminate sends SIGTERM, which lets the CLI release its locks and flush its
// history on the way out.
func terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
