package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// TestAReadTellsACallerHoldingTheCurrentVersionSoRatherThanResending.
//
// A client polling a resource it has read before is asking whether it changed,
// and answering the whole resource says yes however the truth stands.
func TestAReadTellsACallerHoldingTheCurrentVersionSoRatherThanResending(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")
	path := resourcePath("Organization", id)

	read := call{method: http.MethodGet, path: path}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	tag := read.Header().Get(etagField)
	if tag == "" {
		t.Fatal("a read carried no ETag to be conditional on")
	}

	for named, header := range map[string]string{
		"the version it holds":   tag,
		"the version unweakened": strings.TrimPrefix(tag, weakPrefix),
		"any version at all":     "*",
		"one of several":         `W/"99", ` + tag,
	} {
		answer := call{method: http.MethodGet, path: path, ifNoneMatch: header}.send(t, routes)

		if answer.Code != http.StatusNotModified {
			t.Errorf("%s answered %d, want 304", named, answer.Code)
		}

		if answer.Body.Len() != 0 {
			t.Errorf("%s carried a body: %s", named, answer.Body)
		}

		// The validators are still answered, so a client can keep caching.
		if answer.Header().Get(etagField) != tag {
			t.Errorf("%s answered ETag %q", named, answer.Header().Get(etagField))
		}
	}

	// A version the caller does not hold is the resource itself.
	stale := call{method: http.MethodGet, path: path, ifNoneMatch: `W/"99"`}.send(t, routes)
	assertStatus(t, stale, http.StatusOK)
}

// TestAReadIsConditionalOnWhenItChangedToo, which is the header a client that
// kept the date rather than the version sends.
func TestAReadIsConditionalOnWhenItChangedToo(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")
	path := resourcePath("Organization", id)

	read := call{method: http.MethodGet, path: path}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	written := read.Header().Get(lastModifiedField)
	if written == "" {
		t.Fatal("a read carried no Last-Modified to be conditional on")
	}

	if answer := (call{
		method: http.MethodGet, path: path, ifModifiedSince: written,
	}).send(t, routes); answer.Code != http.StatusNotModified {
		t.Errorf("a caller holding the current copy answered %d, want 304", answer.Code)
	}

	// Older than the write is a caller whose copy is out of date.
	moment, err := http.ParseTime(written)
	if err != nil {
		t.Fatalf("read the date back: %v", err)
	}

	older := moment.Add(-time.Hour).UTC().Format(http.TimeFormat)

	if answer := (call{
		method: http.MethodGet, path: path, ifModifiedSince: older,
	}).send(t, routes); answer.Code != http.StatusOK {
		t.Errorf("a caller with an older copy answered %d, want the resource", answer.Code)
	}

	// A malformed date is ignored rather than refused, which is what RFC 9110
	// requires: the caller is answered with more than they asked for.
	if answer := (call{
		method: http.MethodGet, path: path, ifModifiedSince: "not a date",
	}).send(t, routes); answer.Code != http.StatusOK {
		t.Errorf("a malformed date answered %d", answer.Code)
	}
}

// TestAWriteAnswersWithWhatTheClientAskedFor.
func TestAWriteAnswersWithWhatTheClientAskedFor(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for named, held := range map[string]struct {
		prefer string
		body   func(*testing.T, string) bool
	}{
		"nothing back": {"return=minimal", func(t *testing.T, body string) bool {
			return body == ""
		}},
		"the resource": {"return=representation", func(t *testing.T, body string) bool {
			return strings.Contains(body, `"resourceType":"Organization"`)
		}},
		"an outcome": {"return=OperationOutcome", func(t *testing.T, body string) bool {
			return strings.Contains(body, `"resourceType":"OperationOutcome"`)
		}},
		"nothing asked for": {"", func(t *testing.T, body string) bool {
			return strings.Contains(body, `"resourceType":"Organization"`)
		}},
		"something this server does not know": {"return=elsewhere", func(t *testing.T, body string) bool {
			// A preference is a preference: an unknown one falls back rather
			// than being refused, which is what the header itself says.
			return strings.Contains(body, `"resourceType":"Organization"`)
		}},
	} {
		answer := call{
			method: http.MethodPost, path: fhir.BasePath + "/Organization",
			body: submission("Organization"), prefer: held.prefer,
		}.send(t, routes)

		assertStatus(t, answer, http.StatusCreated)

		if !held.body(t, answer.Body.String()) {
			t.Errorf("%s answered %q", named, answer.Body)
		}

		// Whatever was asked for, the write is still described by its headers.
		if answer.Header().Get(locationField) == "" || answer.Header().Get(etagField) == "" {
			t.Errorf("%s answered without naming what it wrote", named)
		}
	}
}

// TestAHistoryNarrowedToWhatChanged, and refusing the narrowings this server
// does not perform rather than answering the whole history to a caller who
// asked for part of it.
func TestAHistoryNarrowedToWhatChanged(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := withVersions(t, routes, 3)
	path := resourcePath("Organization", id) + "/_history"

	// Everything, so the moment to narrow by can be read from it.
	whole := historyPage(t, routes, path)
	if len(whole.Entry) != 3 {
		t.Fatalf("the history holds %d version(s), want 3", len(whole.Entry))
	}

	// Nothing changed after the newest version was written.
	newest := whole.Entry[0].Response.LastModified

	after, err := time.Parse(fhirInstant, newest)
	if err != nil {
		t.Fatalf("read the instant %q: %v", newest, err)
	}

	narrowed := historyPage(t, routes, path+"?"+sinceParameter+"="+
		after.Add(time.Second).UTC().Format(time.RFC3339))

	if len(narrowed.Entry) != 0 {
		t.Errorf("a history since after the last write holds %d", len(narrowed.Entry))
	}

	// And everything written after the beginning of time is everything.
	everything := historyPage(t, routes, path+"?"+sinceParameter+"=1970-01-01T00:00:00Z")
	if len(everything.Entry) != 3 {
		t.Errorf("a history since the epoch holds %d, want 3", len(everything.Entry))
	}
}

