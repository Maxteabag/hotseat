//go:build windows

package tui

import (
	"os/exec"
	"time"
)

func isolateCommand(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
