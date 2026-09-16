package fhir

import "time"

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

// ImplementationDetail describes this particular deployment. R4 requires it
// whenever kind is "instance".
type ImplementationDetail struct {
	Description string `json:"description"`
	URL         string `json:"url,omitempty"`
}

// SoftwareCapability identifies the running build.
type SoftwareCapability struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CapabilityStatement declares what this server can actually do. It must never
// advertise an interaction that is not implemented and covered by tests.
type CapabilityStatement struct {
	ResourceType   string               `json:"resourceType"`
	Status         string               `json:"status"`
	Experimental   bool                 `json:"experimental"`
	Date           string               `json:"date"`
	Kind           string               `json:"kind"`
	FHIRVersion    string               `json:"fhirVersion"`
	Format         []string             `json:"format"`
	Software       SoftwareCapability   `json:"software"`
	Implementation ImplementationDetail `json:"implementation"`
	Rest           []RestCapability     `json:"rest"`
}

// NewCapabilityStatement describes this deployment. The resource list is empty
// until an interaction ships with test coverage behind it.
func NewCapabilityStatement(softwareVersion string, published time.Time, baseURL string) CapabilityStatement {
	return CapabilityStatement{
		ResourceType: "CapabilityStatement",
		Status:       "draft",
		Experimental: true,
		Date:         published.UTC().Format(time.RFC3339),
		Kind:         "instance",
		FHIRVersion:  Release,
		Format:       []string{ContentType},
		Software: SoftwareCapability{
			Name:    "Ilavrita",
			Version: softwareVersion,
		},
		Implementation: ImplementationDetail{
			Description: "Ilavrita server. No FHIR interaction is implemented; every route below the base path answers 501.",
			URL:         baseURL,
		},
		Rest: []RestCapability{{Mode: "server"}},
	}
}
