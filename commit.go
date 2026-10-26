package protolock

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// PluginValidator runs external protolock plugins against a report produced
// from a captured snapshot. Implementations append plugin warnings to the
// report and return a non-nil error if a plugin fails outright.
type PluginValidator func(report *Report) error

// prePublishHook is a test-only seam invoked after compatibility checks and
// before the source/baseline verification. It is always nil outside tests.
var prePublishHook func()

// CommitOptions configures CommitToDisk.
type CommitOptions struct {
	// Force skips the API compatibility checks (built-in rules and plugins).
	// Storage consistency guarantees (single validated snapshot, source/baseline
	// verification, and atomic write) remain enforced regardless of Force.
	Force bool

	// Plugins optionally validates the snapshot via external plugins. It is
	// only invoked when Force is false.
	Plugins PluginValidator
}

// Commit will return an io.Reader with the lock representation data for caller to
// use as needed.
func Commit(cfg Config) (io.Reader, error) {
	if !cfg.LockFileExists() {
		fmt.Println(`no "proto.lock" file found, first run "init"`)
		os.Exit(1)
	}

	updated, err := getUpdatedLock(cfg)
	if err != nil {
		return nil, err
	}

	return readerFromProtolock(updated)
}

// CommitToDisk validates ONE captured snapshot of the proto sources and only
// then atomically publishes it as proto.lock.
//
// The built-in rules and the plugin validator all run against the same
// snapshot that gets serialized, so on-disk edits made during validation can
// never slip into the lock. Immediately before the atomic swap, both the
// source manifest and the on-disk baseline lock are re-checked for changes
// (including concurrent commits). On any conflict or failure the previous
// proto.lock is left byte-for-byte intact.
//
// With opts.Force, compatibility checks (rules and plugins) are skipped, but
// the snapshot reuse, manifest/baseline verification, cross-process locking,
// and atomic write are all still applied.
func CommitToDisk(cfg Config, opts CommitOptions) (report *Report, err error) {
	err = withLockMutex(cfg, func() error {
		// Capture and validate a single snapshot; this same struct is what
		// gets serialized below.
		snap, err := captureSnapshot(cfg)
		if err != nil {
			return err
		}

		baseline, current, exists, err := readLock(cfg)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNoLockFile
		}

		if opts.Force {
			report = &Report{Current: current, Updated: snap.lock}
		} else {
			report, _ = Compare(current, snap.lock)

			if opts.Plugins != nil {
				if pluginErr := opts.Plugins(report); pluginErr != nil {
					return pluginErr
				}
			}

			// Compatibility conflict: do not publish.
			if len(report.Warnings) > 0 {
				return ErrWarningsFound
			}
		}

		// Test-only seam: run after compatibility checks and before the
		// storage consistency verification, so tests can simulate an external
		// writer landing mid-operation (nil in production builds).
		if prePublishHook != nil {
			prePublishHook()
		}

		// Storage consistency: the validated sources and the baseline lock
		// must be unchanged on disk before the atomic swap.
		if err := snap.verify(); err != nil {
			return err
		}

		unchanged, err := lockUnchanged(cfg, baseline)
		if err != nil {
			return err
		}
		if !unchanged {
			return ErrBaselineChanged
		}

		data, err := json.MarshalIndent(&snap.lock, "", "  ")
		if err != nil {
			return err
		}

		return publishLock(cfg, data, false)
	})

	return report, err
}
