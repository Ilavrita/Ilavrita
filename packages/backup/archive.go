// Package backup copies an install's state out and puts it back.
//
// An install is two stores: a database holding every row, and a directory
// holding the payloads those rows describe. A copy of one without the other is
// not a backup of anything, which is why they are taken together and in an
// order that makes the result consistent.
package backup

import (
	"errors"
	"io/fs"
	"time"
)

// What an archive is made of.
const (
	// DatabaseFile is the snapshot of every row.
	DatabaseFile = "ilavrita.db"

	// PayloadDirectory holds the bytes those rows describe.
	PayloadDirectory = "payloads"

	// ManifestFile says what the archive holds, so a restore can tell a
	// complete one from a copy that was interrupted.
	ManifestFile = "manifest.json"
)

// The permissions an archive is written with. It holds everything the database
// holds — patient data, password hashes, sealed second factors and the audit
// trail — and it is a thing an operator moves between machines, so nothing but
// the account that wrote it can read it.
const (
	archiveMode   fs.FileMode = 0o600
	directoryMode fs.FileMode = 0o700
)

// Version is the archive layout this build writes and reads. A restore refuses
// what it does not understand rather than doing its best with it.
const Version = 1

var (
	// ErrNotAnArchive reports a directory holding no manifest, or one this
	// build cannot read.
	ErrNotAnArchive = errors.New("backup: that directory is not an archive this build wrote")

	// ErrArchiveIncomplete reports an archive whose contents do not match what
	// its manifest says. A backup half-written is the one nobody finds out
	// about until they need it.
	ErrArchiveIncomplete = errors.New("backup: the archive does not match its own manifest")

	// ErrOccupied reports a restore into a data directory that already holds an
	// install. Restoring over one is not something to do by accident.
	ErrOccupied = errors.New("backup: that data directory already holds an install")

	// ErrUnsafePath reports a manifest naming a file outside the archive.
	ErrUnsafePath = errors.New("backup: the manifest names a path outside the archive")
)

// Manifest is what an archive says about itself.
type Manifest struct {
	Version int       `json:"version"`
	TakenAt time.Time `json:"takenAt"`

	// Files is every file in the archive besides the manifest, by its path
	// relative to the archive root, with the digest and size it was written
	// with. It is what makes an interrupted copy detectable.
	Files map[string]Recorded `json:"files"`
}

// Recorded is one file as it was written.
type Recorded struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Snapshotter takes a consistent copy of the database into a file.
//
// It is an interface because a consistent snapshot is the database's own
// business — SQLite has VACUUM INTO, which copies without stopping writes —
// and this package should not know which database it is.
type Snapshotter interface {
	SnapshotInto(path string) error
}
