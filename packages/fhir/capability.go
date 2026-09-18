package fhir

import (
	"slices"
	"time"
)

// Interaction names one RESTful interaction, drawn from the R4 type-restful-
// interaction value set.
type Interaction string

// The interaction codes a route may be registered under. Naming one here does
// not serve it: what this build serves is whatever routes were registered, and
// the statement is built from those.
const (
	InteractionCreate          Interaction = "create"
	InteractionRead            Interaction = "read"
	InteractionVersionRead     Interaction = "vread"
	InteractionUpdate          Interaction = "update"
	InteractionDelete          Interaction = "delete"
	InteractionInstanceHistory Interaction = "history-instance"
)

// servedResourceTypes is the closed list of R4 types this build serves. Storage
// accepts any type name, so this list alone decides which names are endpoints:
// it gates every route and fills the statement, so the two cannot disagree.
//
// Every entry is a type authz classifies as carrying no patient data — directory,
// terminology, conformance and definitional content. A clinical type is withheld
// not for want of authentication, which now exists, but because no policy can
// authorize creating one: ResourceStore.Create requires an unconfined write and
// read, and an unconfined rule over a clinical type is refused. Advertising one
// would publish a create nobody can perform.
var servedResourceTypes = []string{
	"ActivityDefinition",
	"CapabilityStatement",
	"ChargeItemDefinition",
	"CodeSystem",
	"CompartmentDefinition",
	"ConceptMap",
	"DeviceDefinition",
	"Endpoint",
	"EventDefinition",
	"ExampleScenario",
	"GraphDefinition",
	"HealthcareService",
	"ImplementationGuide",
	"InsurancePlan",
	"Library",
	"Location",
	"Measure",
	"Medication",
	"MedicationKnowledge",
	"MessageDefinition",
	"NamingSystem",
	"OperationDefinition",
	"Organization",
	"OrganizationAffiliation",
	"PlanDefinition",
	"Practitioner",
	"PractitionerRole",
	"Questionnaire",
	"ResearchDefinition",
	"ResearchElementDefinition",
	"Schedule",
	"SearchParameter",
	"Slot",
	"StructureDefinition",
	"StructureMap",
	"Substance",
	"TerminologyCapabilities",
	"ValueSet",
}

// ServesResourceType reports whether this build declares a type. An undeclared
// name is not an endpoint, whatever storage would accept.
func ServesResourceType(name string) bool {
	return slices.Contains(servedResourceTypes, name)
}

// ServedResourceTypes lists the declared types, for tests that must cover every
// one of them before it may be advertised.
func ServedResourceTypes() []string {
	return slices.Clone(servedResourceTypes)
}

// ResourceInteraction declares one interaction on one resource type. R4 models
// it as a backbone element, so a bare code string would not validate.
type ResourceInteraction struct {
	Code Interaction `json:"code"`
}

// ResourceCapability declares the interactions Ilavrita supports for one
// resource type.
type ResourceCapability struct {
	Type         string                `json:"type"`
	Interaction  []ResourceInteraction `json:"interaction,omitempty"`
	Versioning   string                `json:"versioning"`
	UpdateCreate bool                  `json:"updateCreate"`
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

// CapabilityConfig is what a build must state to describe its own FHIR surface.
// Interactions are the codes the routes were registered under, so a statement
// can only ever be built from what is dispatched.
type CapabilityConfig struct {
	SoftwareVersion string
	Published       time.Time
	BaseURL         string
	Interactions    []Interaction
}

// NewCapabilityStatement describes this deployment. It states what the server
// can do at all; what one caller may do within that is decided per request.
func NewCapabilityStatement(config CapabilityConfig) CapabilityStatement {
	return CapabilityStatement{
		ResourceType: "CapabilityStatement",
		Status:       "draft",
		Experimental: true,
		Date:         config.Published.UTC().Format(time.RFC3339),
		Kind:         "instance",
		FHIRVersion:  Release,
		Format:       []string{ContentType},
		Software: SoftwareCapability{
			Name:    "Ilavrita",
			Version: config.SoftwareVersion,
		},
		Implementation: ImplementationDetail{
			Description: "Ilavrita server. The by-key interactions below are implemented; " +
				"search, transactions and conditional operations answer 501.",
			URL: config.BaseURL,
		},
		Rest: []RestCapability{{Mode: "server", Resource: servedResources(config.Interactions)}},
	}
}

// servedResources gives every declared type the interactions the router
// registered, which is what makes the declaration match the dispatch. Serving
// none declares no resource: a type with nothing to do on it is not an endpoint.
func servedResources(interactions []Interaction) []ResourceCapability {
	if len(interactions) == 0 {
		return nil
	}

	resources := make([]ResourceCapability, 0, len(servedResourceTypes))
	for _, name := range servedResourceTypes {
		resources = append(resources, ResourceCapability{
			Type:         name,
			Interaction:  declared(interactions),
			Versioning:   "versioned",
			UpdateCreate: true,
		})
	}

	return resources
}

// declared wraps each interaction code in the backbone element R4 requires.
func declared(interactions []Interaction) []ResourceInteraction {
	codes := make([]ResourceInteraction, 0, len(interactions))
	for _, code := range interactions {
		codes = append(codes, ResourceInteraction{Code: code})
	}

	return codes
}
