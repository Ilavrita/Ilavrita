// Package files abstracts payload storage for Binary and DocumentReference
// content, over local disk or an S3-compatible object store.
package files

import (
	"context"
	"errors"
	"io"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrNotFound reports a payload that is not there. A caller must not be able
	// to tell that from one it may not read, so a route answers both the way it
	// answers a missing resource.
	ErrNotFound = errors.New("files: no payload is stored under that key")

	// ErrTooLarge reports a payload past what this deployment accepts. It is
	// refused while it is being read rather than after: a limit checked at the
	// end is one the disk has already paid for.
	ErrTooLarge = errors.New("files: the payload is larger than this deployment accepts")

	// ErrMalformedKey reports a key that names no payload this store could hold.
	ErrMalformedKey = errors.New("files: the payload key is malformed")
)

// Key names one payload. It carries the resource's own identity and the version
// it belongs to, so an updated Binary does not overwrite the bytes its earlier
// version served and a history read still answers with what was written then.
type Key struct {
	Project storage.ProjectID
	Type    storage.ResourceType
	ID      storage.LogicalID
	Version storage.VersionID
}

// Valid reports whether every part of the key is named.
func (k Key) Valid() bool {
	return k.Project != "" && k.Type != "" && k.ID != "" && k.Version != ""
}

// Stored is what a payload turned out to be.
type Stored struct {
	// Media is the content type the payload was written under, which is what a
	// read serves it back as. It is recorded here rather than guessed from the
	// bytes: what a client called it is what this server hands back.
	Media string

	Size int64

	// Digest is the SHA-256 of the bytes, hex-encoded. It is what an ETag on a
	// payload route is built from, and what tells a later read that the file on
	// disk is the one that was written.
	Digest string
}

// Store keeps payloads outside the database.
//
// A resource's JSON row carries what the payload is — its media type, its size,
// what it belongs to — and this keeps the bytes. Putting megabytes in the row
// would make every read of the metadata pay for them, and every backup of the
// database carry them.
type Store interface {
	// Put writes one payload, refusing anything past limit bytes while it reads
	// rather than after.
	Put(ctx context.Context, key Key, media string, body io.Reader, limit int64) (Stored, error)

	// Open returns the payload's bytes. The caller closes them.
	Open(ctx context.Context, key Key) (io.ReadCloser, Stored, error)

	// Describe reports what a payload is without reading it.
	Describe(ctx context.Context, key Key) (Stored, error)

	// Remove deletes every version of one resource's payload. It is what a
	// Project's purge calls, not what a FHIR delete calls: a deleted resource
	// keeps its history, and its payloads are that history.
	Remove(ctx context.Context, project storage.ProjectID, resourceType storage.ResourceType, id storage.LogicalID) error
}
