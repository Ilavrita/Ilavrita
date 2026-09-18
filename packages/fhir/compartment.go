package fhir

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// compartmentTypes are the five subjects R4 defines a compartment for. A
// reference to anything else places a resource nowhere, which is what makes an
// unclassified link contribute no reach rather than an unnoticed one.
var compartmentTypes = []string{"Patient", "Encounter", "RelatedPerson", "Practitioner", "Device"}

// compartmentPaths names the top-level elements whose reference places a
// resource in a compartment, following the R4 CompartmentDefinition resources.
//
// Only top-level elements are read. A type whose link is nested — Appointment
// through participant.actor, Provenance through target — derives nothing here,
// so it is not served: a clinical resource landing in no compartment is
// unreachable by any confined grant, and advertising one would publish a create
// nobody can perform.
var compartmentPaths = map[string][]string{
	"Account":             {"subject"},
	"AdverseEvent":        {"subject", "recorder"},
	"AllergyIntolerance":  {"patient", "encounter", "recorder", "asserter"},
	"AppointmentResponse": {"actor"},
	// Binary is the one type here that R4 places in no compartment of its own:
	// it is bytes, and what they are about is only knowable from whatever points
	// at them. securityContext is the element R4 added for exactly this — the
	// resource that governs access to the payload — so it is what places one.
	//
	// A Binary naming none lands nowhere, and a confined caller cannot write it.
	// That is the right answer: an unattributed blob in a clinical server is a
	// document nobody can say whose it is.
	"Binary": {"securityContext"},

	"BodyStructure":               {"patient"},
	"CarePlan":                    {"subject", "encounter"},
	"CareTeam":                    {"subject", "encounter"},
	"ChargeItem":                  {"subject", "context", "enterer"},
	"Claim":                       {"patient", "provider"},
	"ClaimResponse":               {"patient"},
	"ClinicalImpression":          {"subject", "encounter", "assessor"},
	"Communication":               {"subject", "encounter", "sender"},
	"CommunicationRequest":        {"subject", "encounter", "requester", "sender"},
	"Composition":                 {"subject", "encounter", "author"},
	"Condition":                   {"subject", "encounter", "recorder", "asserter"},
	"Consent":                     {"patient"},
	"Contract":                    {"subject"},
	"Coverage":                    {"beneficiary", "policyHolder", "subscriber"},
	"CoverageEligibilityRequest":  {"patient"},
	"CoverageEligibilityResponse": {"patient"},
	"DetectedIssue":               {"patient", "author"},
	"Device":                      {"patient"},
	"DeviceRequest":               {"subject", "encounter", "performer"},
	"DeviceUseStatement":          {"subject", "device"},
	"DiagnosticReport":            {"subject", "encounter"},
	"DocumentManifest":            {"subject", "author", "recipient"},
	"DocumentReference":           {"subject", "author"},
	"Encounter":                   {"subject"},
	"EpisodeOfCare":               {"patient", "careManager"},
	"ExplanationOfBenefit":        {"patient", "provider"},
	"FamilyMemberHistory":         {"patient"},
	"Flag":                        {"subject", "encounter", "author"},
	"Goal":                        {"subject"},
	"GuidanceResponse":            {"subject", "encounter", "performer"},
	"ImagingStudy":                {"subject", "encounter", "referrer", "interpreter"},
	"Immunization":                {"patient", "encounter"},
	"ImmunizationEvaluation":      {"patient"},
	"ImmunizationRecommendation":  {"patient"},
	"Invoice":                     {"subject", "recipient"},
	"List":                        {"subject", "source", "encounter"},
	"MeasureReport":               {"subject", "reporter"},
	"Media":                       {"subject", "encounter", "operator", "device"},
	"MedicationAdministration":    {"subject", "context", "performer"},
	"MedicationDispense":          {"subject", "context"},
	"MedicationRequest":           {"subject", "encounter", "requester"},
	"MedicationStatement":         {"subject", "context"},
	"MolecularSequence":           {"patient"},
	"NutritionOrder":              {"patient", "encounter", "orderer"},
	"Observation":                 {"subject", "encounter", "performer"},
	"Procedure":                   {"subject", "encounter", "asserter", "recorder"},
	"QuestionnaireResponse":       {"subject", "encounter", "author", "source"},
	"RelatedPerson":               {"patient"},
	"RequestGroup":                {"subject", "encounter", "author"},
	"ResearchSubject":             {"individual"},
	"RiskAssessment":              {"subject", "encounter", "performer"},
	"ServiceRequest":              {"subject", "encounter", "requester", "performer"},
	"Specimen":                    {"subject"},
	"SupplyDelivery":              {"patient", "supplier", "receiver"},
	"Task":                        {"for", "encounter", "owner", "requester"},
	"VisionPrescription":          {"patient", "encounter", "prescriber"},
}

