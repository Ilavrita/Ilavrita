package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// TestInteractionLifecycle runs every status and header rule the six
// interactions carry, against every type the CapabilityStatement advertises. A
// type is not advertised until this suite covers it.
func TestInteractionLifecycle(t *testing.T) {
	for _, resourceType := range fhir.ServedResourceTypes() {
		if mintsItsOwnCompartment(t, resourceType) {
			// A create mints this type's compartment, and no confined grant can
			// name one that does not exist yet. TestACompartmentSubjectIsCreatedByNamingIt
			// covers the path such a type is actually created through.
			continue
		}

		t.Run(resourceType, func(t *testing.T) {
			routes := servingFHIR(t, everyAction)

			id := assertCreate(t, routes, resourceType)
			assertRead(t, routes, resourceType, id)
			assertUpdate(t, routes, resourceType, id)
			assertVersionRead(t, routes, resourceType, id)
			assertHistory(t, routes, resourceType, id)
			assertDelete(t, routes, resourceType, id)
			assertRecreate(t, routes, resourceType, id)
		})
	}
}

// assertCreate: 201 always, a versioned Location, and a weak ETag naming the
// first version.
func assertCreate(t *testing.T, routes http.Handler, resourceType string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost,
		path:   fhir.BasePath + "/" + resourceType,
		body:   submission(resourceType),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)
	assertVersionHeaders(t, answer, "1")

	id := resourceID(t, answer)

	want := "http://example.com" + resourcePath(resourceType, id) + "/_history/1"
	if got := answer.Header().Get(locationField); got != want {
		t.Fatalf("Location %q, want %q", got, want)
	}

	return id
}

func assertRead(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	answer := call{method: http.MethodGet, path: resourcePath(resourceType, id)}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)
	assertVersionHeaders(t, answer, "1")

	if got := decodeResource(t, answer)["id"]; got != id {
		t.Fatalf("read answered with id %v, want %q", got, id)
	}

	assertIssue(t,
		call{method: http.MethodGet, path: resourcePath(resourceType, "absent")}.send(t, routes),
		http.StatusNotFound, fhir.CodeNotFound)
}

// assertUpdate takes the resource from version 1 to 3, and asserts that a stale
// claim is refused without writing anything in between.
func assertUpdate(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	path := resourcePath(resourceType, id)
	body := replacement(resourceType, id)

	unconditional := call{method: http.MethodPut, path: path, body: body}.send(t, routes)
	assertStatus(t, unconditional, http.StatusOK)
	assertVersionHeaders(t, unconditional, "2")

	stale := call{method: http.MethodPut, path: path, body: body, ifMatch: `W/"1"`}.send(t, routes)
	assertIssue(t, stale, http.StatusPreconditionFailed, fhir.CodeConflict)

	assertVersionHeaders(t, call{method: http.MethodGet, path: path}.send(t, routes), "2")

	matched := call{method: http.MethodPut, path: path, body: body, ifMatch: `"2"`}.send(t, routes)
	assertStatus(t, matched, http.StatusOK)
	assertVersionHeaders(t, matched, "3")

	mismatched := call{method: http.MethodPut, path: path, body: replacement(resourceType, "elsewhere")}.send(t, routes)
	assertIssue(t, mismatched, http.StatusBadRequest, fhir.CodeInvalid)
}

// assertVersionRead: the ETag echoes the version asked for, never the current
// one, and a version id that can name no row is missing rather than an error.
func assertVersionRead(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	first := call{method: http.MethodGet, path: resourcePath(resourceType, id) + "/_history/1"}.send(t, routes)
	assertStatus(t, first, http.StatusOK)
	assertVersionHeaders(t, first, "1")

	for _, version := range []string{"99", "abc", "0", "-1", "01", "1.0"} {
		assertIssue(t,
			call{method: http.MethodGet, path: resourcePath(resourceType, id) + "/_history/" + version}.send(t, routes),
			http.StatusNotFound, fhir.CodeNotFound)
	}
}

