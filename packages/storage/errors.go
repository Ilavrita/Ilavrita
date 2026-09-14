package storage

import "errors"

var (
	// ErrNotFound reports that no resource exists for the requested key.
	ErrNotFound = errors.New("storage: resource not found")

	// ErrDeleted reports that the resource existed and was deleted. It is
	// distinct from ErrNotFound because FHIR answers the two cases differently
	// (FR-005).
	ErrDeleted = errors.New("storage: resource deleted")

	// ErrVersionConflict reports that the caller's expected version is stale,
	// which is how optimistic concurrency surfaces to the FHIR layer (FR-004).
	ErrVersionConflict = errors.New("storage: version conflict")
)
