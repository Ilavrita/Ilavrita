package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// withVersions writes one resource and updates it until it holds as many
// versions as the caller asked for.
func withVersions(t *testing.T, routes http.Handler, held int) string {
	t.Helper()

	id := assertCreate(t, routes, "Organization")

	for range held - 1 {
		assertStatus(t, call{
			method: http.MethodPut, path: resourcePath("Organization", id),
			body: replacement("Organization", id),
		}.send(t, routes), http.StatusOK)
	}

	return id
}

// historyPage reads one page of a history.
func historyPage(t *testing.T, routes http.Handler, path string) fhir.Bundle {
	t.Helper()

	answer := call{method: http.MethodGet, path: path}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var bundle fhir.Bundle
	if err := json.Unmarshal(answer.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode the history bundle: %v", err)
	}

	if bundle.ResourceType != "Bundle" || bundle.Type != fhir.BundleHistory {
		t.Fatalf("answered %s/%s", bundle.ResourceType, bundle.Type)
	}

	return bundle
}

// linked returns one link's href, and whether the bundle carries it.
func linked(bundle fhir.Bundle, relation string) (string, bool) {
	for _, held := range bundle.Link {
		if held.Relation == relation {
			return held.URL, true
		}
	}

	return "", false
}

// pathOf turns an absolute link into the path a test can request.
func pathOf(t *testing.T, href string) string {
	t.Helper()

	parsed, err := url.Parse(href)
	if err != nil {
		t.Fatalf("read the link %q: %v", href, err)
	}

	if parsed.RawQuery == "" {
		return parsed.Path
	}

	return parsed.Path + "?" + parsed.RawQuery
}

// versionsIn lists the versions a bundle's entries name, in the order they came.
func versionsIn(t *testing.T, bundle fhir.Bundle) []string {
	t.Helper()

	held := make([]string, 0, len(bundle.Entry))

	for _, entry := range bundle.Entry {
		if entry.Response == nil {
			t.Fatal("a history entry records no response")
		}

		held = append(held, strings.TrimSuffix(strings.TrimPrefix(entry.Response.ETag, `W/"`), `"`))
	}

	return held
}

// TestAHistoryIsPagedRatherThanReturnedWhole. A resource written to for years
// has a history nobody asked to receive in one response, and this server holds
// one pooled connection per process — so an unbounded read is every other
// request in that process waiting behind it.
func TestAHistoryIsPagedRatherThanReturnedWhole(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	const held = 7

	id := withVersions(t, routes, held)
	path := resourcePath("Organization", id) + "/_history?_count=3"

	var (
		seen  []string
		pages int
	)

	for path != "" {
		bundle := historyPage(t, routes, path)
		pages++

		if len(bundle.Entry) > 3 {
			t.Fatalf("a page asked to hold 3 holds %d", len(bundle.Entry))
		}

		seen = append(seen, versionsIn(t, bundle)...)

		next, more := linked(bundle, "next")
		if !more {
			break
		}

		path = pathOf(t, next)

		if pages > held {
			t.Fatal("the pages do not end")
		}
	}

	if pages != 3 {
		t.Errorf("%d versions came back in %d pages of 3", len(seen), pages)
	}

	// Every version, newest first, each exactly once.
	if len(seen) != held {
		t.Fatalf("read %d versions of %d: %v", len(seen), held, seen)
	}

	for index, version := range seen {
		if want := strconv.Itoa(held - index); version != want {
			t.Errorf("version %d of the page sequence is %q, want %q", index, version, want)
		}
	}
}

// TestAHistoryThatFitsCarriesItsTotal, and one that does not carries none. A
// client can handle a missing total and cannot handle a wrong one.
func TestAHistoryThatFitsCarriesItsTotal(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := withVersions(t, routes, 5)

	whole := historyPage(t, routes, resourcePath("Organization", id)+"/_history")
	if whole.Total == nil || *whole.Total != 5 {
		t.Errorf("a history that fits reports total %v", whole.Total)
	}

	if _, more := linked(whole, "next"); more {
		t.Error("a history that fits offers a next page")
	}

	part := historyPage(t, routes, resourcePath("Organization", id)+"/_history?_count=2")
	if part.Total != nil {
		t.Errorf("a partial page reports total %d, which it did not count", *part.Total)
	}
}

// TestOnlyTheOldestVersionIsACreate. Which interaction produced a version is
// read from where it sits in the history, so a page with more to come must not
// call its last entry the create — the one that is sits on a page the client
// has not read yet.
func TestOnlyTheOldestVersionIsACreate(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := withVersions(t, routes, 5)

	first := historyPage(t, routes, resourcePath("Organization", id)+"/_history?_count=2")

	for _, entry := range first.Entry {
		if entry.Request.Method == fhir.VerbPost {
			t.Errorf("a page with more to come calls an entry a create: %+v", entry.Request)
		}
	}

	// Walk to the last page, which is the one that holds it.
	bundle := first

	for {
		next, more := linked(bundle, "next")
		if !more {
			break
		}

		bundle = historyPage(t, routes, pathOf(t, next))
	}

	last := bundle.Entry[len(bundle.Entry)-1]
	if last.Request.Method != fhir.VerbPost {
		t.Errorf("the oldest version is recorded as %s", last.Request.Method)
	}
}

// TestAPageThisServerDoesNotAnswerIsRefused, rather than rounded down: a caller
// who asked for a thousand and silently got two hundred cannot tell a short page
// from the end of the history.
func TestAPageThisServerDoesNotAnswerIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := withVersions(t, routes, 2)

	for _, asked := range []string{
		"_count=0", "_count=-1", "_count=1000", "_count=many",
		"_cursor=not-a-version", "_cursor=0", "_cursor=-3",
	} {
		answer := call{
			method: http.MethodGet,
			path:   resourcePath("Organization", id) + "/_history?" + asked,
		}.send(t, routes)

		if answer.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d", asked, answer.Code)
		}
	}
}

// TestAHistoryPageIsBoundedEvenWhenNobodyAsks. The default is what protects an
// install whose clients never send _count at all.
func TestAHistoryPageIsBoundedEvenWhenNobodyAsks(t *testing.T) {
	if storage.DefaultVersions > storage.MaxVersions || storage.DefaultVersions < 1 {
		t.Fatalf("the default page is %d", storage.DefaultVersions)
	}

	if window := storage.DefaultWindow(); window.Count != storage.DefaultVersions {
		t.Errorf("a request naming no count reads %d versions", window.Count)
	}

	// And zero is a caller asking for nothing, which is a different request and
	// is refused rather than quietly turned into the default.
	if _, err := storage.NewVersionWindow(0, ""); err == nil {
		t.Error("a count of zero was accepted")
	}
}
