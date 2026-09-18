package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// searchedBundle runs one search and decodes the Bundle it answered.
func searchedBundle(t *testing.T, routes http.Handler, sent call) fhir.Bundle {
	t.Helper()

	answer := sent.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var bundle fhir.Bundle
	if err := json.Unmarshal(answer.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}

	if bundle.ResourceType != "Bundle" || bundle.Type != fhir.BundleSearchset {
		t.Fatalf("answered %s/%s, want a searchset Bundle", bundle.ResourceType, bundle.Type)
	}

	return bundle
}

// matched lists the ids one Bundle carries.
func matched(t *testing.T, bundle fhir.Bundle) []string {
	t.Helper()

	ids := make([]string, 0, len(bundle.Entry))

	for _, entry := range bundle.Entry {
		if entry.Search == nil || entry.Search.Mode != "match" {
			t.Errorf("an entry is not marked as a match: %+v", entry.Search)
		}

		var held struct {
			ID string `json:"id"`
		}

		if err := json.Unmarshal(entry.Resource, &held); err != nil {
			t.Fatalf("decode an entry: %v", err)
		}

		ids = append(ids, held.ID)
	}

	return ids
}

func linkNamed(bundle fhir.Bundle, relation string) string {
	for _, link := range bundle.Link {
		if link.Relation == relation {
			return link.URL
		}
	}

	return ""
}

// seedOrganizations creates one Organization per name and returns their ids.
func seedOrganizations(t *testing.T, routes http.Handler, names ...string) []string {
	t.Helper()

	ids := make([]string, 0, len(names))

	for _, name := range names {
		answer := call{
			method: http.MethodPost, path: fhir.BasePath + "/Organization",
			body: `{"resourceType":"Organization","name":"` + name + `","active":true}`,
		}.send(t, routes)

		assertStatus(t, answer, http.StatusCreated)
		ids = append(ids, resourceID(t, answer))
	}

	return ids
}

// TestASearchAnswersASearchsetBundle.
func TestASearchAnswersASearchsetBundle(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	ids := seedOrganizations(t, routes, "Ward Clinic", "Riverside Clinic")

	bundle := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?name=ward",
	})

	if got := matched(t, bundle); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("name=ward matched %v, want %v", got, ids[:1])
	}

	if self := linkNamed(bundle, "self"); !strings.Contains(self, "name=ward") {
		t.Errorf("the self link is %q, want the search that produced the page", self)
	}
}

// TestAPostedSearchResolvesIdenticallyToTheSameQueryString. R4 offers the POST
// so a query too long or too sensitive for a URL is not one a client has to
// shorten, and the two are the same interaction (SRC-1).
func TestAPostedSearchResolvesIdenticallyToTheSameQueryString(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	seedOrganizations(t, routes, "Ward Clinic", "Riverside Clinic")

	const query = "name=ward&_total=accurate"

	fromURL := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?" + query,
	})

	fromBody := searchedBundle(t, routes, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization/_search",
		body: query, contentType: formContentType,
	})

	if got, want := matched(t, fromBody), matched(t, fromURL); len(got) != len(want) {
		t.Fatalf("the posted search matched %v and the query string %v", got, want)
	}

	if fromBody.Total == nil || fromURL.Total == nil || *fromBody.Total != *fromURL.Total {
		t.Errorf("the two forms answered different totals")
	}
}

// TestAPostedSearchIsFormEncoded, because R4 names one media type for it and a
// route that accepted a resource as a form would accept a resource as a form.
func TestAPostedSearchIsFormEncoded(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization/_search",
		body: `{"name":"ward"}`, contentType: fhir.ContentType,
	}.send(t, routes), http.StatusUnsupportedMediaType, fhir.CodeNotSupported)
}

// TestAParameterThisServerDoesNotImplementIsRefusedOverHTTP. A search that
// silently dropped it would return more than it was asked for, and the caller
// could not tell (SRC-4).
func TestAParameterThisServerDoesNotImplementIsRefusedOverHTTP(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	seedOrganizations(t, routes, "Ward Clinic")

	for _, query := range []string{
		"?colour=blue", "?name:exact=Ward", "?_sort=name", "?_include=Organization:endpoint",
		"?_count=0", "?_count=9999",
	} {
		answer := call{method: http.MethodGet, path: fhir.BasePath + "/Organization" + query}.send(t, routes)
		if answer.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", query, answer.Code)
		}
	}
}

