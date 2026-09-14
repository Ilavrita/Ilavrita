package storage

import (
	"encoding/json"
	"time"
)

// ResourceRecord is one stored resource version.
//
// Content holds the complete FHIR JSON and is the source of truth. Everything
// else is metadata the server maintains, and search indexes derived from
// Content are always rebuildable from it (FR-022, FR-024).
type ResourceRecord struct {
	Key         ResourceKey
	Version     VersionID
	LastUpdated time.Time
	Deleted     bool
	Content     json.RawMessage
}
