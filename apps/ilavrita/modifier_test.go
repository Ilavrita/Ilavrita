package main

import (
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// TestAModifierNarrowsDifferentlyFromTheBareParameter.
//
// `name:exact=Ward` and `name=Ward` are different searches. Answering the
// second when the first was asked would come back looking answered, which is
// the failure every refusal in this package exists to avoid.
func TestAModifierNarrowsDifferentlyFromTheBareParameter(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	ids := seedOrganizations(t, routes, "Ward Clinic", "Riverside Ward")

	for named, held := range map[string]struct {
		query string
		want  []string
	}{
		// The bare parameter is a case-folded prefix.
		"a prefix":              {"name=ward", ids[:1]},
		"a prefix, capitalised": {"name=Ward", ids[:1]},

		// :exact is the value as written, whole.
		"exactly, as written":      {"name:exact=Ward%20Clinic", ids[:1]},
		"exactly, mis-capitalised": {"name:exact=ward%20clinic", nil},
		"exactly, a prefix only":   {"name:exact=Ward", nil},

		// :contains matches anywhere, which is what makes it the expensive one.
		"anywhere":                {"name:contains=ward", ids},
		"anywhere, in the middle": {"name:contains=iversi", ids[1:]},
	} {
		t.Run(named, func(t *testing.T) {
			found := matchedIDs(t, routes, held.query)

			if len(found) != len(held.want) {
				t.Fatalf("%s matched %v, want %v", held.query, found, held.want)
			}

			for _, id := range held.want {
				if !contains(found, id) {
					t.Errorf("%s did not match %s; got %v", held.query, id, found)
				}
			}
		})
	}
}

// TestMissingAsksWhetherTheElementIsThere, which no value can ask: a resource
// that never stated a gender and one that stated an unknown gender are
// different facts about a person.
func TestMissingAsksWhetherTheElementIsThere(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	withActive := namedOrganization(t, routes, "active", "states it")

	assertStatus(t, call{
		method: http.MethodPut, path: resourcePath("Organization", withActive),
		body: `{"resourceType":"Organization","id":"` + withActive + `","name":"states it",` +
			`"identifier":[{"system":"http://example.test/ids","value":"active"}],"active":true}`,
	}.send(t, routes), http.StatusOK)

	withoutActive := namedOrganization(t, routes, "quiet", "says nothing")

	if found := matchedIDs(t, routes, "active:missing=false"); len(found) != 1 || found[0] != withActive {
		t.Errorf("active:missing=false matched %v, want [%s]", found, withActive)
	}

	if found := matchedIDs(t, routes, "active:missing=true"); len(found) != 1 || found[0] != withoutActive {
		t.Errorf("active:missing=true matched %v, want [%s]", found, withoutActive)
	}

	// And it is a yes or a no, not a value to match.
	assertIssue(t, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?active:missing=maybe",
	}.send(t, routes), http.StatusBadRequest, fhir.CodeInvalid)
}

// TestNotExcludesTheWholeResourceRatherThanOneValue.
//
// A resource carrying two identifiers, one of them the excluded code, is one
// the client asked not to see. Negating the comparison instead would return it
// for the other row, which is the subtle way to get this wrong.
func TestNotExcludesTheWholeResourceRatherThanOneValue(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	kept := namedOrganization(t, routes, "keep", "the one wanted")

	both := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"the one excluded",` +
			`"identifier":[{"system":"http://example.test/ids","value":"drop"},` +
			`{"system":"http://example.test/ids","value":"keep-too"}]}`,
	}.send(t, routes)
	assertStatus(t, both, http.StatusCreated)

	found := matchedIDs(t, routes, "identifier:not=http://example.test/ids|drop")

	if len(found) != 1 || found[0] != kept {
		t.Errorf("identifier:not matched %v, want [%s]", found, kept)
	}
}

// TestAModifierThisBuildDoesNotApplyIsRefusedByName, because each of them needs
// something built first and none of them is the bare parameter.
func TestAModifierThisBuildDoesNotApplyIsRefusedByName(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, query := range []string{
		"identifier:above=x", "identifier:below=x",
		"identifier:in=http://example.test/vs", "identifier:not-in=http://example.test/vs",
		"identifier:text=x", "identifier:of-type=x",
		// A modifier that means nothing for the kind it was put on.
		"name:not=x", "identifier:contains=x",
	} {
		assertIssue(t, call{
			method: http.MethodGet, path: fhir.BasePath + "/Organization?" + query,
		}.send(t, routes), http.StatusBadRequest, fhir.CodeNotSupported)
	}
}

// contains reports membership, so a test can say what it means.
func contains(held []string, wanted string) bool {
	for _, one := range held {
		if one == wanted {
			return true
		}
	}

	return false
}
