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
	InteractionSearchType      Interaction = "search-type"
)

// servedResourceTypes is the closed list of R4 types this build serves. Storage
// accepts any type name, so this list alone decides which names are endpoints:
// it gates every route and fills the statement, so the two cannot disagree.
//
// The directory, terminology, conformance and definitional types are here
// alongside the clinical ones. A clinical type reaches patient data, so an
// unrestricted rule may not cover it: a Project authorizes one through a
// compartment-restricted grant, and a create is checked against the compartments
// the submitted resource itself declares. Every clinical entry below is a type
// Compartments can place, because one it cannot place would be a create no
// confined grant could ever authorize.
var servedResourceTypes = []string{
	"ActivityDefinition",
	"CapabilityStatement",
	"CatalogEntry",
	"ChargeItemDefinition",
	"CodeSystem",
	"CompartmentDefinition",
	"ConceptMap",
	"DeviceDefinition",
	"EffectEvidenceSynthesis",
	"Endpoint",
	"EventDefinition",
	"Evidence",
	"EvidenceVariable",
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
	"MedicinalProduct",
	"MedicinalProductAuthorization",
	"MedicinalProductContraindication",
	"MedicinalProductIndication",
	"MedicinalProductIngredient",
	"MedicinalProductInteraction",
	"MedicinalProductManufactured",
	"MedicinalProductPackaged",
	"MedicinalProductPharmaceutical",
	"MedicinalProductUndesirableEffect",
	"MessageDefinition",
	"NamingSystem",
	"ObservationDefinition",
	"OperationDefinition",
	"Organization",
	"OrganizationAffiliation",
	"PlanDefinition",
	"Practitioner",
	"PractitionerRole",
	"Questionnaire",
	"ResearchDefinition",
	"ResearchElementDefinition",
	"RiskEvidenceSynthesis",
	"Schedule",
	"SearchParameter",
	"Slot",
	"SpecimenDefinition",
	"StructureDefinition",
	"StructureMap",
	"Subscription",
	"Substance",
	"SubstanceNucleicAcid",
	"SubstancePolymer",
	"SubstanceProtein",
	"SubstanceReferenceInformation",
	"SubstanceSourceMaterial",
	"SubstanceSpecification",
	"TerminologyCapabilities",
	"TestReport",
	"TestScript",
	"ValueSet",

	// Clinical types, reachable only through a compartment-restricted grant.
	"Account",
	"AdverseEvent",
	"AllergyIntolerance",
	"AppointmentResponse",
	"Binary",
	"BodyStructure",
	"CarePlan",
	"CareTeam",
	"ChargeItem",
	"Claim",
	"ClaimResponse",
	"ClinicalImpression",
	"Communication",
	"CommunicationRequest",
	"Composition",
	"Condition",
	"Consent",
	"Contract",
	"Coverage",
	"CoverageEligibilityRequest",
	"CoverageEligibilityResponse",
	"DetectedIssue",
	"Device",
	"DeviceRequest",
	"DeviceUseStatement",
	"DiagnosticReport",
	"DocumentManifest",
	"DocumentReference",
	"Encounter",
	"EpisodeOfCare",
	"ExplanationOfBenefit",
	"FamilyMemberHistory",
	"Flag",
	"Goal",
	"GuidanceResponse",
	"ImagingStudy",
	"Immunization",
	"ImmunizationEvaluation",
	"ImmunizationRecommendation",
	"Invoice",
	"List",
	"MeasureReport",
	"Media",
	"MedicationAdministration",
	"MedicationDispense",
	"MedicationRequest",
	"MedicationStatement",
	"MolecularSequence",
	"NutritionOrder",
	"Observation",
	"Patient",
	"Procedure",
	"QuestionnaireResponse",
	"RelatedPerson",
	"RequestGroup",
	"ResearchSubject",
	"RiskAssessment",
	"ServiceRequest",
	"Specimen",
	"SupplyDelivery",
	"Task",
	"VisionPrescription",
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
	Type         string                  `json:"type"`
	Interaction  []ResourceInteraction   `json:"interaction,omitempty"`
	SearchParam  []SearchParamCapability `json:"searchParam,omitempty"`
	Versioning   string                  `json:"versioning"`
	UpdateCreate bool                    `json:"updateCreate"`
}

// SearchParamCapability declares one search parameter a type answers. A
// statement listing a parameter the server refuses would send clients to write
// queries it will not run.
type SearchParamCapability struct {
	Name string `json:"name"`
	Type string `json:"type"`
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

	// SearchParameters answers what one type may be searched by. It is supplied
	// rather than known here, because the registry of parameters and the routes
	// that serve them are the caller's to keep in step; this package would only
	// be a third place they could fall out of step at.
	SearchParameters func(resourceType string) []SearchParamCapability
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
			Description: "Ilavrita server. The interactions and search parameters below are " +
				"implemented; transactions and conditional operations answer 501.",
			URL: config.BaseURL,
		},
		Rest: []RestCapability{{Mode: "server", Resource: servedResources(config)}},
	}
}

// servedResources gives every declared type the interactions the router
// registered, which is what makes the declaration match the dispatch. Serving
// none declares no resource: a type with nothing to do on it is not an endpoint.
func servedResources(config CapabilityConfig) []ResourceCapability {
	if len(config.Interactions) == 0 {
		return nil
	}

	searchable := slices.Contains(config.Interactions, InteractionSearchType)

	resources := make([]ResourceCapability, 0, len(servedResourceTypes))

	for _, name := range servedResourceTypes {
		held := ResourceCapability{
			Type:         name,
			Interaction:  declared(config.Interactions),
			Versioning:   "versioned",
			UpdateCreate: true,
		}

		// Parameters are advertised only where the interaction that uses them
		// is, so a statement cannot name a way to search a server that does not.
		if searchable && config.SearchParameters != nil {
			held.SearchParam = config.SearchParameters(name)
		}

		resources = append(resources, held)
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
