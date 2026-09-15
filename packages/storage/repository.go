package storage

import "context"

// ResourceRepository reads and writes the current state of a resource.
type ResourceRepository interface {
	Read(ctx context.Context, key ResourceKey) (ResourceRecord, error)
	Write(ctx context.Context, record ResourceRecord) error
}

// VersionStore reads the immutable history of a resource. History is its own
// evidence, not reconstructed from audit logs, which may be pruned.
type VersionStore interface {
	ReadVersion(ctx context.Context, key ResourceKey, version VersionID) (ResourceRecord, error)
	ListVersions(ctx context.Context, key ResourceKey) ([]ResourceRecord, error)
}

// Transactor runs work inside a single commit boundary. A write, its version, its
// indexes and its audit record commit together or not at all.
type Transactor interface {
	WithinTransaction(ctx context.Context, work func(ctx context.Context) error) error
}
