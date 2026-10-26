package protolock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testProto = `syntax = "proto3";

package test;

message Foo {
  string name = 1;
}
`

// setupWorkspace creates a temporary lock/proto directory containing one
// proto file and returns its config and path.
func setupWorkspace(t *testing.T) (Config, string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.proto"), []byte(testProto), 0o644))

	cfg, err := NewConfig(dir, dir, "", false, false)
	require.NoError(t, err)

	return *cfg, dir
}

func mustInit(t *testing.T, cfg Config) {
	t.Helper()
	require.NoError(t, InitToDisk(cfg))
}

func readLockBytes(t *testing.T, cfg Config) []byte {
	t.Helper()
	b, err := os.ReadFile(cfg.LockFilePath())
	require.NoError(t, err)
	return b
}

// assertNoTempFiles ensures no partial-write temp files remain in the lock dir.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), tempLockPrefix),
			"temporary lock file left behind: %s", e.Name())
	}
}

func TestInitToDiskCreatesLock(t *testing.T) {
	cfg, dir := setupWorkspace(t)

	require.NoError(t, InitToDisk(cfg))

	data := readLockBytes(t, cfg)
	var parsed Protolock
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Len(t, parsed.Definitions, 1)
	assertNoTempFiles(t, dir)
}

func TestInitToDiskIsExclusive(t *testing.T) {
	cfg, _ := setupWorkspace(t)
	mustInit(t, cfg)

	// a second init must never overwrite the existing lock
	err := InitToDisk(cfg)
	assert.ErrorIs(t, err, ErrLockExists)
}

func TestCommitRequiresInit(t *testing.T) {
	cfg, _ := setupWorkspace(t)

	_, err := CommitToDisk(cfg, CommitOptions{})
	assert.ErrorIs(t, err, ErrNoLockFile)
}