// TestASearchPagesWithALinkThatWorks. A next link is the only thing a client
// has to page with, so following it has to actually produce the next page.
func TestASearchPagesWithALinkThatWorks(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	ids := seedOrganizations(t, routes, "One Clinic", "Two Clinic", "Three Clinic")

	first := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?active=true&_count=2",
	})

	if len(first.Entry) != 2 {
		t.Fatalf("the first page carries %d entries, want 2", len(first.Entry))
	}

	next := linkNamed(first, "next")
	if next == "" {
		t.Fatal("no next link on a page that has one following it")
	}

	parsed, err := url.Parse(next)
	if err != nil {
		t.Fatalf("parse the next link: %v", err)
	}

	second := searchedBundle(t, routes, call{
		method: http.MethodGet, path: parsed.Path + "?" + parsed.RawQuery,
	})

	if len(second.Entry) != 1 {
		t.Fatalf("the second page carries %d entries, want the one remaining", len(second.Entry))
	}

	if linkNamed(second, "next") != "" {
		t.Error("the last page offered a next link")
	}

	seen := append(matched(t, first), matched(t, second)...)
	for _, id := range ids {
		if !strings.Contains(strings.Join(seen, " "), id) {
			t.Errorf("%s was not returned across the pages", id)
		}
	}
}

// TestATotalIsAbsentUnlessItWasAskedFor, because an absent total and a wrong
// one are very different promises (SRC-2).
func TestATotalIsAbsentUnlessItWasAskedFor(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	seedOrganizations(t, routes, "One Clinic", "Two Clinic", "Three Clinic")

	quiet := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization?active=true&_count=1",
	})

	if quiet.Total != nil {
		t.Errorf("a page nobody asked a total of carries %d", *quiet.Total)
	}

	counted := searchedBundle(t, routes, call{
		method: http.MethodGet,
		path:   fhir.BasePath + "/Organization?active=true&_count=1&_total=accurate",
	})

	if counted.Total == nil || *counted.Total != 3 {
		t.Errorf("an accurate total is %v, want every match rather than the page", counted.Total)
	}
}

// TestASearchIsBoundedByTheCallersScopeOverHTTP. The conformance policy
// confines every clinical type to one patient, so a search of one returns only
// that patient's — the same Scope a by-key read carries (SRC-3).
func TestASearchIsBoundedByTheCallersScopeOverHTTP(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	mine := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: `{"resourceType":"Observation","status":"final","subject":{"reference":"Patient/` +
			string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, mine, http.StatusCreated)

	// Another patient's reading cannot even be written under this policy, which
	// is what makes the search below a search of what is reachable.
	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: `{"resourceType":"Observation","status":"final","subject":{"reference":"Patient/someone-else"}}`,
	}.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)

	bundle := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Observation?status=final",
	})

	if got := matched(t, bundle); len(got) != 1 || got[0] != resourceID(t, mine) {
		t.Errorf("a confined search matched %v", got)
	}
}

// TestSearchingAnUndeclaredTypeIsNotAnEndpoint, answered before anything is
// authorized, as every other interaction on one is.
func TestSearchingAnUndeclaredTypeIsNotAnEndpoint(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertIssue(t, call{
		method: http.MethodGet, path: fhir.BasePath + "/NotAResource?status=final",
	}.send(t, routes), http.StatusNotFound, fhir.CodeNotFound)

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/NotAResource/_search",
		body: "status=final", contentType: formContentType,
	}.send(t, routes), http.StatusNotFound, fhir.CodeNotFound)
}

// TestAWholeSystemInteractionIsNotAResourceType. /_history and /_search name
// interactions this build does not implement, not types nobody declared.
func TestAWholeSystemInteractionIsNotAResourceType(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, path := range []string{fhir.BasePath + "/_history", fhir.BasePath + "/_search"} {
		assertIssue(t, call{method: http.MethodGet, path: path}.send(t, routes),
			http.StatusNotImplemented, fhir.CodeNotSupported)
	}
}
