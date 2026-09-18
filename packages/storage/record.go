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

	// Compartments are the subjects this resource belongs to, derived from its
	// own content by the layer that understands the format. Storage accepts any
	// resource type and reads no field, so it cannot derive them: it writes what
	// it is given and checks a confined Grant against it.
	Compartments []Compartment
}
