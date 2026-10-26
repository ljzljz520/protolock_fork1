package protolock

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const protoSuffix = ".proto"

// Init will return an io.Reader with the lock representation data for caller to
// use as needed.
func Init(cfg Config) (io.Reader, error) {
	if cfg.LockFileExists() {
		fmt.Println(`a "proto.lock" file was already found, use "commit" to update`)
		os.Exit(1)
	}

	updated, err := getUpdatedLock(cfg)
	if err != nil {
		return nil, err
	}

	return readerFromProtolock(updated)
}

// InitToDisk captures a snapshot of the proto sources and atomically creates
// proto.lock.
//
// The operation is serialized with concurrent init/commit calls, the source
// snapshot is re-verified immediately before publishing, and the lock is
// created via an exclusive, atomic operation: if another process created
// proto.lock first, InitToDisk fails with ErrLockExists instead of
// overwriting it. A crash or write failure leaves either no lock or a
// complete new lock, never a partial file.
func InitToDisk(cfg Config) error {
	return withLockMutex(cfg, func() error {
		if cfg.LockFileExists() {
			return ErrLockExists
		}

		snap, err := captureSnapshot(cfg)
		if err != nil {
			return err
		}

		// Sources must not have changed while they were being captured.
		if err := snap.verify(); err != nil {
			return err
		}

		// Re-check under the cross-process lock; publishLock's exclusive
		// create is the final atomic guarantee.
		if cfg.LockFileExists() {
			return ErrLockExists
		}

		data, err := json.MarshalIndent(&snap.lock, "", "  ")
		if err != nil {
			return err
		}

		return publishLock(cfg, data, true)
	})
}

func readerFromProtolock(lock *Protolock) (io.Reader, error) {
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, err
	}

	return strings.NewReader(string(b)), nil
}
