//go:build !windows && !plan9 && !js && !wasip1

package protolock

import (
	"os"
	"syscall"
)

// acquireFileLock takes an exclusive advisory lock shared across processes
// (and goroutines using separate file descriptions) via flock(2).
func acquireFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// releaseFileLock releases the advisory lock. Closing the descriptor would
// also release it; unlocking explicitly keeps intent visible.
func releaseFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