// DerivesCompartments reports whether this build can place a resource of this
// type in a compartment. A type it cannot place is one no confined grant can
// reach, which is why such a type is not advertised.
func DerivesCompartments(resourceType string) bool {
	if slices.Contains(compartmentTypes, resourceType) {
		return true
	}

	_, known := compartmentPaths[resourceType]

	return known
}

// Compartments derives the compartments a resource body places it in. It is the
// whole of what a confined grant is checked against at create, so a link this
// function does not read is reach the resource does not get — never reach it
// gets silently.
//
// A resource whose own type is a compartment subject is in its own compartment:
// a Patient is in the Patient compartment it names.
func Compartments(
	resourceType string, id storage.LogicalID, content json.RawMessage,
) ([]storage.Compartment, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return nil, fmt.Errorf("fhir: read %s to derive its compartments: %w", resourceType, err)
	}

	var derived []storage.Compartment

	if slices.Contains(compartmentTypes, resourceType) && id != "" {
		derived = append(derived, storage.Compartment{
			Type: storage.ResourceType(resourceType), ID: id,
		})
	}

	for _, path := range compartmentPaths[resourceType] {
		raw, carried := fields[path]
		if !carried {
			continue
		}

		found, err := referenced(raw)
		if err != nil {
			return nil, fmt.Errorf("fhir: read %s.%s: %w", resourceType, path, err)
		}

		for _, compartment := range found {
			if !slices.Contains(derived, compartment) {
				derived = append(derived, compartment)
			}
		}
	}

	return derived, nil
}

// referenced reads one element, which R4 models as either a single Reference or
// an array of them, and returns whatever compartments its references name.
func referenced(raw json.RawMessage) ([]storage.Compartment, error) {
	if trimmed := strings.TrimSpace(string(raw)); !strings.HasPrefix(trimmed, "[") {
		compartment, named, err := reference(raw)
		if err != nil || !named {
			return nil, err
		}

		return []storage.Compartment{compartment}, nil
	}

	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}

	var found []storage.Compartment

	for _, one := range many {
		compartment, named, err := reference(one)
		if err != nil {
			return nil, err
		}

		if named {
			found = append(found, compartment)
		}
	}

	return found, nil
}

// reference reads one Reference and reports the compartment it names, if it
// names one this server recognises. A logical reference, a contained one and an
// absolute URL all name nothing here: none is a current local row, so no
// compartment predicate could match them.
func reference(raw json.RawMessage) (storage.Compartment, bool, error) {
	var held struct {
		Reference string `json:"reference"`
	}

	// An element that is not an object is not a Reference. This function is not
	// the validator, so it reads nothing there rather than refusing the resource.
	if err := json.Unmarshal(raw, &held); err != nil {
		return storage.Compartment{}, false, nil
	}

	subject, id, split := strings.Cut(held.Reference, "/")
	if !split || subject == "" || id == "" {
		return storage.Compartment{}, false, nil
	}

	// A versioned or absolute reference names no current local row.
	if strings.Contains(id, "/") || strings.Contains(subject, ":") {
		return storage.Compartment{}, false, nil
	}

	if !slices.Contains(compartmentTypes, subject) {
		return storage.Compartment{}, false, nil
	}

	return storage.Compartment{
		Type: storage.ResourceType(subject), ID: storage.LogicalID(id),
	}, true, nil
}