func assertHistory(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	answer := call{method: http.MethodGet, path: resourcePath(resourceType, id) + "/_history"}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	bundle := decodeBundle(t, answer)
	if bundle.ResourceType != "Bundle" || bundle.Type != fhir.BundleHistory ||
		bundle.Total == nil || *bundle.Total != 3 {
		t.Fatalf("bundle is a %s %s of %d, want a history Bundle of 3",
			bundle.ResourceType, bundle.Type, bundle.Total)
	}

	// Newest first, and the oldest version of an identity is always its create.
	assertEntries(t, bundle, []fhir.HTTPVerb{fhir.VerbPut, fhir.VerbPut, fhir.VerbPost}, []string{"3", "2", "1"})

	assertIssue(t,
		call{method: http.MethodGet, path: resourcePath(resourceType, "absent") + "/_history"}.send(t, routes),
		http.StatusNotFound, fhir.CodeNotFound)
}

func assertDelete(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	path := resourcePath(resourceType, id)

	stale := call{method: http.MethodDelete, path: path, ifMatch: `W/"1"`}.send(t, routes)
	assertIssue(t, stale, http.StatusPreconditionFailed, fhir.CodeConflict)

	answer := call{method: http.MethodDelete, path: path}.send(t, routes)
	assertStatus(t, answer, http.StatusNoContent)

	if answer.Body.Len() != 0 {
		t.Fatalf("delete answered with a body: %s", answer.Body.String())
	}

	assertIssue(t, call{method: http.MethodGet, path: path}.send(t, routes),
		http.StatusGone, fhir.CodeDeleted)

	assertIssue(t, call{method: http.MethodGet, path: path + "/_history/4"}.send(t, routes),
		http.StatusGone, fhir.CodeDeleted)

	// A history that ends in a deletion is still a history, so the collection
	// answers where the instance no longer does.
	history := call{method: http.MethodGet, path: path + "/_history"}.send(t, routes)
	assertStatus(t, history, http.StatusOK)

	bundle := decodeBundle(t, history)
	if bundle.Entry[0].Request.Method != fhir.VerbDelete || bundle.Entry[0].Resource != nil {
		t.Fatalf("newest entry is %+v, want a DELETE carrying no resource", bundle.Entry[0])
	}
}

// assertRecreate: an update over a tombstone creates, and the version counter
// carries on rather than restarting at 1.
func assertRecreate(t *testing.T, routes http.Handler, resourceType, id string) {
	t.Helper()

	path := resourcePath(resourceType, id)
	body := replacement(resourceType, id)

	claimed := call{method: http.MethodPut, path: path, body: body, ifMatch: `W/"4"`}.send(t, routes)
	assertIssue(t, claimed, http.StatusPreconditionFailed, fhir.CodeConflict)

	answer := call{method: http.MethodPut, path: path, body: body}.send(t, routes)
	assertStatus(t, answer, http.StatusCreated)
	assertVersionHeaders(t, answer, "5")
}

// An id the client chose is created rather than refused: nothing in the storage
// layer can tell a client-named id from a minted one.
func TestUpdateCreatesTheIDTheClientNames(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPut,
		path:   resourcePath("Location", "chosen-id"),
		body:   replacement("Location", "chosen-id"),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)
	assertVersionHeaders(t, answer, "1")

	if got := answer.Header().Get(locationField); !strings.HasSuffix(got, "/Location/chosen-id/_history/1") {
		t.Fatalf("Location %q does not name the created version", got)
	}
}

// A version claimed against something nothing is visible at is at least as
// strong a mismatch as a wrong one, so it is a precondition failure.
func TestUpdateRefusesAVersionClaimedAgainstNothing(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method:  http.MethodPut,
		path:    resourcePath("Location", "absent"),
		body:    replacement("Location", "absent"),
		ifMatch: `W/"1"`,
	}.send(t, routes)

	assertIssue(t, answer, http.StatusPreconditionFailed, fhir.CodeConflict)
}

