//go:build !unix

package claude

import "os"

// Advisory file locks are a Unix facility; elsewhere refreshes are not
// serialised across processes.
func flockExclusive(*os.File) error { return nil }

func funlock(*os.File) error { return nil }
