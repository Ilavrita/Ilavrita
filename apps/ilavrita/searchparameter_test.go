package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// definedParameter posts one SearchParameter and returns its id.
func definedParameter(t *testing.T, routes http.Handler, code, kind, expression string) string {
	t.Helper()

	body := `{"resourceType":"SearchParameter","url":"http://example.com/sp/` + code + `",` +
		`"name":"` + code + `","status":"active","description":"defined by a test",` +
		`"code":"` + code + `","type":"` + kind + `","base":["Organization"],` +
		`"expression":"` + expression + `"}`

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/SearchParameter", body: body,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// organizationWith writes one Organization stating the elements a test needs.
func organizationWith(t *testing.T, routes http.Handler, stating string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost,
		path:   fhir.BasePath + "/Organization",
		body:   `{"resourceType":"Organization","name":"a named one",` + stating + `}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// matchedIDs returns the ids one search found.
func matchedIDs(t *testing.T, routes http.Handler, query string) []string {
	t.Helper()

	answer := call{method: http.MethodGet, path: fhir.BasePath + "/Organization?" + query}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var bundle struct {
		Entry []struct {
			Resource struct {
				ID string `json:"id"`
			} `json:"resource"`
		} `json:"entry"`
	}

	if err := json.Unmarshal(answer.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode the bundle: %v (%s)", err, answer.Body)
	}

	found := make([]string, 0, len(bundle.Entry))
	for _, entry := range bundle.Entry {
		found = append(found, entry.Resource.ID)
	}

	return found
}

// TestAProjectSearchesByAParameterItDefined, which is the whole point: a
// SearchParameter stored in a Project changes what that Project can ask for,
// with no restart and no operator step.
func TestAProjectSearchesByAParameterItDefined(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	// Before it is defined, the code is refused rather than ignored: a query
	// naming a parameter nobody implements must not come back looking answered.
	assertIssue(t, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?phone=555-0100",
	}.send(t, routes), http.StatusBadRequest, fhir.CodeNotSupported)

	definedParameter(t, routes, "phone", "token", "Organization.telecom")

	wanted := organizationWith(t, routes,
		`"telecom":[{"system":"phone","value":"555-0100"}]`)
	other := organizationWith(t, routes,
		`"telecom":[{"system":"phone","value":"555-0199"}]`)

	found := matchedIDs(t, routes, "phone=555-0100")
	if len(found) != 1 || found[0] != wanted {
		t.Fatalf("searching by the new parameter found %v, want [%s]", found, wanted)
	}

	// And it is a real restriction, not a parameter that matches everything.
	if elsewhere := matchedIDs(t, routes, "phone=555-0199"); len(elsewhere) != 1 || elsewhere[0] != other {
		t.Errorf("the second value found %v, want [%s]", elsewhere, other)
	}

	// The system half is read from the same element as the code.
	if qualified := matchedIDs(t, routes, "phone=phone|555-0100"); len(qualified) != 1 {
		t.Errorf("a system-qualified token found %v", qualified)
	}
}

// TestAParameterRemovedStopsBeingAnswered. What a definition indexed goes with
// it: a row left behind is a match for a restriction that no longer exists, and
// the code itself must stop being accepted rather than quietly matching nothing.
func TestAParameterRemovedStopsBeingAnswered(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := definedParameter(t, routes, "phone", "token", "Organization.telecom")
	organizationWith(t, routes, `"telecom":[{"system":"phone","value":"555-0100"}]`)

	if found := matchedIDs(t, routes, "phone=555-0100"); len(found) != 1 {
		t.Fatalf("the parameter did not work before it was removed: %v", found)
	}

	answer := call{
		method: http.MethodDelete, path: resourcePath("SearchParameter", id),
	}.send(t, routes)
	assertStatus(t, answer, http.StatusNoContent)

	assertIssue(t, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?phone=555-0100",
	}.send(t, routes), http.StatusBadRequest, fhir.CodeNotSupported)
}

// TestASearchParameterThisBuildCannotApplyIsRefused. Storing one that states
// where it reads from and then indexing nothing would answer the claim with an
// empty page, and an empty page is what a correct search looks like.
func TestASearchParameterThisBuildCannotApplyIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for named, body := range map[string]string{
		"a kind this build does not index": `"type":"quantity","expression":"Organization.telecom"`,
		"an expression it cannot walk":     `"type":"string","expression":"Organization.name.where(x='y')"`,
		"an element the type lacks":        `"type":"string","expression":"Organization.nonesuch"`,
		"a code already answered":          `"type":"token","expression":"Organization.telecom"`,
	} {
		code := "custom"
		if named == "a code already answered" {
			code = "_id"
		}

		answer := call{
			method: http.MethodPost,
			path:   fhir.BasePath + "/SearchParameter",
			body: `{"resourceType":"SearchParameter","url":"http://example.com/sp/x",` +
				`"name":"x","status":"active","description":"d","code":"` + code + `",` +
				`"base":["Organization"],` + body + `}`,
		}.send(t, routes)

		assertIssue(t, answer, http.StatusBadRequest, fhir.CodeNotSupported)

		if _ = named; t.Failed() {
			t.Logf("while checking %s", named)
		}
	}
}
