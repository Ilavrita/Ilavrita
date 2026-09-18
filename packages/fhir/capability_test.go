package fhir

import (
	"slices"
	"testing"
	"time"
)

// registered stands in for the router's own table. The statement declares what
// it is handed, so a test hands it a set and checks what comes back.
var registered = []Interaction{
	InteractionCreate, InteractionRead, InteractionVersionRead,
	InteractionUpdate, InteractionDelete, InteractionInstanceHistory,
}

func statement() CapabilityStatement {
	return statementServing(registered)
}

func statementServing(interactions []Interaction) CapabilityStatement {
	return NewCapabilityStatement(CapabilityConfig{
		SoftwareVersion: "0.0.1",
		Published:       time.Unix(1_700_000_000, 0),
		BaseURL:         "https://example.org/fhir/R4",
		Interactions:    interactions,
	})
}

// R4 requires status, date, kind, fhirVersion and at least one format.
func TestRequiredElementsArePresent(t *testing.T) {
	got := statement()

	for name, value := range map[string]string{
		"status":      got.Status,
		"date":        got.Date,
		"kind":        got.Kind,
		"fhirVersion": got.FHIRVersion,
	} {
		if value == "" {
			t.Errorf("%s is empty; R4 requires it", name)
		}
	}

	if len(got.Format) == 0 {
		t.Error("format is empty; R4 requires at least one")
	}
}

func TestDateIsAnRFC3339Instant(t *testing.T) {
	if _, err := time.Parse(time.RFC3339, statement().Date); err != nil {
		t.Errorf("date %q is not a valid dateTime: %v", statement().Date, err)
	}
}

// R4 invariant cpb-2: if kind is "instance", implementation must be present.
func TestAnInstanceStatementCarriesItsImplementation(t *testing.T) {
	got := statement()

	if got.Kind == "instance" && got.Implementation.Description == "" {
		t.Error("kind is instance but implementation is absent, violating cpb-2")
	}
}

// R4 invariant cpb-1: at least one of rest, messaging or document.
func TestStatementDeclaresARestEndpoint(t *testing.T) {
	if len(statement().Rest) == 0 {
		t.Error("no rest element, violating cpb-1")
	}
}

// One set of generic handlers serves every declared type, so a type advertising
// a different interaction set would describe a dispatch that does not exist.
func TestEveryAdvertisedResourceDeclaresTheRegisteredInteractions(t *testing.T) {
	declaredTypes := ServedResourceTypes()
	if len(declaredTypes) == 0 {
		t.Fatal("no resource type is advertised, so no interaction is reachable")
	}

	for _, rest := range statement().Rest {
		if len(rest.Resource) != len(declaredTypes) {
			t.Fatalf("advertised %d resource(s), want %d", len(rest.Resource), len(declaredTypes))
		}

		for _, resource := range rest.Resource {
			assertAdvertised(t, resource, registered)
		}
	}
}

// Nothing the caller did not register may appear: a build serving one
// interaction advertises that one, never the set this package happens to name.
func TestOnlyTheRegisteredInteractionsAreAdvertised(t *testing.T) {
	served := []Interaction{InteractionRead}

	for _, rest := range statementServing(served).Rest {
		for _, resource := range rest.Resource {
			assertAdvertised(t, resource, served)
		}
	}
}

// A build that registered no route advertises no resource at all, rather than a
// type whose empty interaction list still reads as an endpoint.
func TestWithNothingRegisteredNoResourceIsAdvertised(t *testing.T) {
	for _, rest := range statementServing(nil).Rest {
		if len(rest.Resource) != 0 {
			t.Errorf("advertised %d resource(s) with no interaction registered", len(rest.Resource))
		}
	}
}

func assertAdvertised(t *testing.T, resource ResourceCapability, want []Interaction) {
	t.Helper()

	if !ServesResourceType(resource.Type) {
		t.Errorf("%s is advertised but is not a type this build serves", resource.Type)
	}

	advertisedCodes := make([]Interaction, 0, len(resource.Interaction))
	for _, interaction := range resource.Interaction {
		advertisedCodes = append(advertisedCodes, interaction.Code)
	}

	if !slices.Equal(advertisedCodes, want) {
		t.Errorf("%s declares %v, want %v", resource.Type, advertisedCodes, want)
	}
}

// Nothing may be advertised that the route guard would then refuse, and nothing
// reachable may go unadvertised: one list decides both.
func TestAnUndeclaredTypeIsNotServed(t *testing.T) {
	for _, name := range []string{"", "patient", "NoSuchType", "Organization ", "Appointment", "Provenance"} {
		if ServesResourceType(name) {
			t.Errorf("%q is reported as served but is not declared", name)
		}
	}
}

// TestEveryServedClinicalTypeCanBePlacedInACompartment. A clinical type this
// build cannot place is one no confined grant could ever reach, so advertising it
// would publish a create nobody can perform. Appointment and Provenance are
// withheld for exactly that reason: their links are nested.
func TestEveryServedClinicalTypeCanBePlacedInACompartment(t *testing.T) {
	for _, name := range []string{"Patient", "Observation", "Condition", "Encounter"} {
		if !ServesResourceType(name) {
			t.Errorf("%s is not served", name)
		}

		if !DerivesCompartments(name) {
			t.Errorf("%s is served but cannot be placed in a compartment", name)
		}
	}

	for _, name := range []string{"Appointment", "Provenance"} {
		if ServesResourceType(name) {
			t.Errorf("%s is served though its compartment link is nested and underived", name)
		}
	}
}

func TestTheStatementIsMarkedExperimental(t *testing.T) {
	if !statement().Experimental {
		t.Error("a server implementing no interaction must not present itself as production")
	}
}
