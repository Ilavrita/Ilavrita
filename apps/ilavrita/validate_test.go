package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// validating asks what this server would make of a resource.
func validating(t *testing.T, routes http.Handler, resourceType, body string) *httptest.ResponseRecorder {
	t.Helper()

	return call{
		method: http.MethodPost, path: fhir.BasePath + "/" + resourceType + "/$validate",
		body: body,
	}.send(t, routes)
}

// outcomeOf reads the OperationOutcome one answer carries.
func outcomeOf(t *testing.T, answer *httptest.ResponseRecorder) fhir.OperationOutcome {
	t.Helper()

	var held fhir.OperationOutcome
	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the outcome: %v (%s)", err, answer.Body)
	}

	if held.ResourceType != "OperationOutcome" {
		t.Fatalf("the answer is a %s", held.ResourceType)
	}

	return held
}

// TestValidateAnswersWhatItFoundAndStoresNothing. R4 answers 200 whichever way
// it went: the operation was performed, and what it found is the outcome.
func TestValidateAnswersWhatItFoundAndStoresNothing(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	clean := validating(t, routes, "Observation",
		`{"resourceType":"Observation","status":"final",`+
			`"subject":{"reference":"Patient/`+string(conformancePatient)+`"}}`)

	assertStatus(t, clean, http.StatusOK)

	if issues := outcomeOf(t, clean).Issue; len(issues) != 1 ||
		issues[0].Severity != fhir.SeverityInformation {
		t.Errorf("a clean resource validated as %+v", issues)
	}

	broken := validating(t, routes, "Observation",
		`{"resourceType":"Observation","status":null,"performer":[]}`)

	// Still 200: the operation was performed, and the issues are the answer.
	assertStatus(t, broken, http.StatusOK)

	issues := outcomeOf(t, broken).Issue
	if len(issues) != 2 {
		t.Fatalf("a broken resource validated as %+v", issues)
	}

	for _, issue := range issues {
		if issue.Severity != fhir.SeverityError || len(issue.Expression) != 1 {
			t.Errorf("an issue reads %+v", issue)
		}
	}

	// And nothing was stored by asking.
	found := matched(t, searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Observation?status=final",
	}))

	if len(found) != 0 {
		t.Errorf("validating stored %v", found)
	}
}

// TestValidateIsAuthorizedAsAWrite. Nothing is stored, but validating is what a
// client does before writing, and an endpoint that did this work for anybody
// who asked would be one that does work for anybody who asks.
func TestValidateIsAuthorizedAsAWrite(t *testing.T) {
	routes := servingFHIR(t, readActions)

	assertStatus(t, validating(t, routes, "Observation",
		`{"resourceType":"Observation","status":"final"}`), http.StatusForbidden)
}

// TestValidateRefusesATypeThisBuildDoesNotServe, the same as every other route
// under a name nobody declared.
func TestValidateRefusesATypeThisBuildDoesNotServe(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertStatus(t, validating(t, routes, "Appointment",
		`{"resourceType":"Appointment","status":"booked"}`), http.StatusNotFound)
}

// TestAResourceThisServerWillNotStoreIsRefusedOnTheWayIn, with the issues that
// say which elements — the whole point of validating is knowing where.
func TestAResourceThisServerWillNotStoreIsRefusedOnTheWayIn(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: `{"resourceType":"Observation","status":null,` +
			`"subject":{"reference":"patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	issues := outcomeOf(t, answer).Issue
	if len(issues) != 2 {
		t.Fatalf("the refusal carries %+v", issues)
	}

	named := map[string]bool{}
	for _, issue := range issues {
		for _, where := range issue.Expression {
			named[where] = true
		}
	}

	for _, where := range []string{"Observation.status", "Observation.subject.reference"} {
		if !named[where] {
			t.Errorf("the refusal does not name %s: %+v", issues, where)
		}
	}
}

// TestAnUpdateCannotStoreWhatACreateCouldNot. A rule checked only on the way in
// is one PUT away from being no rule at all.
func TestAnUpdateCannotStoreWhatACreateCouldNot(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Observation")

	assertStatus(t, call{
		method: http.MethodPut, path: resourcePath("Observation", id),
		body: `{"resourceType":"Observation","id":"` + id + `","performer":[],` +
			`"subject":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes), http.StatusBadRequest)
}

// TestTheStatementDeclaresTheOperationItServes. A CapabilityStatement is
// generated from what was registered, so an operation served and undeclared
// would be one no client discovers — and one declared and unserved would send
// them at a 501.
func TestTheStatementDeclaresTheOperationItServes(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{method: http.MethodGet, path: fhir.BasePath + "/metadata"}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var statement fhir.CapabilityStatement
	if err := json.Unmarshal(answer.Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode the statement: %v", err)
	}

	if len(statement.Rest) != 1 || len(statement.Rest[0].Resource) == 0 {
		t.Fatalf("the statement declares no resource")
	}

	for _, resource := range statement.Rest[0].Resource {
		named := false

		for _, operation := range resource.Operation {
			if operation.Name == validateOperationName && operation.Definition != "" {
				named = true
			}
		}

		if !named {
			t.Fatalf("%s declares %v, and $validate is served on it",
				resource.Type, resource.Operation)
		}

		// And it is served: the same body through the route the statement
		// pointed at.
		assertStatus(t, validating(t, routes, resource.Type,
			`{"resourceType":"`+resource.Type+`"}`), http.StatusOK)
	}
}
