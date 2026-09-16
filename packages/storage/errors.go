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

	// ErrAlreadyExists reports that a create named a logical id another resource
	// already holds. A create never falls through to an update, so a taken id is
	// a conflict rather than an overwrite.
	ErrAlreadyExists = errors.New("storage: resource already exists")

	// ErrDenied reports that the Scope authorizes no Grant for this operation.
	// It is distinct from ErrNotFound, which is what an out-of-scope row reads
	// as, so a caller cannot probe for ids it may not see.
	ErrDenied = errors.New("storage: scope authorizes nothing for this operation")
)
