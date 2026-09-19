package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// A Bundle that validates against R4 can still be wrong about what it says.
//
// The validator judges shape: that link is a list of relation and url, that
// search.mode is one of the codes, that total is an integer. None of that is
// the promise the Bundle makes. What is asserted here is the rest of the
// promise, and only the parts the suite does not already hold elsewhere —
// TestASearchPagesWithALinkThatWorks and TestOnlyTheOldestVersionIsACreate
// cover the paging link and the create derivation.

// TestEverySearchEntryIsAMatchAndSaysWhereItLives.
//
// `_include` is not implemented, so every entry in a searchset is something the
// query matched. An entry that did not say so would be indistinguishable from
// one included alongside a match, which is a resource the caller did not ask
// for — and `fullUrl` is how they read it back, so a relative one sends them to
// resolve it against a base nobody stated.
func TestEverySearchEntryIsAMatchAndSaysWhereItLives(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	seedOrganizations(t, routes, "Ward Clinic", "Riverside Clinic")

	bundle := searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization",
	})

	if len(bundle.Entry) == 0 {
		t.Fatal("the search matched nothing, so this proves nothing")
	}

	for index, entry := range bundle.Entry {
		if entry.Search == nil || entry.Search.Mode != "match" {
			t.Errorf("entry %d is not marked a match: %+v", index, entry.Search)
		}

		if !strings.HasPrefix(entry.FullURL, "http://") &&
			!strings.HasPrefix(entry.FullURL, "https://") {
			t.Errorf("entry %d names %q, which is not somewhere a client can read it",
				index, entry.FullURL)
		}
	}
}

// TestSearchPagesHoldNothingTwice.
//
// The existing paging test proves every resource is reached. This proves none
// is reached twice, which is the other half of a page boundary being a boundary:
// a duplicate is counted twice by whoever is reading, and a cursor that
// overlapped would look exactly like a correct page to them.
func TestSearchPagesHoldNothingTwice(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	written := seedOrganizations(t, routes, "One", "Two", "Three", "Four", "Five")

	seen := map[string]int{}
	path := fhir.BasePath + "/Organization?_count=2"

	for pages := 0; pages <= len(written)+1; pages++ {
		bundle := searchedBundle(t, routes, call{method: http.MethodGet, path: path})

		for _, id := range matched(t, bundle) {
			seen[id]++
		}

		next := linkNamed(bundle, "next")
		if next == "" {
			break
		}

		path = pathOf(t, next)
	}

	for _, id := range written {
		if seen[id] != 1 {
			t.Errorf("%s was returned %d time(s) across the pages, want once", id, seen[id])
		}
	}
}

// TestAHistoryNamesTheInteractionThatWroteEachVersion.
//
// Nothing stores which interaction produced a version, so it is derived from
// where the version sits. The create half of that is held by
// TestOnlyTheOldestVersionIsACreate; this holds the rest, because a client
// reading a history tells a correction from a retraction by exactly this.
func TestAHistoryNamesTheInteractionThatWroteEachVersion(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")
	assertUpdate(t, routes, "Organization", id)
	assertDelete(t, routes, "Organization", id)

	bundle := historyPage(t, routes, resourcePath("Organization", id)+"/_history")

	// The rule rather than a count: newest first, the deletion is the delete,
	// the oldest is the create, and everything between replaced the one before
	// it. Stating it this way survives a helper writing one more version.
	if len(bundle.Entry) < 3 {
		t.Fatalf("the history holds %d version(s), too few to say anything", len(bundle.Entry))
	}

	last := len(bundle.Entry) - 1

	for index, entry := range bundle.Entry {
		if entry.Request == nil {
			t.Errorf("version %d says nothing about how it was written", index)

			continue
		}

		want := fhir.VerbPut

		switch index {
		case 0:
			want = fhir.VerbDelete
		case last:
			want = fhir.VerbPost
		}

		if entry.Request.Method != want {
			t.Errorf("version %d of %d reads as %s, want %s",
				index, last, entry.Request.Method, want)
		}
	}

	// A tombstone is the absence of content, not content saying it is absent.
	if len(bundle.Entry[0].Resource) != 0 {
		t.Errorf("the deleted version carries a resource: %s", bundle.Entry[0].Resource)
	}
}
