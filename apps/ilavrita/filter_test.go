package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// finalObservation is one reading in whatever state the case is about, always
// subject to the patient the conformance policy is confined to.
func finalObservation(status string) string {
	return valid("Observation", map[string]string{
		"status":  `"` + status + `"`,
		"subject": `{"reference":"Patient/` + string(conformancePatient) + `"}`,
	})
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

// richObservation carries members a projection can withhold, subject to the
// patient the conformance policy is confined to.
func richObservation() string {
	return valid("Observation", map[string]string{
		"status":        `"final"`,
		"note":          `[{"text":"private"}]`,
		"valueQuantity": `{"value":7}`,
		"subject":       `{"reference":"Patient/` + string(conformancePatient) + `"}`,
	})
}

// projectedPolicy is the conformance policy with every clinical rule returning
// only the elements named, which is the shape a research reader holds when the
// narrative notes are not theirs to see.
func projectedPolicy(t *testing.T, elements ...string) authz.AccessPolicy {
	t.Helper()

	projection, err := storage.NewProjection(elements...)
	if err != nil {
		t.Fatalf("build the projection: %v", err)
	}

	var rules []authz.Rule

	for _, name := range fhir.ServedResourceTypes() {
		for _, action := range everyAction {
			rule := widestRule(t, storage.ResourceType(name), action)

			if authz.CarriesClinicalData(storage.ResourceType(name)) {
				rule = rule.Returning(projection)
			}

			rules = append(rules, rule)
		}
	}

	policy, err := authz.NewAccessPolicy(authz.PolicyConfig{
		Project: homeProject, ID: conformancePolicyID, Rules: rules,
	})
	if err != nil {
		t.Fatalf("author the projected policy: %v", err)
	}

	return policy
}

// memberNames reads which members a response body actually carries.
func memberNames(t *testing.T, body []byte) map[string]bool {
	t.Helper()

	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("decode the response: %v", err)
	}

	present := map[string]bool{}
	for name := range members {
		present[name] = true
	}

	return present
}

// TestAPolicyProjectionWithholdsElementsOverHTTP is the restriction end to end:
// a rule naming what it returns, compiled to a Grant, applied to the row, and
// rendered. An element the rule does not name must be absent from the response
// rather than empty, so a reader cannot tell a withheld value from one nobody
// recorded.
func TestAPolicyProjectionWithholdsElementsOverHTTP(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation", body: richObservation(),
	}.send(t, fhirRoutes(t))

	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	// The whole resource was stored, so what the next reader misses is withheld
	// rather than never written.
	whole := memberNames(t, created.Body.Bytes())
	for _, member := range []string{"note", "valueQuantity", "subject"} {
		if !whole[member] {
			t.Fatalf("%s was not stored, so this proves nothing about withholding it", member)
		}
	}

	serveUnder(t, db, homeProject, projectedPolicy(t, "status", "subject"))

	read := call{method: http.MethodGet, path: resourcePath("Observation", id)}.send(t, fhirRoutes(t))
	assertStatus(t, read, http.StatusOK)

	narrowed := memberNames(t, read.Body.Bytes())

	for _, kept := range []string{"resourceType", "id", "status", "subject"} {
		if !narrowed[kept] {
			t.Errorf("%s was withheld but the projection returns it", kept)
		}
	}

	for _, withheld := range []string{"note", "valueQuantity"} {
		if narrowed[withheld] {
			t.Errorf("%s reached a reader whose policy does not name it", withheld)
		}
	}
}

// TestAProjectedReaderCannotReplaceTheWholeResource. A PUT replaces content
// wholesale, so a reader shown part of a resource would send back what they saw
// and silently drop the rest — data loss produced by an authorization rule
// rather than by anyone's intent.
func TestAProjectedReaderCannotReplaceTheWholeResource(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation", body: richObservation(),
	}.send(t, fhirRoutes(t))

	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	serveUnder(t, db, homeProject, projectedPolicy(t, "status", "subject"))

	routes := fhirRoutes(t)

	read := call{method: http.MethodGet, path: resourcePath("Observation", id)}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	// Sending back exactly what was read is the read-modify-write a partial
	// reader would perform, and it is refused rather than answered.
	assertIssue(t, call{
		method: http.MethodPut, path: resourcePath("Observation", id),
		body: read.Body.String(),
	}.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)

	// Nothing was lost.
	serveProject(t, db, homeProject, everyAction)

	after := memberNames(t, call{
		method: http.MethodGet, path: resourcePath("Observation", id),
	}.send(t, fhirRoutes(t)).Body.Bytes())

	for _, kept := range []string{"note", "valueQuantity"} {
		if !after[kept] {
			t.Errorf("%s was lost by a write that should have been refused", kept)
		}
	}
}
