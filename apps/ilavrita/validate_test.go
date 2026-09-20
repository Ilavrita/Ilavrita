package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// The subject is provisioned first: $validate reports a reference that
	// leads nowhere, and this test is about something else.
	assertStatus(t, call{
		method: http.MethodPut, path: resourcePath("Patient", string(conformancePatient)),
		body: `{"resourceType":"Patient","id":"` + string(conformancePatient) + `"}`,
	}.send(t, routes), http.StatusCreated)

	clean := validating(t, routes, "Observation", valid("Observation", map[string]string{
		"status":  `"final"`,
		"subject": `{"reference":"Patient/` + string(conformancePatient) + `"}`,
	}))

	assertStatus(t, clean, http.StatusOK)

	if issues := outcomeOf(t, clean).Issue; len(issues) != 1 ||
		issues[0].Severity != fhir.SeverityInformation {
		t.Errorf("a clean resource validated as %+v", issues)
	}

	broken := validating(t, routes, "Observation", valid("Observation", map[string]string{
		"status": `null`, "performer": `[]`,
	}))

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
		valid("Observation", map[string]string{"status": `"final"`})), http.StatusForbidden)
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
		body: valid("Observation", map[string]string{
			"status":  `null`,
			"subject": `{"reference":"patient/` + string(conformancePatient) + `"}`,
		}),
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
		body: valid("Observation", map[string]string{
			"id": `"` + id + `"`, "performer": `[]`,
			"subject": `{"reference":"Patient/` + string(conformancePatient) + `"}`,
		}),
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

// TestAnInvariantR4StatesIsEnforcedOnTheWayIn.
//
// Cardinality cannot express "an Organization SHALL have a name or an
// identifier": both are optional on their own and the rule is about the
// resource. R4 writes those as FHIRPath, and a server that stored what they
// refuse is a server whose records other implementations will reject.
func TestAnInvariantR4StatesIsEnforcedOnTheWayIn(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for named, body := range map[string]string{
		"an Organization with neither a name nor an identifier": `{"resourceType":"Organization"}`,
		"a Consent with neither a policy nor a policy rule": `{"resourceType":"Consent",` +
			`"status":"active","scope":{"coding":[{"code":"patient-privacy"}]},` +
			`"category":[{"coding":[{"code":"acd"}]}]}`,
	} {
		answer := call{
			method: http.MethodPost,
			path:   fhir.BasePath + "/" + decodeType(t, body),
			body:   body,
		}.send(t, routes)

		if answer.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", named, answer.Code, answer.Body)
		}
	}

	// And one that satisfies the rule is stored, so this is a rule rather than
	// a refusal of the type.
	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"a named one"}`,
	}.send(t, routes), http.StatusCreated)
}

// TestARefusalNamesTheRuleR4NamesIt, because "org-1" is what the specification
// and every other implementation call this, and a client that reads it can look
// it up.
func TestARefusalNamesTheRuleR4NamesIt(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization"}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	if !strings.Contains(answer.Body.String(), "org-1") {
		t.Errorf("the refusal does not name the rule: %s", answer.Body)
	}
}

// TestBestPracticeIsNotARule. R4 marks some constraints as guidance with an
// extension it puts on exactly those — "a resource should have narrative" is
// true, and is not something to refuse a write over or to say about every
// resource that ever arrives.
func TestBestPracticeIsNotARule(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"a named one"}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	validated := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization/$validate",
		body: `{"resourceType":"Organization","name":"a named one"}`,
	}.send(t, routes)

	if strings.Contains(validated.Body.String(), "dom-6") {
		t.Errorf("a best-practice constraint was reported: %s", validated.Body)
	}
}

// decodeType reads which type a body names.
func decodeType(t *testing.T, body string) string {
	t.Helper()

	var held struct {
		ResourceType string `json:"resourceType"`
	}

	if err := json.Unmarshal([]byte(body), &held); err != nil {
		t.Fatalf("read the body: %v", err)
	}

	return held.ResourceType
}

// TestAProfileThisServerDoesNotHoldIsSaidSoRatherThanPassed.
//
// A resource declaring a profile is claiming to conform to it. A server that
// stored the claim and checked nothing would be handing every reader a line
// saying this was verified, when nobody looked.
func TestAProfileThisServerDoesNotHoldIsSaidSoRatherThanPassed(t *testing.T) {
	routes := definedServer(t)

	body := `{"resourceType":"Organization","name":"a named one",` +
		`"meta":{"profile":["http://example.test/StructureDefinition/Nowhere"]}}`

	// It is stored: naming a profile from somewhere else is not a client error.
	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization", body: body,
	}.send(t, routes), http.StatusCreated)

	// And $validate says the claim went unchecked.
	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization/$validate", body: body,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if !strings.Contains(answer.Body.String(), "does not hold") {
		t.Errorf("an unheld profile was not reported: %s", answer.Body)
	}
}

// TestAProfileIsCheckedAgainstTheTypeItConstrains.
//
// The other half: a claim this server can check, it checks. An Organization
// declaring the Patient definition is well formed as an Organization and is
// claiming something untrue — which only reading the profile can catch, so it
// is what says the profile was read at all.
func TestAProfileIsCheckedAgainstTheTypeItConstrains(t *testing.T) {
	routes := definedServer(t)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"a named one",` +
			`"meta":{"profile":["http://hl7.org/fhir/StructureDefinition/Patient"]}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	if !strings.Contains(answer.Body.String(), "constrains Patient") {
		t.Errorf("a profile for another type was accepted: %s", answer.Body)
	}

	// And declaring the definition of what it actually is, is fine.
	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"a named one",` +
			`"meta":{"profile":["http://hl7.org/fhir/StructureDefinition/Organization"]}}`,
	}.send(t, routes), http.StatusCreated)
}

// TestADanglingReferenceIsReportedAndNotRefused.
//
// R4 permits a reference to name something this server does not hold, and this
// build depends on it: a confined grant names a compartment before the Patient
// exists. So it is answered by $validate rather than enforced on the way in.
func TestADanglingReferenceIsReportedAndNotRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	body := `{"resourceType":"Observation","status":"final",` +
		`"code":{"text":"a reading"},` +
		`"subject":{"reference":"Patient/` + string(conformancePatient) + `"},` +
		`"performer":[{"reference":"Practitioner/nobody-here"}]}`

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation", body: body,
	}.send(t, routes), http.StatusCreated)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation/$validate", body: body,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if !strings.Contains(answer.Body.String(), "Practitioner/nobody-here") {
		t.Errorf("a dangling reference was not reported: %s", answer.Body)
	}

	// An absolute reference names another server's resource, which is not this
	// server's to follow and not something to report.
	elsewhere := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation/$validate",
		body: `{"resourceType":"Observation","status":"final","code":{"text":"x"},` +
			`"subject":{"reference":"https://example.test/fhir/Patient/p1"}}`,
	}.send(t, routes)

	if strings.Contains(elsewhere.Body.String(), "example.test") {
		t.Errorf("an absolute reference was reported: %s", elsewhere.Body)
	}
}