// TestANarrowingThisServerDoesNotPerformIsRefused. A caller who narrowed a
// history and was handed the whole of it has no way to tell, and will read it
// as the answer to what they asked.
func TestANarrowingThisServerDoesNotPerformIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")
	path := resourcePath("Organization", id) + "/_history"

	for _, asked := range []string{
		atParameter + "=2026-01-01",
		listParameter + "=a-list",
		"_summary=true",
		sinceParameter + "=not-an-instant",
	} {
		answer := call{method: http.MethodGet, path: path + "?" + asked}.send(t, routes)

		assertIssue(t, answer, http.StatusBadRequest, fhir.CodeInvalid)
	}
}

// namedOrganization writes one with an identifier a condition can find it by.
func namedOrganization(t *testing.T, routes http.Handler, value, name string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Organization",
		body: `{"resourceType":"Organization","name":"` + name + `",` +
			`"identifier":[{"system":"http://example.test/ids","value":"` + value + `"}]}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// TestAConditionalUpdateReplacesTheOneItMatches, so a client holding an
// identifier from its own system can keep a record current without ever
// learning the id this server minted.
func TestAConditionalUpdateReplacesTheOneItMatches(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := namedOrganization(t, routes, "keep", "the first name")

	answer := call{
		method: http.MethodPut,
		path:   fhir.BasePath + "/Organization?identifier=http://example.test/ids|keep",
		body: `{"resourceType":"Organization","name":"the second name",` +
			`"identifier":[{"system":"http://example.test/ids","value":"keep"}]}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if got := resourceID(t, answer); got != id {
		t.Errorf("a conditional update wrote %s, want %s", got, id)
	}

	read := call{method: http.MethodGet, path: resourcePath("Organization", id)}.send(t, routes)
	if !strings.Contains(read.Body.String(), "the second name") {
		t.Errorf("the resource reads %s", read.Body)
	}

	// One resource, not two.
	if found := searchedOrganizations(t, routes); found != 1 {
		t.Errorf("%d Organizations exist after a conditional update", found)
	}
}

// TestAConditionalUpdateMatchingNothingCreates, which is what R4 says and what
// this server's unconditional update does as well.
func TestAConditionalUpdateMatchingNothingCreates(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPut,
		path:   fhir.BasePath + "/Organization?identifier=http://example.test/ids|new",
		body: `{"resourceType":"Organization","name":"brought into being",` +
			`"identifier":[{"system":"http://example.test/ids","value":"new"}]}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	if found := searchedOrganizations(t, routes); found != 1 {
		t.Errorf("%d Organizations exist, want the one that was created", found)
	}
}

// TestAConditionalDeleteRemovesTheOneItMatches, and treats nothing matching as
// done: the client wanted none matching, and there are none.
func TestAConditionalDeleteRemovesTheOneItMatches(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := namedOrganization(t, routes, "gone", "to be removed")

	condition := fhir.BasePath + "/Organization?identifier=http://example.test/ids|gone"

	assertStatus(t, call{method: http.MethodDelete, path: condition}.send(t, routes),
		http.StatusNoContent)

	assertIssue(t, call{
		method: http.MethodGet, path: resourcePath("Organization", id),
	}.send(t, routes), http.StatusGone, fhir.CodeDeleted)

	// Asked again, still done: a retry after a successful delete is ordinary.
	assertStatus(t, call{method: http.MethodDelete, path: condition}.send(t, routes),
		http.StatusNoContent)
}

// TestAConditionalWriteWithNoConditionIsRefused, because without one it would
// act on every resource of the type.
func TestAConditionalWriteWithNoConditionIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	namedOrganization(t, routes, "safe", "not to be touched")

	for _, held := range []struct {
		method, body string
	}{
		{http.MethodPut, `{"resourceType":"Organization","name":"x"}`},
		{http.MethodDelete, ""},
	} {
		assertIssue(t, call{
			method: held.method, path: fhir.BasePath + "/Organization", body: held.body,
		}.send(t, routes), http.StatusBadRequest, fhir.CodeInvalid)
	}

	if found := searchedOrganizations(t, routes); found != 1 {
		t.Errorf("an unconditioned write changed the store: %d remain", found)
	}
}

// TestTheStatementDeclaresTheConditionalsItPerforms, because a client reads the
// statement to know whether it may rely on one.
func TestTheStatementDeclaresTheConditionalsItPerforms(t *testing.T) {
	for _, resource := range advertised(t) {
		if !resource.ConditionalCreate || !resource.ConditionalUpdate {
			t.Errorf("%s does not declare the conditionals this server performs", resource.Type)
		}

		// Which kind of conditional delete matters: one that removes every
		// match and one that removes a single match answer the same request
		// differently.
		if resource.ConditionalDelete != "single" {
			t.Errorf("%s declares conditionalDelete %q", resource.Type, resource.ConditionalDelete)
		}
	}
}
