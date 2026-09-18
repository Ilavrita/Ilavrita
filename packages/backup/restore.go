package backup

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Read returns what an archive says about itself.
func Read(archive string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(archive, ManifestFile)) //nolint:gosec // the caller's own archive.
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrNotAnArchive, err)
	}

	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrNotAnArchive, err)
	}

	if manifest.Version != Version {
		return Manifest{}, fmt.Errorf("%w: it is version %d and this build reads %d",
			ErrNotAnArchive, manifest.Version, Version)
	}

	if _, held := manifest.Files[DatabaseFile]; !held {
		return Manifest{}, fmt.Errorf("%w: it names no database", ErrNotAnArchive)
	}

	return manifest, nil
}

// Verify checks an archive against its own manifest, file by file.
//
// It is separate from Restore so an operator can ask whether a backup is worth
// anything without restoring it. A backup nobody checked is one whose first
// test is the day it is needed.
func Verify(archive string) (Manifest, error) {
	manifest, err := Read(archive)
	if err != nil {
		return Manifest{}, err
	}

	for named, recorded := range manifest.Files {
		path, err := within(archive, named)
		if err != nil {
			return Manifest{}, err
		}

		held, err := recordOf(path)
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %s: %w", ErrArchiveIncomplete, named, err)
		}

		if held.Size != recorded.Size ||
			subtle.ConstantTimeCompare([]byte(held.Digest), []byte(recorded.Digest)) != 1 {
			return Manifest{}, fmt.Errorf("%w: %s is not the file it was written as",
				ErrArchiveIncomplete, named)
		}
	}

	return manifest, nil
}

// Restore puts an archive back into a data directory.
//
// The archive is verified first and in full: a restore that discovered a
// corrupt file halfway through would leave a directory that is neither the
// backup nor what was there before.
//
// It refuses a directory that already holds a database. Restoring over a live
// install is a thing to do deliberately, with the old one moved aside by
// somebody who knows they mean it.
func Restore(archive, into string) (Manifest, error) {
	manifest, err := Verify(archive)
	if err != nil {
		return Manifest{}, err
	}

	switch _, err := os.Stat(filepath.Join(into, DatabaseFile)); {
	case err == nil:
		return Manifest{}, fmt.Errorf("%w: %s", ErrOccupied, into)
	case !errors.Is(err, os.ErrNotExist):
		return Manifest{}, fmt.Errorf("backup: look at the data directory: %w", err)
	}

	if err := os.MkdirAll(into, directoryMode); err != nil {
		return Manifest{}, fmt.Errorf("backup: make room for the install: %w", err)
	}

	// The payloads first, and the database last. A restore interrupted halfway
	// leaves bytes no row names rather than rows naming bytes that are not
	// there — the same direction the live server is careful to be wrong in.
	for _, named := range ordered(manifest) {
		from, err := within(archive, named)
		if err != nil {
			return Manifest{}, err
		}

		to, err := within(into, named)
		if err != nil {
			return Manifest{}, err
		}

		if err := copyFile(from, to); err != nil {
			return Manifest{}, err
		}
	}

	return manifest, nil
}

// ordered lists the archive's files with the database last, which is what makes
// an interrupted restore leave the safe kind of inconsistency.
func ordered(manifest Manifest) []string {
	named := make([]string, 0, len(manifest.Files))

	for path := range manifest.Files {
		if path != DatabaseFile {
			named = append(named, path)
		}
	}

	return append(named, DatabaseFile)
}

// within resolves one manifest entry against a root, refusing anything that
// would land outside it. A manifest is a file, and a file is something somebody
// can edit: one naming "../../etc/passwd" must not be followed.
func within(root, named string) (string, error) {
	if named == "" || strings.Contains(named, "\x00") {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, named)
	}

	path := filepath.Join(root, filepath.FromSlash(named))

	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, named)
	}

	return path, nil
}
