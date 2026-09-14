package storage

import "context"

// ResourceRepository reads and writes the current state of a resource.
type ResourceRepository interface {
	Read(ctx context.Context, key ResourceKey) (ResourceRecord, error)
	Write(ctx context.Context, record ResourceRecord) error
}

// VersionStore reads the immutable history of a resource.
//
// History is recorded as its own evidence rather than reconstructed from audit
// logs, which are written for a different purpose and may be pruned (FR-006).
type VersionStore interface {
	ReadVersion(ctx context.Context, key ResourceKey, version VersionID) (ResourceRecord, error)
	ListVersions(ctx context.Context, key ResourceKey) ([]ResourceRecord, error)
}

// Transactor runs work inside a single commit boundary.
//
// A resource write, its new version, its search indexes and its audit record
// belong to one transaction. A failure before commit must leave no partial
// state visible (FR-009, FR-023).
type Transactor interface {
	WithinTransaction(ctx context.Context, work func(ctx context.Context) error) error
}