func TestCommitWritesValidatedSnapshot(t *testing.T) {
	cfg, dir := setupWorkspace(t)
	mustInit(t, cfg)
	before := readLockBytes(t, cfg)

	var validated Protolock
	report, err := CommitToDisk(cfg, CommitOptions{
		Plugins: func(r *Report) error {
			validated = r.Updated
			return nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Empty(t, report.Warnings)

	after := readLockBytes(t, cfg)

	// the bytes on disk must be exactly the serialization of the snapshot
	// that the built-in rules and plugins validated
	expected, err := json.MarshalIndent(&validated, "", "  ")
	require.NoError(t, err)
	assert.Equal(t, expected, after)
	assert.Equal(t, before, after)
	assertNoTempFiles(t, dir)
}

func TestCommitPluginWarningsBlockPublish(t *testing.T) {
	cfg, _ := setupWorkspace(t)
	mustInit(t, cfg)
	before := readLockBytes(t, cfg)

	report, err := CommitToDisk(cfg, CommitOptions{
		Plugins: func(r *Report) error {
			r.Warnings = append(r.Warnings, Warning{Message: "plugin says no"})
			return nil
		},
	})
	require.ErrorIs(t, err, ErrWarningsFound)
	require.NotNil(t, report)
	assert.Len(t, report.Warnings, 1)

	// the old lock must be untouched
	assert.Equal(t, before, readLockBytes(t, cfg))
}

func TestCommitPluginErrorBlocksPublish(t *testing.T) {
	cfg, _ := setupWorkspace(t)
	mustInit(t, cfg)
	before := readLockBytes(t, cfg)

	pluginErr := assert.AnError
	_, err := CommitToDisk(cfg, CommitOptions{
		Plugins: func(r *Report) error { return pluginErr },
	})
	assert.ErrorIs(t, err, pluginErr)
	assert.Equal(t, before, readLockBytes(t, cfg))
}

func TestCommitSourceDriftAborts(t *testing.T) {
	cfg, dir := setupWorkspace(t)
	mustInit(t, cfg)
	before := readLockBytes(t, cfg)

	// simulate an external edit landing during validation, after the
	// snapshot was captured
	_, err := CommitToDisk(cfg, CommitOptions{
		Plugins: func(r *Report) error {
			return os.WriteFile(filepath.Join(dir, "test.proto"),
				[]byte(testProto+"\n// edited mid-validation\n"), 0o644)
		},
	})
	assert.ErrorIs(t, err, ErrSourceChanged)
	assert.Equal(t, before, readLockBytes(t, cfg), "old lock must survive source drift")
	assertNoTempFiles(t, dir)
}

func TestCommitBaselineDriftAborts(t *testing.T) {
	cfg, dir := setupWorkspace(t)
	mustInit(t, cfg)

	// simulate a concurrent process replacing the baseline lock during
	// validation; the newer baseline must never be overwritten
	external := []byte(`{"external":"baseline"}`)
	_, err := CommitToDisk(cfg, CommitOptions{
		Plugins: func(r *Report) error {
			return os.WriteFile(cfg.LockFilePath(), external, 0o644)
		},
	})
	assert.ErrorIs(t, err, ErrBaselineChanged)
	assert.Equal(t, external, readLockBytes(t, cfg),
		"newer baseline written by another process must be preserved")
	assertNoTempFiles(t, dir)
}

func TestForceSkipsCompatibilityButKeepsStorageGuarantees(t *testing.T) {
	t.Run("force skips plugins", func(t *testing.T) {
		cfg, dir := setupWorkspace(t)
		mustInit(t, cfg)

		called := false
		_, err := CommitToDisk(cfg, CommitOptions{
			Force: true,
			Plugins: func(r *Report) error {
				called = true
				r.Warnings = append(r.Warnings, Warning{Message: "should be skipped"})
				return nil
			},
		})
		require.NoError(t, err)
		assert.False(t, called, "--force must skip plugin compatibility checks")
		assertNoTempFiles(t, dir)
	})

	t.Run("force still rejects source drift", func(t *testing.T) {
		cfg, dir := setupWorkspace(t)
		mustInit(t, cfg)
		before := readLockBytes(t, cfg)

		prePublishHook = func() {
			require.NoError(t, os.WriteFile(
				filepath.Join(dir, "test.proto"),
				[]byte(testProto+"\n// edited after validation\n"),
				0o644,
			))
		}
		defer func() { prePublishHook = nil }()

		_, err := CommitToDisk(cfg, CommitOptions{Force: true})
		assert.ErrorIs(t, err, ErrSourceChanged)
		assert.Equal(t, before, readLockBytes(t, cfg), "old lock must survive source drift")
	})

	t.Run("force still rejects baseline drift", func(t *testing.T) {
		cfg, _ := setupWorkspace(t)
		mustInit(t, cfg)

		// an external writer replaces the baseline after this process read it
		external := []byte(`{"external":"baseline"}`)
		prePublishHook = func() {
			require.NoError(t, os.WriteFile(cfg.LockFilePath(), external, 0o644))
		}
		defer func() { prePublishHook = nil }()

		_, err := CommitToDisk(cfg, CommitOptions{Force: true})
		assert.ErrorIs(t, err, ErrBaselineChanged)
		assert.Equal(t, external, readLockBytes(t, cfg),
			"newer baseline written by another process must be preserved")
	})
}

func TestConcurrentInitHasSingleWinner(t *testing.T) {
	cfg, _ := setupWorkspace(t)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			errs <- InitToDisk(cfg)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	winners, losers := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrLockExists):
			losers++
		default:
			t.Fatalf("unexpected init error: %v", err)
		}
	}
	assert.Equal(t, 1, winners, "exactly one init must create the lock")
	assert.Equal(t, n-1, losers, "all other inits must report an existing lock")

	data := readLockBytes(t, cfg)
	var parsed Protolock
	require.NoError(t, json.Unmarshal(data, &parsed), "published lock must be complete JSON")
}

func TestConcurrentCommitsNeverCorruptLock(t *testing.T) {
	cfg, dir := setupWorkspace(t)
	mustInit(t, cfg)

	const n = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := CommitToDisk(cfg, CommitOptions{})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}

	data := readLockBytes(t, cfg)
	var parsed Protolock
	require.NoError(t, json.Unmarshal(data, &parsed),
		"concurrent commits must never leave a partial lock")
	assert.Len(t, parsed.Definitions, 1)
	assertNoTempFiles(t, dir)
}
