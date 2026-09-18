package main

import (
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// observation builds one body naming whatever references a case is about.
func observation(extra string) string {
	body := `{"resourceType":"Observation","subject":{"reference":"Patient/` +
		string(conformancePatient) + `"}`

	if extra != "" {
		body += "," + extra
	}

	return body + `}`
}

// postObservation creates one against the confined conformance policy.
func postObservation(t *testing.T, routes http.Handler, body string) int {
	t.Helper()

	return call{method: http.MethodPost, path: fhir.BasePath + "/Observation", body: body}.send(t, routes).Code
}

// TestAConfinedWriteMayNameTheRestOfTheClinicalRecord. A Grant confined to one
// patient speaks to the Patient dimension and to nothing else: the encounter a
// reading happened in and the clinician who took it are incidental, and refusing
// them would refuse nearly every real observation.
func TestAConfinedWriteMayNameTheRestOfTheClinicalRecord(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	cases := map[string]string{
		"the subject alone":  "",
		"a performer":        `"performer":[{"reference":"Practitioner/prac-1"}]`,
		"an encounter":       `"encounter":{"reference":"Encounter/enc-1"}`,
		"a device performer": `"performer":[{"reference":"Device/dev-1"}]`,
		"several at once": `"encounter":{"reference":"Encounter/enc-1"},` +
			`"performer":[{"reference":"Practitioner/prac-1"},{"reference":"Device/dev-1"}]`,
	}

	for name, extra := range cases {
		if got := postObservation(t, routes, observation(extra)); got != http.StatusCreated {
			t.Errorf("%s: status %d, want 201", name, got)
		}
	}
}

// TestAConfinedWriteReachesNoOtherPatient. The Patient dimension is the one the
// Grant speaks to, so every Patient compartment the resource lands in must be
// the one it names.
func TestAConfinedWriteReachesNoOtherPatient(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	refused := map[string]string{
		"another patient as subject": `{"resourceType":"Observation",` +
			`"subject":{"reference":"Patient/someone-else"}}`,
		"another patient alongside its own": observation(
			`"performer":[{"reference":"Patient/someone-else"}]`),
	}

	for name, body := range refused {
		if got := postObservation(t, routes, body); got != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
}

// TestAConfinedWriteLandingNowhereIsRefused. A resource reaching no compartment
// its author holds is one they could not read back, so writing it is refused
// rather than leaving a row nobody can address.
func TestAConfinedWriteLandingNowhereIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	refused := map[string]string{
		"no subject at all": `{"resourceType":"Observation"}`,
		"only an incidental reference": `{"resourceType":"Observation",` +
			`"performer":[{"reference":"Practitioner/prac-1"}]}`,
		"a subject that places nothing": `{"resourceType":"Observation",` +
			`"subject":{"reference":"Group/grp-1"}}`,
	}

	for name, body := range refused {
		if got := postObservation(t, routes, body); got != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
}

// TestAConfinedWriteIsReadableAfterwards closes the loop the compartment
// projection exists for: a create that lands in the caller's compartment must be
// one the same caller can then read, or the write was into a hole.
func TestAConfinedWriteIsReadableAfterwards(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: observation(`"encounter":{"reference":"Encounter/enc-1"}`),
	}.send(t, routes)

	assertStatus(t, created, http.StatusCreated)

	read := call{
		method: http.MethodGet,
		path:   resourcePath("Observation", resourceID(t, created)),
	}.send(t, routes)

	assertStatus(t, read, http.StatusOK)
}
