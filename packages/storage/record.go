package storage

import (
	"encoding/json"
	"time"
)

// ResourceRecord is one stored resource version. Content holds the complete FHIR
// JSON and is the source of truth; derived search indexes rebuild from it.
type ResourceRecord struct {
	Key         ResourceKey
	Version     VersionID
	LastUpdated time.Time
	Deleted     bool
	Content     json.RawMessage
}