// "*" claims that the resource exists. A live one satisfies it, so the write
// proceeds exactly as an unconditional one would.
func TestIfMatchStarReplacesALiveResource(t *testing.T) {
	routes := servingFHIR(t, everyAction)
	id := assertCreate(t, routes, "Organization")

	answer := call{
		method:  http.MethodPut,
		path:    resourcePath("Organization", id),
		body:    replacement("Organization", id),
		ifMatch: "*",
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)
	assertVersionHeaders(t, answer, "2")
}

// And nothing satisfies it when there is no current representation. RFC 9110
// 13.1.1 requires 412 there, so a client using "*" to mean "replace, never
// create" is answered rather than quietly given a resource it did not ask for.
func TestIfMatchStarNeverCreates(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	absent := call{
		method:  http.MethodPut,
		path:    resourcePath("Organization", "neverused"),
		body:    replacement("Organization", "neverused"),
		ifMatch: "*",
	}.send(t, routes)

	assertIssue(t, absent, http.StatusPreconditionFailed, fhir.CodeConflict)

	assertIssue(t, call{method: http.MethodGet, path: resourcePath("Organization", "neverused")}.send(t, routes),
		http.StatusNotFound, fhir.CodeNotFound)

	// A tombstone has no current representation either, so it is refused the
	// same way rather than being recreated.
	id := assertCreate(t, routes, "Organization")
	assertStatus(t, call{method: http.MethodDelete, path: resourcePath("Organization", id)}.send(t, routes),
		http.StatusNoContent)

	buried := call{
		method:  http.MethodPut,
		path:    resourcePath("Organization", id),
		body:    replacement("Organization", id),
		ifMatch: "*",
	}.send(t, routes)

	assertIssue(t, buried, http.StatusPreconditionFailed, fhir.CodeConflict)
}

// A client that claimed no version asked for last-write-wins. It must not be
// handed a conflict it never opted into and has nothing to retry against.
func TestAnUnconditionalUpdateIsNotVersionLocked(t *testing.T) {
	routes := servingFHIR(t, everyAction)
	id := assertCreate(t, routes, "Organization")
	path := resourcePath("Organization", id)

	const writers = 24

	answers := make(chan int, writers)

	for range writers {
		go func() {
			answers <- call{method: http.MethodPut, path: path, body: replacement("Organization", id)}.
				send(t, routes).Code
		}()
	}

	for range writers {
		if status := <-answers; status != http.StatusOK {
			t.Errorf("an unconditional update answered %d, want %d", status, http.StatusOK)
		}
	}
}

func TestAMalformedIfMatchIsRefusedBeforeAnyWrite(t *testing.T) {
	routes := servingFHIR(t, everyAction)
	id := assertCreate(t, routes, "Organization")
	path := resourcePath("Organization", id)

	for _, claimed := range []string{"1", `"1", "2"`, `""`, "W/1", "not a version"} {
		assertIssue(t, call{method: http.MethodDelete, path: path, ifMatch: claimed}.send(t, routes),
			http.StatusBadRequest, fhir.CodeInvalid)
	}

	assertVersionHeaders(t, call{method: http.MethodGet, path: path}.send(t, routes), "1")
}

func TestCreateRefusesABodyTheURLDisagreesWith(t *testing.T) {
	routes := servingFHIR(t, everyAction)
	collection := fhir.BasePath + "/Organization"

	refused := map[string]string{
		"another type":       `{"resourceType":"Location"}`,
		"no type at all":     `{"name":"nameless"}`,
		"a client-set id":    `{"resourceType":"Organization","id":"chosen"}`,
		"not JSON at all":    `{`,
		"not an object":      `["Organization"]`,
		"an unreadable meta": `{"resourceType":"Organization","meta":7}`,
	}

	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			assertIssue(t, call{method: http.MethodPost, path: collection, body: body}.send(t, routes),
				http.StatusBadRequest, fhir.CodeInvalid)
		})
	}
}

