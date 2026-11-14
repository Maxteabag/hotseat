//go:build !unix && !windows

package quota

import (
	"errors"
	"os"
)

func tryReadLock(*os.File) (bool, error) {
	return false, errors.New("shared usage cache locking unavailable on this platform")
}
func releaseReadLock(*os.File) {}
