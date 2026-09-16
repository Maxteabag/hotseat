//go:build windows

package work

import (
	"errors"
	"os"
)

// ownedByCaller has no /proc to consult on Windows; nothing is ever identified.
func ownedByCaller(os.FileInfo) bool { return false }

func terminate(int) error { return errors.New("process control is not supported on this platform") }
