package storage

import "context"

// ResourceRepository reads and writes the current state of a resource. Scope
// leads every method on purpose: a parameter cannot be forgotten, and naming a
// Project in a key is not proof of entitlement.
type ResourceRepository interface {
	Read(ctx context.Context, scope Scope, key ResourceKey) (ResourceRecord, error)
	Create(ctx context.Context, scope Scope, record ResourceRecord) error
	Update(ctx context.Context, scope Scope, record ResourceRecord, expect VersionID) error
	Delete(ctx context.Context, scope Scope, key ResourceKey, expect VersionID) error
}

// VersionStore reads the immutable history of a resource. History is its own
// evidence, not reconstructed from audit logs, which may be pruned.
type VersionStore interface {
	ReadVersion(ctx context.Context, scope Scope, key ResourceKey, version VersionID) (ResourceRecord, error)
	// ListVersions reads one page of a resource's history, newest first. It is
	// paged because a resource written to for years has a history no client
	// asked to receive in one response — and because one pooled connection per
	// process means a long read is every other request waiting behind it.
	ListVersions(
		ctx context.Context, scope Scope, key ResourceKey, window VersionWindow,
	) (VersionPage, error)
}

// Transactor runs work inside a single commit boundary. A write, its version, its
// indexes and its audit record commit together or not at all.
type Transactor interface {
	WithinTransaction(ctx context.Context, work func(ctx context.Context) error) error
}
