package main

import (
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// TestEveryServedTypeCarriesNoClinicalData is the line between what this build
// serves and what it is ready to serve. Authentication exists as a store and not
// yet as a route, so nothing here turns a request into a principal: a type
// carrying patient data would be reachable only through the development
// principal, which checks no credential at all.
//
// This is the test to delete, deliberately and in its own commit, on the day a
// login route lands.
func TestEveryServedTypeCarriesNoClinicalData(t *testing.T) {
	for _, name := range fhir.ServedResourceTypes() {
		if authz.CarriesClinicalData(storage.ResourceType(name)) {
			t.Errorf("%s carries patient data and is served on a build with no login route", name)
		}
	}
}

// TestTheClinicalTypesAreDeliberatelyWithheld names what is missing, so the gap
// reads as a decision rather than an oversight.
func TestTheClinicalTypesAreDeliberatelyWithheld(t *testing.T) {
	for _, name := range []string{"Patient", "Observation", "Encounter", "Condition", "Binary"} {
		if fhir.ServesResourceType(name) {
			t.Errorf("%s is served though no route authenticates anyone", name)
		}

		if !authz.CarriesClinicalData(storage.ResourceType(name)) {
			t.Errorf("%s is classified as carrying no patient data, which this gate relies on", name)
		}
	}
}

// TestAServedTypeMayCarryAnUnrestrictedPolicy ties the two lists together: a type
// this build serves must be one an unrestricted rule may cover, or a Project
// could not author a policy reaching it at all.
func TestAServedTypeMayCarryAnUnrestrictedPolicy(t *testing.T) {
	for _, name := range fhir.ServedResourceTypes() {
		_, err := authz.NewUnrestrictedRule(storage.KindFHIR, storage.ResourceType(name), storage.ActionRead)
		if err != nil {
			t.Errorf("no unrestricted rule may cover %s, so nothing could reach it: %v", name, err)
		}
	}
}
