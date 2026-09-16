//go:build !unix

package claude

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

func detachedSpawn(argv []string, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func execReplace(string, []string, []string) error {
	return errors.New("replacing the process is not supported on " + runtime.GOOS)
}

// Without uid ownership to check, the sweep trusts the name and age alone.
func ownedByCurrentUser(os.FileInfo) bool { return true }
