package fhir

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func derived(t *testing.T, resourceType string, id storage.LogicalID, body string) []storage.Compartment {
	t.Helper()

	found, err := Compartments(resourceType, id, json.RawMessage(body))
	if err != nil {
		t.Fatalf("derive %s compartments: %v", resourceType, err)
	}

	return found
}

// TestAReferencePlacesAResourceInACompartment is the whole mechanism: a confined
// grant is checked against what this returns, so a link it does not read is
// reach the resource does not get.
func TestAReferencePlacesAResourceInACompartment(t *testing.T) {
	found := derived(t, "Observation", "obs-1",
		`{"resourceType":"Observation","subject":{"reference":"Patient/pat-1"}}`)

	want := storage.Compartment{Type: "Patient", ID: "pat-1"}
	if !slices.Contains(found, want) {
		t.Errorf("derived %v, want it to contain %v", found, want)
	}
}

// TestACompartmentSubjectIsItsOwnCompartment. A Patient is in the Patient
// compartment it names, which is what a confined read of that patient finds.
func TestACompartmentSubjectIsItsOwnCompartment(t *testing.T) {
	found := derived(t, "Patient", "pat-1", `{"resourceType":"Patient","id":"pat-1"}`)

	if len(found) != 1 || found[0] != (storage.Compartment{Type: "Patient", ID: "pat-1"}) {
		t.Errorf("derived %v, want the patient's own compartment alone", found)
	}
}

// TestAResourceNamingNoSubjectIsPlacedNowhere. This is what makes an underived
// compartment fail closed: no confined grant covers a resource in no compartment.
func TestAResourceNamingNoSubjectIsPlacedNowhere(t *testing.T) {
	if found := derived(t, "Observation", "obs-1", `{"resourceType":"Observation"}`); len(found) != 0 {
		t.Errorf("derived %v, want nothing", found)
	}
}

// TestAnArrayOfReferencesPlacesEveryOne, because R4 models several of these
// elements as repeating.
func TestAnArrayOfReferencesPlacesEveryOne(t *testing.T) {
	found := derived(t, "Observation", "obs-1",
		`{"resourceType":"Observation","performer":[{"reference":"Practitioner/prac-1"},`+
			`{"reference":"Organization/org-1"}]}`)

	if !slices.Contains(found, storage.Compartment{Type: "Practitioner", ID: "prac-1"}) {
		t.Errorf("derived %v, want the practitioner", found)
	}

	// An Organization is not a compartment subject, so it places nothing.
	for _, compartment := range found {
		if compartment.Type == "Organization" {
			t.Errorf("an Organization reference placed the resource: %v", found)
		}
	}
}

// TestOnlyACompartmentSubjectPlacesAResource. A reference to anything else is
// reach the resource does not get, rather than reach nobody noticed.
func TestOnlyACompartmentSubjectPlacesAResource(t *testing.T) {
	bodies := map[string]string{
		"a non-subject type":  `{"resourceType":"Observation","subject":{"reference":"Group/grp-1"}}`,
		"a contained target":  `{"resourceType":"Observation","subject":{"reference":"#contained"}}`,
		"an absolute url":     `{"resourceType":"Observation","subject":{"reference":"http://x/Patient/p"}}`,
		"a versioned target":  `{"resourceType":"Observation","subject":{"reference":"Patient/p/_history/2"}}`,
		"a display-only ref":  `{"resourceType":"Observation","subject":{"display":"Someone"}}`,
		"an identifier-only":  `{"resourceType":"Observation","subject":{"identifier":{"value":"x"}}}`,
		"an unexpected shape": `{"resourceType":"Observation","subject":"Patient/pat-1"}`,
	}

	for name, body := range bodies {
		if found := derived(t, "Observation", "obs-1", body); len(found) != 0 {
			t.Errorf("%s placed the resource: %v", name, found)
		}
	}
}

// TestADuplicateReferencePlacesOnce, so a resource naming one patient twice
// carries one compartment row rather than two.
func TestADuplicateReferencePlacesOnce(t *testing.T) {
	found := derived(t, "Observation", "obs-1",
		`{"resourceType":"Observation","subject":{"reference":"Patient/pat-1"},`+
			`"performer":[{"reference":"Patient/pat-1"}]}`)

	if len(found) != 1 {
		t.Errorf("derived %v, want one compartment", found)
	}
}

// TestMalformedContentIsAnErrorRatherThanNoCompartments. A body this cannot read
// must not be treated as one placing the resource nowhere, because nowhere is a
// value a confined grant would then be checked against.
func TestMalformedContentIsAnErrorRatherThanNoCompartments(t *testing.T) {
	if _, err := Compartments("Observation", "obs-1", json.RawMessage(`not json`)); err == nil {
		t.Error("unreadable content derived no compartments instead of failing")
	}
}

// TestEveryPlaceableTypeIsOneThisBuildServes, and every clinical type it serves
// is placeable. The two lists decide together what may be advertised.
func TestEveryPlaceableTypeIsOneThisBuildServes(t *testing.T) {
	for resourceType := range compartmentPaths {
		if !ServesResourceType(resourceType) {
			t.Errorf("%s can be placed but is not served", resourceType)
		}
	}
}
