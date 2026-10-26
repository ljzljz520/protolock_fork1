//go:build windows || plan9 || js || wasip1

package protolock

import "os"

// acquireFileLock is a no-op on platforms without flock(2). Storage safety is
// still enforced there by the exclusive hard-link create (init), the
// pre-publish baseline re-read (commit), and atomic temp-file replacement.
func acquireFileLock(f *os.File) error { return nil }

func releaseFileLock(f *os.File) error { return nil }
