package fhir

import (
	"encoding/json"
	"slices"
)

// BundleType names what a Bundle collects.
type BundleType string

// BundleHistory collects the versions of one resource. It is the only Bundle
// type Ilavrita builds today.
const BundleHistory BundleType = "history"

// HTTPVerb is the interaction one Bundle entry records.
type HTTPVerb string

// The verbs a history entry can record.
const (
	VerbPost   HTTPVerb = "POST"
	VerbPut    HTTPVerb = "PUT"
	VerbDelete HTTPVerb = "DELETE"
)

// Bundle is a collection of entries. Only history is built here; search and
// transaction Bundles belong to interactions that are not implemented.
type Bundle struct {
	ResourceType string        `json:"resourceType"`
	Type         BundleType    `json:"type"`
	Total        int           `json:"total"`
	Entry        []BundleEntry `json:"entry,omitempty"`
}

// BundleEntry is one version within a history Bundle. Resource is absent
// exactly when the entry records a deletion, which carries no body.
type BundleEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource,omitempty"`
	Request  EntryRequest    `json:"request"`
	Response EntryResponse   `json:"response"`
}

// EntryRequest records the interaction that produced a version.
type EntryRequest struct {
	Method HTTPVerb `json:"method"`
	URL    string   `json:"url"`
}

// EntryResponse records what that interaction answered.
type EntryResponse struct {
	Status       string `json:"status"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

// NewHistoryBundle collects one resource's versions. The entries keep the order
// they arrive in, which is newest first.
func NewHistoryBundle(entries []BundleEntry) Bundle {
	return Bundle{
		ResourceType: "Bundle",
		Type:         BundleHistory,
		Total:        len(entries),
		Entry:        slices.Clone(entries),
	}
}
