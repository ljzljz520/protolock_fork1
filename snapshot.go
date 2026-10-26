package protolock

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrSourceChanged indicates that the set of .proto files or their contents
// changed on disk after a snapshot was captured and validated, so the snapshot
// can no longer be safely published. The existing proto.lock is left intact.
var ErrSourceChanged = errors.New(
	"protolock: proto sources changed between validation and write; commit aborted, proto.lock unchanged",
)

// sourceFile is a single .proto file captured in a Snapshot.
type sourceFile struct {
	// protopath is the lock-relative path used as the Definition.Filepath key.
	protopath string

	// sum is the SHA-256 digest of the exact bytes that were parsed.
	sum [sha256.Size]byte
}

// Snapshot is a point-in-time capture of the parsed proto source tree.
//
// Every validation step (built-in rules and plugins) and the serialized
// output of a publish MUST use the same Snapshot. Reusing one capture removes
// the window in which on-disk edits could be published without ever being
// checked.
type Snapshot struct {
	root   string
	ignore string

	// files is the source manifest: one entry per .proto file found.
	files []sourceFile

	// lock is the parsed representation of the captured sources.
	lock Protolock
}

// captureSnapshot walks the proto root once, reading each .proto file's bytes
// a single time. The same bytes are hashed (for the source manifest), parsed
// (for comparison rules and plugins), and later serialized on publish.
func captureSnapshot(cfg Config) (*Snapshot, error) {
	root, err := filepath.Abs(cfg.ProtoRoot)
	if err != nil {
		return nil, err
	}

	protoFiles, err := getProtoFiles(root, cfg.Ignore)
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{root: root, ignore: cfg.Ignore}

	cwd, cwdErr := os.Getwd()

	for _, path := range protoFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// Have the parser report a friendly file path, mirroring the previous
		// behavior of passing the open file handle to Parse.
		friendlyPath := path
		if cwdErr == nil {
			if relpath, relErr := filepath.Rel(cwd, path); relErr == nil {
				friendlyPath = relpath
			}
		}

		entry, err := Parse(friendlyPath, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}

		localPath := strings.TrimPrefix(path, root)
		localPath = strings.TrimPrefix(localPath, string(filepath.Separator))

		snap.files = append(snap.files, sourceFile{
			protopath: localPath,
			sum:       sha256.Sum256(data),
		})
		snap.lock.Definitions = append(snap.lock.Definitions, Definition{
			Filepath: ProtoPath(Protopath(localPath)),
			Def:      entry,
		})
	}

	return snap, nil
}

// getUpdatedLock parses all .proto files in the configured tree into an
// updated Protolock, using a single captured snapshot.
func getUpdatedLock(cfg Config) (*Protolock, error) {
	snap, err := captureSnapshot(cfg)
	if err != nil {
		return nil, err
	}

	updated := snap.lock
	return &updated, nil
}

// readManifest walks the proto root and hashes each .proto file without
// parsing it. It is used to confirm that the sources captured in a Snapshot
// are byte-for-byte identical to the sources on disk immediately before a
// publish.
func readManifest(root, ignore string) ([]sourceFile, error) {
	protoFiles, err := getProtoFiles(root, ignore)
	if err != nil {
		return nil, err
	}

	files := make([]sourceFile, 0, len(protoFiles))
	for _, path := range protoFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		localPath := strings.TrimPrefix(path, root)
		localPath = strings.TrimPrefix(localPath, string(filepath.Separator))

		files = append(files, sourceFile{
			protopath: localPath,
			sum:       sha256.Sum256(data),
		})
	}

	return files, nil
}

// verify re-reads the source manifest and compares it against the one captured
// with the snapshot. Any added, removed, or modified .proto file causes
// ErrSourceChanged, guaranteeing that only validated bytes get published.
func (s *Snapshot) verify() error {
	current, err := readManifest(s.root, s.ignore)
	if err != nil {
		return err
	}

	if !sameManifest(s.files, current) {
		return ErrSourceChanged
	}

	return nil
}

// sameManifest reports whether two source manifests contain exactly the same
// protopath -> content-digest pairs, regardless of ordering.
func sameManifest(a, b []sourceFile) bool {
	if len(a) != len(b) {
		return false
	}

	have := make(map[string][sha256.Size]byte, len(a))
	for _, f := range a {
		have[f.protopath] = f.sum
	}

	for _, f := range b {
		sum, ok := have[f.protopath]
		if !ok || sum != f.sum {
			return false
		}
	}

	return true
}
