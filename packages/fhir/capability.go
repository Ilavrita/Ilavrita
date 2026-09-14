package fhir

// ResourceCapability declares the interactions Ilavrita supports for one
// resource type.
type ResourceCapability struct {
	Type        string   `json:"type"`
	Interaction []string `json:"interaction,omitempty"`
}

// RestCapability describes one RESTful endpoint of the server.
type RestCapability struct {
	Mode     string               `json:"mode"`
	Resource []ResourceCapability `json:"resource,omitempty"`
}

// SoftwareCapability identifies the running build.
type SoftwareCapability struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CapabilityStatement is the machine-readable declaration of what this server
// can actually do.
//
// It is the contract clients test against, so it must never advertise an
// interaction that is not implemented and covered by tests (FR-001, SM-008).
type CapabilityStatement struct {
	ResourceType string             `json:"resourceType"`
	Status       string             `json:"status"`
	Kind         string             `json:"kind"`
	FHIRVersion  string             `json:"fhirVersion"`
	Format       []string           `json:"format"`
	Software     SoftwareCapability `json:"software"`
	Rest         []RestCapability   `json:"rest"`
}

// NewCapabilityStatement describes the current build.
//
// The resource list is empty because no FHIR interaction is implemented yet.
// Entries are added only once the matching behaviour ships with test coverage.
func NewCapabilityStatement(softwareVersion string) CapabilityStatement {
	return CapabilityStatement{
		ResourceType: "CapabilityStatement",
		Status:       "draft",
		Kind:         "instance",
		FHIRVersion:  Release,
		Format:       []string{ContentType},
		Software: SoftwareCapability{
			Name:    "Ilavrita",
			Version: softwareVersion,
		},
		Rest: []RestCapability{{Mode: "server"}},
	}
}
