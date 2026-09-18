package main

import (
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// TestNoUnrestrictedRuleReachesPatientData is what replaced the gate that
// withheld clinical types altogether. They are served now, and this is the line
// that makes that safe: a Project authorizes one through a compartment it names,
// and an unrestricted rule over one is refused however it is authored.
func TestNoUnrestrictedRuleReachesPatientData(t *testing.T) {
	var clinical int

	for _, name := range fhir.ServedResourceTypes() {
		resourceType := storage.ResourceType(name)

		if !authz.CarriesClinicalData(resourceType) {
			continue
		}

		clinical++

		_, err := authz.NewUnrestrictedRule(storage.KindFHIR, resourceType, storage.ActionRead)
		if err == nil {
			t.Errorf("an unrestricted rule may cover %s, which reaches patient data", name)
		}
	}

	if clinical == 0 {
		t.Error("no clinical type is served, so this proves nothing")
	}
}

// TestEveryNonClinicalTypeStaysUnrestrictable. The directory and terminology
// types carry no patient data, so a Project may grant them outright — losing that
// would make an ordinary read unauthorizable.
func TestEveryNonClinicalTypeStaysUnrestrictable(t *testing.T) {
	for _, name := range fhir.ServedResourceTypes() {
		resourceType := storage.ResourceType(name)

		if authz.CarriesClinicalData(resourceType) {
			continue
		}

		if _, err := authz.NewUnrestrictedRule(
			storage.KindFHIR, resourceType, storage.ActionRead); err != nil {
			t.Errorf("no unrestricted rule may cover %s, so nothing could reach it: %v", name, err)
		}
	}
}

// TestAServedTypeIsOneOrTheOther. Every advertised type is reachable by exactly
// one of the two routes — an unrestricted grant, or a compartment it declares —
// so none is advertised that no policy could ever authorize.
func TestAServedTypeIsOneOrTheOther(t *testing.T) {
	for _, name := range fhir.ServedResourceTypes() {
		unrestrictable := !authz.CarriesClinicalData(storage.ResourceType(name))
		placeable := fhir.DerivesCompartments(name)

		if !unrestrictable && !placeable {
			t.Errorf("%s is advertised but no grant could reach it", name)
		}
	}
}
