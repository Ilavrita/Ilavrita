package main

import (
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// finalObservation is one reading in whatever state the case is about, always
// subject to the patient the conformance policy is confined to.
func finalObservation(status string) string {
	return `{"resourceType":"Observation","status":"` + status + `",` +
		`"subject":{"reference":"Patient/` + string(conformancePatient) + `"}}`
}

// filteredPolicy is the conformance policy with every clinical rule narrowed to
// one element value, which is the shape a reviewing clinician holds when
// preliminary results are not theirs to see. The non-clinical rules are left
// alone, so what a filter does and what it leaves alone are both observable.
func filteredPolicy(t *testing.T, filter storage.Filter) authz.AccessPolicy {
	t.Helper()

	var rules []authz.Rule

	for _, name := range fhir.ServedResourceTypes() {
		for _, action := range everyAction {
			rule := widestRule(t, storage.ResourceType(name), action)

			if authz.CarriesClinicalData(storage.ResourceType(name)) {
				rule = rule.WithFilter(filter)
			}

			rules = append(rules, rule)
		}
	}

	policy, err := authz.NewAccessPolicy(authz.PolicyConfig{
		Project: homeProject, ID: conformancePolicyID, Rules: rules,
	})
	if err != nil {
		t.Fatalf("author the filtered policy: %v", err)
	}

	return policy
}

// servingFilteredFHIR wires a whole server under one filtered policy.
func servingFilteredFHIR(t *testing.T, filter storage.Filter) http.Handler {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveUnder(t, db, homeProject, filteredPolicy(t, filter))

	return fhirRoutes(t)
}

func mustStatusFilter(t *testing.T, values ...string) storage.Filter {
	t.Helper()

	comparator := storage.ComparatorIn
	if len(values) == 1 {
		comparator = storage.ComparatorEqual
	}

	filter, err := storage.NewFilter("status", comparator, values...)
	if err != nil {
		t.Fatalf("build the status filter: %v", err)
	}

	return filter
}

// TestAPolicyFilterNarrowsARealRequest is the whole path end to end: a rule
// authored with a filter, compiled to a Grant, turned into a predicate, and
// answered over HTTP. Every layer between the policy and the response has to
// carry the restriction or this passes only by accident.
func TestAPolicyFilterNarrowsARealRequest(t *testing.T) {
	routes := servingFilteredFHIR(t, mustStatusFilter(t, "final"))

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: finalObservation("final"),
	}.send(t, routes)

	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("Observation", id),
	}.send(t, routes), http.StatusOK)

	// A reading the filter does not name cannot be written at all: the author
	// could not read back what they wrote.
	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: finalObservation("preliminary"),
	}.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)
}

// TestAFilteredPolicyCannotReachWhatAnUnfilteredOneWrote. The row is the same
// row and the compartment is the same compartment; only the filter differs, so
// this is the restriction acting and nothing else.
func TestAFilteredPolicyCannotReachWhatAnUnfilteredOneWrote(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: finalObservation("preliminary"),
	}.send(t, fhirRoutes(t))

	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	// The same database, now served under a policy narrowed to final readings.
	serveUnder(t, db, homeProject, filteredPolicy(t, mustStatusFilter(t, "final")))

	narrowed := fhirRoutes(t)

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("Observation", id),
	}.send(t, narrowed), http.StatusNotFound)

	// A non-clinical type is untouched, so the filter narrowed what it names and
	// not the server.
	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"Clinic"}`,
	}.send(t, narrowed), http.StatusCreated)
}

// TestAFilterOverASetAdmitsEveryValueItNamesOverHTTP, so membership survives
// the trip through a policy and a compiled predicate.
func TestAFilterOverASetAdmitsEveryValueItNamesOverHTTP(t *testing.T) {
	routes := servingFilteredFHIR(t, mustStatusFilter(t, "final", "amended"))

	for _, status := range []string{"final", "amended"} {
		assertStatus(t, call{
			method: http.MethodPost, path: fhir.BasePath + "/Observation",
			body: finalObservation(status),
		}.send(t, routes), http.StatusCreated)
	}

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: finalObservation("registered"),
	}.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)
}