// TestCreateRefusesAConditionOnItsOwnExistence. `If-None-Exist` is how R4 asks
// for a create that does nothing if the resource is already there. This server
// performs no conditional interaction, and a header quietly dropped is worse
// than one refused: the client believes it was given idempotency and is holding
// a duplicate it will never look for.
func TestCreateRefusesAConditionOnItsOwnExistence(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method:      http.MethodPost,
		path:        fhir.BasePath + "/Organization",
		body:        submission("Organization"),
		ifNoneExist: "name=the%20same%20one",
	}.send(t, routes)

	assertIssue(t, answer, http.StatusBadRequest, fhir.CodeNotSupported)

	// And nothing was created, so the refusal is not a duplicate with an error
	// page in front of it.
	if found := searchedOrganizations(t, routes); found != 0 {
		t.Errorf("a refused conditional create wrote %d Organization(s)", found)
	}
}

// The version and the instant belong to the row. A client claiming them must
// not have that claim stored, or a later read would contradict its own ETag.
func TestSubmittedMetaNeverOverridesTheRow(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost,
		path:   fhir.BasePath + "/Organization",
		body:   `{"resourceType":"Organization","meta":{"versionId":"99","lastUpdated":"1999-01-01T00:00:00Z","source":"kept"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)
	assertVersionHeaders(t, answer, "1")

	meta, _ := decodeResource(t, answer)["meta"].(map[string]any)
	if meta["source"] != "kept" {
		t.Fatalf("meta is %v, want the client's own members kept", meta)
	}

	if meta["lastUpdated"] == "1999-01-01T00:00:00Z" {
		t.Fatal("the submitted lastUpdated survived into the stored resource")
	}
}

// Nothing identifies the caller by default. That is a strictly prior question to
// what a caller may do, so it is answered before any Scope exists to widen.
func TestWithoutAPrincipalEveryInteractionIsUnauthenticated(t *testing.T) {
	serve(t, &backend{})

	routes := fhirRoutes(t)

	for _, attempt := range everyInteraction("Organization", "example") {
		assertIssue(t, attempt.send(t, routes), http.StatusUnauthorized, fhir.CodeLogin)
	}

	// The CapabilityStatement names no resource and touches no storage, so a
	// client may read it before it has authenticated at all.
	assertStatus(t, call{method: http.MethodGet, path: fhir.BasePath + metadataPath}.send(t, routes), http.StatusOK)
}

// A Scope that authorizes nothing is a refusal, never a lookup: a client learns
// what it may not do, never whether the resource it named exists.
func TestAScopeThatAuthorizesNothingIsForbidden(t *testing.T) {
	routes := servingFHIR(t, nil)

	for _, attempt := range everyInteraction("Organization", "example") {
		assertIssue(t, attempt.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)
	}
}

// A read Grant alone does not authorize history: the two are separate decisions
// and this is one of the two directions that must be asserted.
func TestAReadGrantDoesNotAuthorizeHistory(t *testing.T) {
	routes := servingFHIR(t, []storage.Action{storage.ActionRead})
	path := resourcePath("Organization", "example")

	assertIssue(t, call{method: http.MethodGet, path: path}.send(t, routes),
		http.StatusNotFound, fhir.CodeNotFound)

	assertIssue(t, call{method: http.MethodGet, path: path + "/_history"}.send(t, routes),
		http.StatusForbidden, fhir.CodeForbidden)

	assertIssue(t, call{method: http.MethodGet, path: path + "/_history/1"}.send(t, routes),
		http.StatusForbidden, fhir.CodeForbidden)
}

// And the other direction: a history Grant alone does not authorize a read.
func TestAHistoryGrantDoesNotAuthorizeARead(t *testing.T) {
	routes := servingFHIR(t, []storage.Action{storage.ActionHistory})
	path := resourcePath("Organization", "example")

	assertIssue(t, call{method: http.MethodGet, path: path}.send(t, routes),
		http.StatusForbidden, fhir.CodeForbidden)

	assertIssue(t, call{method: http.MethodGet, path: path + "/_history"}.send(t, routes),
		http.StatusNotFound, fhir.CodeNotFound)
}

// Storage takes any type name uncritically, so the declared list is the only
// thing that makes a name an endpoint.
func TestAnUndeclaredTypeIsNotAnEndpoint(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, attempt := range everyInteraction("NoSuchType", "example") {
		assertIssue(t, attempt.send(t, routes), http.StatusNotFound, fhir.CodeNotFound)
	}
}

// The same answer whether or not a principal is configured: an undeclared type
// reveals nothing about this deployment's authentication.
func TestAnUndeclaredTypeAnswersBeforeThePrincipal(t *testing.T) {
	serve(t, &backend{})

	routes := fhirRoutes(t)

	for _, attempt := range everyInteraction("NoSuchType", "example") {
		assertIssue(t, attempt.send(t, routes), http.StatusNotFound, fhir.CodeNotFound)
	}
}

// A by-key route carries no way to name another Project, so a principal
// authenticated into one can never reach another's rows, even by naming an id
// that exists there.
func TestAPrincipalReachesOnlyItsOwnProject(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	seedProject(t, db, otherProject)

	routes := fhirRoutes(t)

	serveProject(t, db, homeProject, everyAction)
	id := assertCreate(t, routes, "Organization")

	serveProject(t, db, otherProject, everyAction)

	path := resourcePath("Organization", id)

	for _, attempt := range []call{
		{method: http.MethodGet, path: path},
		{method: http.MethodDelete, path: path},
		{method: http.MethodGet, path: path + "/_history"},
		{method: http.MethodGet, path: path + "/_history/1"},
	} {
		assertIssue(t, attempt.send(t, routes), http.StatusNotFound, fhir.CodeNotFound)
	}

	// An update naming the same id creates a second resource in the second
	// Project rather than replacing the first one.
	assertStatus(t, call{method: http.MethodPut, path: path, body: replacement("Organization", id)}.send(t, routes),
		http.StatusCreated)

	serveProject(t, db, homeProject, everyAction)
	assertVersionHeaders(t, call{method: http.MethodGet, path: path}.send(t, routes), "1")
}

func TestUnsupportedRepresentationsAreRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)
	path := resourcePath("Organization", "example")

	assertIssue(t, call{method: http.MethodGet, path: path, accept: "application/fhir+xml"}.send(t, routes),
		http.StatusNotAcceptable, fhir.CodeNotSupported)

	assertIssue(t, call{method: http.MethodGet, path: path + "?_format=xml"}.send(t, routes),
		http.StatusNotAcceptable, fhir.CodeNotSupported)

	assertIssue(t, call{
		method:      http.MethodPost,
		path:        fhir.BasePath + "/Organization",
		body:        submission("Organization"),
		contentType: "text/plain",
	}.send(t, routes), http.StatusUnsupportedMediaType, fhir.CodeNotSupported)
}

func TestTheRepresentationThisServerSendsIsAccepted(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, accepted := range []string{"*/*", "application/fhir+json", "application/json; q=0.9", "text/html, */*"} {
		answer := call{
			method: http.MethodPost,
			path:   fhir.BasePath + "/Organization",
			body:   submission("Organization"),
			accept: accepted,
		}.send(t, routes)

		assertStatus(t, answer, http.StatusCreated)
	}
}

// The interactions that are not implemented keep answering as they did, so a
// client can still tell "not here" from "not supported".
func TestUnimplementedInteractionsStillAnswerNotSupported(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, path := range []string{
		fhir.BasePath + "/Organization/example/$everything",
		fhir.BasePath + "/Organization/_history",
		fhir.BasePath + "/_history",
	} {
		assertIssue(t, call{method: http.MethodGet, path: path}.send(t, routes),
			http.StatusNotImplemented, fhir.CodeNotSupported)
	}
}

func everyInteraction(resourceType, id string) []call {
	calls := make([]call, 0, len(servedInteractions))
	for _, served := range servedInteractions {
		calls = append(calls, served.request(resourceType, id))
	}

	return calls
}

// mintsItsOwnCompartment reports a type whose create reaches into nothing: its
// only compartment is the one it is. It is derived rather than listed, so a
// fixture cannot drift from what the server actually computes.
func mintsItsOwnCompartment(t *testing.T, resourceType string) bool {
	t.Helper()

	const probe = storage.LogicalID("probe")

	derived, err := fhir.Compartments(resourceType, probe, json.RawMessage(submission(resourceType)))
	if err != nil {
		t.Fatalf("derive %s compartments: %v", resourceType, err)
	}

	self := storage.Compartment{Type: storage.ResourceType(resourceType), ID: probe}

	return len(derived) == 1 && derived[0] == self
}

// TestACompartmentSubjectIsCreatedByNamingIt. A Patient is its own compartment,
// so a confined grant authorizes creating one only when it already names that
// patient — which means a client-named id, never a minted one. That is the whole
// difference between provisioning a compartment and writing into someone else's.
func TestACompartmentSubjectIsCreatedByNamingIt(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	named := resourcePath("Patient", string(conformancePatient))
	body := `{"resourceType":"Patient","id":"` + string(conformancePatient) + `"}`

	answer := call{method: http.MethodPut, path: named, body: body}.send(t, routes)
	assertStatus(t, answer, http.StatusCreated)

	// And it is readable afterwards, which is what the compartment projection is
	// for: without the row the confined read would find nothing it just wrote.
	assertStatus(t, call{method: http.MethodGet, path: named}.send(t, routes), http.StatusOK)

	// Any other patient is refused: the grant names one compartment, not the type.
	other := resourcePath("Patient", "someone-else")
	refused := call{
		method: http.MethodPut, path: other,
		body: `{"resourceType":"Patient","id":"someone-else"}`,
	}.send(t, routes)
	assertIssue(t, refused, http.StatusForbidden, fhir.CodeForbidden)
}

// replacement is a body naming the id it replaces.
//
// It is the submission with the id written in: an update states the whole
// resource, so a replacement missing what a create had to carry is a body no
// client could send either. Binary's bytes differ, so a test can tell the two
// versions apart.
func replacement(resourceType, id string) string {
	fields := map[string]json.RawMessage{}

	if err := json.Unmarshal([]byte(submission(resourceType)), &fields); err != nil {
		t := "a fixture that does not parse: " + err.Error()
		panic(t)
	}

	fields[idField] = json.RawMessage(`"` + id + `"`)

	if resourceType == string(binaryType) {
		fields["data"] = json.RawMessage(`"cmVwbGFjZWQ="`)
	}

	body, err := json.Marshal(fields)
	if err != nil {
		panic("a replacement that cannot be encoded: " + err.Error())
	}

	return string(body)
}

func decodeBundle(t *testing.T, answer *httptest.ResponseRecorder) fhir.Bundle {
	t.Helper()

	var bundle fhir.Bundle
	if err := json.Unmarshal(answer.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode the bundle: %v; body %s", err, answer.Body.String())
	}

	return bundle
}

func assertEntries(t *testing.T, bundle fhir.Bundle, methods []fhir.HTTPVerb, versions []string) {
	t.Helper()

	if len(bundle.Entry) != len(methods) {
		t.Fatalf("bundle carries %d entries, want %d", len(bundle.Entry), len(methods))
	}

	for index, entry := range bundle.Entry {
		if entry.Request.Method != methods[index] {
			t.Fatalf("entry %d records %s, want %s", index, entry.Request.Method, methods[index])
		}

		if entry.Response.ETag != `W/"`+versions[index]+`"` {
			t.Fatalf("entry %d is version %q, want %q", index, entry.Response.ETag, versions[index])
		}

		if entry.Resource == nil {
			t.Fatalf("entry %d carries no resource", index)
		}
	}
}

// A principal whose read is confined to a compartment can never see a row it
// has just created, because storage projects no compartment for one. It is
// refused before the write rather than told 404 about a row that did commit.
func TestAConfinedReadMayNotWrite(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveUnder(t, db, homeProject, confinedReadPolicy(t, homeProject))

	routes := fhirRoutes(t)

	for _, attempt := range []call{
		{method: http.MethodPost, path: fhir.BasePath + "/Practitioner", body: submission("Practitioner")},
		{
			method: http.MethodPut,
			path:   resourcePath("Practitioner", "named1"),
			body:   replacement("Practitioner", "named1"),
		},
	} {
		assertIssue(t, attempt.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)
	}

	assertStoredCount(t, db, "Practitioner", 0)
}

// 409 against a taken id and 404 against a free one would enumerate the ids a
// caller cannot read. Both are refused before storage is asked, so an update
// answers identically whichever the id turns out to be.
func TestAConfinedReadCannotProbeForTakenIDs(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)
	taken := assertCreate(t, routes, "Practitioner")

	serveUnder(t, db, homeProject, confinedReadPolicy(t, homeProject))

	for _, id := range []string{taken, "neverused1"} {
		answer := call{
			method: http.MethodPut,
			path:   resourcePath("Practitioner", id),
			body:   replacement("Practitioner", id),
		}.send(t, routes)

		assertIssue(t, answer, http.StatusForbidden, fhir.CodeForbidden)
	}

	assertStoredCount(t, db, "Practitioner", 1)
}

// The read-back that renders a write shares the write's transaction, so a Scope
// that cannot see what it wrote commits nothing. Without that, a client is told
// a row does not exist while the row is there, under an id it never learned.
func TestAWriteWhoseReadBackFailsCommitsNothing(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)
	id := assertCreate(t, routes, "Organization")

	key, err := storage.NewResourceKey(homeProject, "Organization", storage.LogicalID(id))
	if err != nil {
		t.Fatalf("name the resource: %v", err)
	}

	blind := writeOnly(db, key)
	content := json.RawMessage(replacement("Organization", id))

	_, err = blind.written(context.Background(), key, func(ctx context.Context) error {
		return blind.resources.Update(ctx, blind.scope, storage.ResourceRecord{Key: key, Content: content}, "1")
	}, nil)

	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the read-back answered %v, want %v", err, storage.ErrNotFound)
	}

	assertVersionHeaders(t, call{method: http.MethodGet, path: resourcePath("Organization", id)}.send(t, routes), "1")
}

// writeOnly holds a Scope that may write a row and may not read one, which is
// the shape that proves the read-back is inside the write's commit boundary.
func writeOnly(db *sql.DB, key storage.ResourceKey) granted {
	store := sqlite.NewResourceStore(db)

	return granted{
		resources:    store,
		versions:     store,
		transactions: store,
		project:      key.Project,
		resourceType: key.Type,
		scope: storage.NewScope(storage.Grant{
			Project: key.Project, Kind: storage.KindFHIR, Type: key.Type,
			Action: storage.ActionWrite, Source: storage.SourceMembership,
		}),
	}
}

// A Host header is chosen by the client. Reflecting an unrecognised one would
// let a client point another at an attacker's origin through Location,
// fullUrl or the CapabilityStatement, so the request is refused instead.
func TestAnUnrecognisedHostPublishesNothing(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)
	id := assertCreate(t, routes, "Organization")

	for _, attempt := range []call{
		{method: http.MethodGet, path: fhir.BasePath + metadataPath, host: "evil.example"},
		{method: http.MethodGet, path: resourcePath("Organization", id), host: "evil.example"},
		{method: http.MethodGet, path: resourcePath("Organization", id) + "/_history", host: "evil.example"},
		{
			method: http.MethodPost,
			path:   fhir.BasePath + "/Organization",
			body:   submission("Organization"),
			host:   "evil.example",
		},
	} {
		assertIssue(t, attempt.send(t, routes), http.StatusBadRequest, fhir.CodeSecurity)
	}

	// The refused create wrote nothing: the address is settled before storage.
	assertStoredCount(t, db, "Organization", 1)
}

// A body this server declines to buffer is refused as too costly, not as one it
// could not parse: a client that sent too much is told which mistake it made.
func TestAnOversizedBodyIsRefusedAsTooCostly(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	oversized := `{"resourceType":"Organization","name":"` + strings.Repeat("x", maximumBodyBytes) + `"}`

	answer := call{method: http.MethodPost, path: fhir.BasePath + "/Organization", body: oversized}.send(t, routes)
	assertIssue(t, answer, http.StatusRequestEntityTooLarge, fhir.CodeTooCostly)
}
