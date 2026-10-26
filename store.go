package protolock

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
)

// mutexSuffix names the hidden sidecar file (proto.lock.~lock) used to
// serialize init/commit operations across cooperating protolock processes.
// It lives next to proto.lock so both always share the same filesystem.
const mutexSuffix = ".~lock"

// tempLockPrefix is used for temporary files created in the lock directory.
const tempLockPrefix = ".proto.lock.tmp-"

var (
	// ErrNoLockFile is returned when a commit is attempted without an existing
	// proto.lock file.
	ErrNoLockFile = errors.New(`no "proto.lock" file found, first run "init"`)

	// ErrLockExists is returned by InitToDisk if another process created
	// proto.lock first; init must never clobber an existing baseline.
	ErrLockExists = errors.New(
		`a "proto.lock" file was already found, use "commit" to update`,
	)

	// ErrBaselineChanged indicates proto.lock was modified on disk after the
	// committing process read it (e.g. a concurrent commit published first).
	// The publish is aborted so the newer baseline is never overwritten.
	ErrBaselineChanged = errors.New(
		"protolock: proto.lock changed on disk since it was read; commit aborted, re-run to validate against the new baseline",
	)
)

// mutexFilePath returns the path of the cross-process mutex sidecar.
func (cfg *Config) mutexFilePath() string {
	return cfg.LockFilePath() + ".~lock"
}

// withLockMutex runs fn while holding an exclusive, advisory, cross-process
// lock for the configured proto.lock. Cooperating protolock processes are
// serialized, so an init/commit can never interleave scan, validation, and
// publish with another one.
func withLockMutex(cfg Config, fn func() error) error {
	path := cfg.mutexFilePath()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := acquireFileLock(f); err != nil {
		return err
	}
	defer releaseFileLock(f)

	return fn()
}

// readLock reads the raw proto.lock bytes and their parsed form. The raw
// bytes are retained as a baseline digest to detect concurrent writers.
func readLock(cfg Config) (raw []byte, lock Protolock, exists bool, err error) {
	raw, err = os.ReadFile(cfg.LockFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Protolock{}, false, nil
		}
		return nil, Protolock{}, false, err
	}

	lock, err = FromReader(bytes.NewReader(raw))
	if err != nil {
		return nil, Protolock{}, true, err
	}

	return raw, lock, true, nil
}

// lockUnchanged reports whether the current on-disk proto.lock is still
// byte-for-byte identical to the baseline read at the start of an operation.
func lockUnchanged(cfg Config, baseline []byte) (bool, error) {
	raw, err := os.ReadFile(cfg.LockFilePath())
	if err != nil {
		return false, err
	}

	return sha256.Sum256(raw) == sha256.Sum256(baseline), nil
}

// publishLock writes data to proto.lock atomically.
//
// Data is fully written and fsynced to a temporary file in the same directory,
// then put into place in a single metadata operation:
//   - exclusive (init): a hard link is used, which fails if proto.lock already
//     exists, so a lock just created by another process is never overwritten.
//   - non-exclusive (commit): rename(2) atomically replaces the old lock.
//
// A failure at any point before the swap leaves the previous proto.lock
// completely intact; a crash can only leave the complete old or the complete
// new file, never a truncated/partial one.
func publishLock(cfg Config, data []byte, exclusive bool) (err error) {
	target := cfg.LockFilePath()

	// Preserve the existing lock's permission bits on update; default to the
	// mode os.Create would have used for a new file.
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(target); statErr == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(cfg.LockDir, tempLockPrefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// The temp file is only renamed/linked away on success; remove it on any
	// other exit path so failed attempts leave nothing behind.
	swapped := false
	defer func() {
		if !swapped {
			printIfErr(os.Remove(tmpName))
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		printIfErr(tmp.Close())
		return err
	}

	if err = tmp.Chmod(mode); err != nil {
		printIfErr(tmp.Close())
		return err
	}

	// Flush file contents before the swap so an OS crash after the rename
	// cannot expose a zero-length/newly-created inode.
	if err = tmp.Sync(); err != nil {
		printIfErr(tmp.Close())
		return err
	}

	if err = tmp.Close(); err != nil {
		return err
	}

	if exclusive {
		// os.Link fails if target already exists: an atomic, exclusive create.
		if err = os.Link(tmpName, target); err != nil {
			if os.IsExist(err) {
				return ErrLockExists
			}
			return err
		}
		// A hard link keeps the temp inode reachable via the temp path.
		printIfErr(os.Remove(tmpName))
	} else {
		if err = os.Rename(tmpName, target); err != nil {
			return err
		}
	}

	swapped = true

	// Best-effort: make the directory entry change durable as well.
	syncDir(cfg.LockDir)

	return nil
}

// syncDir opens a directory and flushes its metadata to disk. Errors are
// ignored on platforms that do not support fsync on directory descriptors.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
