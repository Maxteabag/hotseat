package quota

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Separate lock files survive atomic cache replacement. OS locks are released
// on process exit; a cancelled caller never falls through into a fresh fetch.
func lockRead(ctx context.Context, root, name string) (func(), error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		ok, err := tryReadLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { releaseReadLock(f); f.Close() }, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
