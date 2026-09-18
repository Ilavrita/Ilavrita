package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Take writes an archive of one install into a directory.
//
// The order is the whole of what makes the result consistent. The database is
// snapshotted first and the payloads copied afterwards, so every row in the
// snapshot names bytes that already existed when it was taken and are therefore
// copied. Copying the payloads first would put a row in the archive whose bytes
// were written after the copy began — a backup naming a document it does not
// hold.
//
// The cost of that order is an archive that may hold a payload no row names: a
// write that landed between the snapshot and the copy. That is wasted space in
// the archive and nothing else, which is the direction to be wrong in.
func Take(into string, source Snapshotter, payloads string, now time.Time) (Manifest, error) {
	if err := os.MkdirAll(into, directoryMode); err != nil {
		return Manifest{}, fmt.Errorf("backup: make room for an archive: %w", err)
	}

	manifest := Manifest{Version: Version, TakenAt: now.UTC(), Files: map[string]Recorded{}}

	snapshot := filepath.Join(into, DatabaseFile)

	if err := source.SnapshotInto(snapshot); err != nil {
		return Manifest{}, err
	}

	// The snapshot is the database's own file, created with whatever mode the
	// database decided. This archive holds patient data, password hashes,
	// sealed second factors and the audit trail, and it is a thing an operator
	// moves between machines, so it is restricted here rather than left to a
	// umask somewhere.
	if err := os.Chmod(snapshot, archiveMode); err != nil {
		return Manifest{}, fmt.Errorf("backup: restrict the snapshot: %w", err)
	}

	held, err := recordOf(snapshot)
	if err != nil {
		return Manifest{}, err
	}

	manifest.Files[DatabaseFile] = held

	copied, err := copyTree(payloads, filepath.Join(into, PayloadDirectory))
	if err != nil {
		return Manifest{}, err
	}

	for path, record := range copied {
		manifest.Files[filepath.ToSlash(filepath.Join(PayloadDirectory, path))] = record
	}

	// Written last, because it is what says the archive is complete. One that
	// died before this point is a directory with no manifest, which a restore
	// refuses rather than half-trusts.
	if err := writeManifest(filepath.Join(into, ManifestFile), manifest); err != nil {
		return Manifest{}, err
	}

	return manifest, nil
}

// copyTree copies one directory into another, recording what it wrote. A source
// that is not there copies nothing, which is what a deployment holding no
// payloads has.
func copyTree(from, to string) (map[string]Recorded, error) {
	copied := map[string]Recorded{}

	if _, err := os.Stat(from); os.IsNotExist(err) {
		return copied, nil
	}

	walked := func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}

		landing := filepath.Join(to, relative)

		if entry.IsDir() {
			return os.MkdirAll(landing, directoryMode)
		}

		if !entry.Type().IsRegular() {
			// A socket or a symlink under a payload directory is not a payload.
			// Copying one would put something in the archive that a restore
			// would then put back on a different host.
			return nil
		}

		if err := copyFile(path, landing); err != nil {
			return err
		}

		record, err := recordOf(landing)
		if err != nil {
			return err
		}

		copied[filepath.ToSlash(relative)] = record

		return nil
	}

	if err := filepath.WalkDir(from, walked); err != nil {
		return nil, fmt.Errorf("backup: copy the payloads: %w", err)
	}

	return copied, nil
}

// copyFile writes one file into the archive, flushed before it returns: an
// archive is worth what it is worth after the power goes out.
func copyFile(from, to string) error {
	source, err := os.Open(from) //nolint:gosec // both paths are the caller's own directories.
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", filepath.Base(from), err)
	}

	defer func() { _ = source.Close() }()

	if err := os.MkdirAll(filepath.Dir(to), directoryMode); err != nil {
		return fmt.Errorf("backup: make room for %s: %w", filepath.Base(to), err)
	}

	landing, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, archiveMode) //nolint:gosec // as above.
	if err != nil {
		return fmt.Errorf("backup: write %s: %w", filepath.Base(to), err)
	}

	if _, err := io.Copy(landing, source); err != nil {
		_ = landing.Close()

		return fmt.Errorf("backup: copy %s: %w", filepath.Base(to), err)
	}

	if err := landing.Sync(); err != nil {
		_ = landing.Close()

		return fmt.Errorf("backup: flush %s: %w", filepath.Base(to), err)
	}

	return landing.Close()
}

// recordOf digests one file, which is what tells a restore it is the file the
// manifest names.
func recordOf(path string) (Recorded, error) {
	held, err := os.Open(path) //nolint:gosec // the caller's own archive.
	if err != nil {
		return Recorded{}, fmt.Errorf("backup: read %s: %w", filepath.Base(path), err)
	}

	defer func() { _ = held.Close() }()

	digest := sha256.New()

	size, err := io.Copy(digest, held)
	if err != nil {
		return Recorded{}, fmt.Errorf("backup: digest %s: %w", filepath.Base(path), err)
	}

	return Recorded{Digest: hex.EncodeToString(digest.Sum(nil)), Size: size}, nil
}

// writeManifest records what the archive holds.
func writeManifest(path string, manifest Manifest) error {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: describe the archive: %w", err)
	}

	if err := os.WriteFile(path, append(body, '\n'), archiveMode); err != nil {
		return fmt.Errorf("backup: write the manifest: %w", err)
	}

	return nil
}
