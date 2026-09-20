package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// submitting posts one Bundle to the base path, which is where a transaction
// goes.
func submitting(t *testing.T, routes http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	return call{method: http.MethodPost, path: fhir.BasePath, body: body}.send(t, routes)
}

// transactionOf wraps entries in a Bundle of the type that is all-or-nothing.
func transactionOf(entries ...string) string {
	return `{"resourceType":"Bundle","type":"transaction","entry":[` +
		strings.Join(entries, ",") + `]}`
}

// entryOf writes one entry.
func entryOf(fullURL, method, url, resource string) string {
	held := `{`
	if fullURL != "" {
		held += `"fullUrl":"` + fullURL + `",`
	}

	if resource != "" {
		held += `"resource":` + resource + `,`
	}

	return held + `"request":{"method":"` + method + `","url":"` + url + `"}}`
}

// responseBundle reads a transaction response.
func responseBundle(t *testing.T, recorded *httptest.ResponseRecorder) fhir.Bundle {
	t.Helper()

	var held fhir.Bundle
	if err := json.Unmarshal(recorded.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the response bundle: %v (%s)", err, recorded.Body)
	}

	if held.ResourceType != "Bundle" || held.Type != fhir.BundleTransactionResponse {
		t.Fatalf("answered %s/%s", held.ResourceType, held.Type)
	}

	return held
}

// TestATransactionWritesEveryEntry, which is the ordinary case it exists for.
func TestATransactionWritesEveryEntry(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
		entryOf("urn:uuid:two", "POST", "Organization", valid("Organization", nil)),
	))

	assertStatus(t, answer, http.StatusOK)

	held := responseBundle(t, answer)
	if len(held.Entry) != 2 {
		t.Fatalf("it answered %d entries", len(held.Entry))
	}

	for index, entry := range held.Entry {
		if entry.Response == nil || !strings.HasPrefix(entry.Response.Status, "201") {
			t.Errorf("entry %d answered %+v", index, entry.Response)
		}

		if entry.Response.ETag == "" || entry.FullURL == "" {
			t.Errorf("entry %d names %q at %q", index, entry.Response.ETag, entry.FullURL)
		}
	}

	// And both are readable afterwards.
	if found := searchedOrganizations(t, routes); found != 2 {
		t.Errorf("%d organizations exist after a transaction of two", found)
	}
}

// TestATransactionIsAllOrNothing. This is the whole reason to use one: a Patient
// and their Observations either all exist or none do, and an entry that fails
// takes every other entry with it.
func TestATransactionIsAllOrNothing(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
		// A resource this server refuses: R4 requires Observation.status.
		entryOf("urn:uuid:two", "POST", "Observation",
			`{"resourceType":"Observation","subject":{"reference":"Patient/`+
				string(conformancePatient)+`"}}`),
		entryOf("urn:uuid:three", "POST", "Organization", valid("Organization", nil)),
	))

	if answer.Code < 400 {
		t.Fatalf("a transaction holding a refused entry answered %d", answer.Code)
	}

	// Nothing was written — not the entry before the failure, nor the one after.
	if found := searchedOrganizations(t, routes); found != 0 {
		t.Errorf("%d organizations survived a transaction that failed", found)
	}
}

// TestATransactionResolvesItsOwnReferences. A transaction states a Patient and
// an Observation about them at once, and the Observation cannot name an id
// nobody has minted yet — so it names the placeholder, and the server rewrites
// it to what it assigned.
func TestATransactionResolvesItsOwnReferences(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	patient := `{"resourceType":"Patient","id":"` + string(conformancePatient) + `"}`

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:the-patient", "PUT",
			"Patient/"+string(conformancePatient), patient),
		entryOf("urn:uuid:the-reading", "POST", "Observation", valid("Observation",
			map[string]string{"subject": `{"reference":"urn:uuid:the-patient"}`})),
	))

	assertStatus(t, answer, http.StatusOK)

	// The Observation is stored naming the patient by the identity the
	// transaction settled on, not by the placeholder.
	body := readOnlyObservation(t, routes)

	if strings.Contains(body, "urn:uuid:") {
		t.Errorf("a placeholder survived into the stored resource: %s", body)
	}

	if !strings.Contains(body, "Patient/"+string(conformancePatient)) {
		t.Errorf("the reference was not resolved: %s", body)
	}
}

// TestAPlaceholderNobodyClaimsIsLeftAlone. It may name something outside this
// transaction, and rewriting it to nothing would quietly break a link the client
// meant.
func TestAPlaceholderNobodyClaimsIsLeftAlone(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:the-reading", "POST", "Observation", valid("Observation",
			map[string]string{"performer": `[{"reference":"urn:uuid:somebody-else"}]`})),
	))

	assertStatus(t, answer, http.StatusOK)

	if body := readOnlyObservation(t, routes); !strings.Contains(body, "urn:uuid:somebody-else") {
		t.Errorf("a placeholder nobody claimed was rewritten: %s", body)
	}
}

// TestEveryEntryIsDecidedOnItsOwn. A Bundle is not a way to perform an
// interaction the caller could not have performed one at a time.
func TestEveryEntryIsDecidedOnItsOwn(t *testing.T) {
	// Read and nothing else, so no entry may write.
	routes := servingFHIR(t, readActions)

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
	))

	if answer.Code != http.StatusForbidden {
		t.Errorf("a caller who may not write answered %d: %s", answer.Code, answer.Body)
	}
}

// TestATransactionRefusesWhatItCannotPerform.
func TestATransactionRefusesWhatItCannotPerform(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for described, body := range map[string]string{
		"a batch, which this build does not perform":         `{"resourceType":"Bundle","type":"batch","entry":[]}`,
		"a resource that is not a Bundle":                    `{"resourceType":"Patient"}`,
		"an entry naming no request":                         `{"resourceType":"Bundle","type":"transaction","entry":[{"resource":{"resourceType":"Organization"}}]}`,
		"an entry naming a type nobody serves":               transactionOf(entryOf("", "POST", "Appointment", `{"resourceType":"Appointment"}`)),
		"an entry naming an interaction it does not perform": transactionOf(entryOf("", "PATCH", "Organization/x", `{}`)),
		"two entries claiming one identity": transactionOf(
			entryOf("urn:uuid:same", "POST", "Organization", valid("Organization", nil)),
			entryOf("urn:uuid:same", "POST", "Organization", valid("Organization", nil)),
		),
	} {
		if answer := submitting(t, routes, body); answer.Code < 400 {
			t.Errorf("%s answered %d", described, answer.Code)
		}
	}
}

// TestATransactionIsBounded. Every entry is a write inside one commit boundary,
// and this server holds one pooled connection per process — so an unbounded
// transaction is every other request in that process waiting for it.
func TestATransactionIsBounded(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	entries := make([]string, 0, maximumEntries+1)
	for range maximumEntries + 1 {
		entries = append(entries, entryOf("", "POST", "Organization", valid("Organization", nil)))
	}

	answer := submitting(t, routes, transactionOf(entries...))
	if answer.Code != http.StatusBadRequest {
		t.Errorf("a transaction of %d entries answered %d", len(entries), answer.Code)
	}

	if found := searchedOrganizations(t, routes); found != 0 {
		t.Errorf("%d organizations were written by a refused transaction", found)
	}
}

// searchedOrganizations counts what the caller can reach.
func searchedOrganizations(t *testing.T, routes http.Handler) int {
	t.Helper()

	return len(matched(t, searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Organization",
	})))
}

// readOnlyObservation returns the body of the one Observation that exists.
func readOnlyObservation(t *testing.T, routes http.Handler) string {
	t.Helper()

	found := matched(t, searchedBundle(t, routes, call{
		method: http.MethodGet, path: fhir.BasePath + "/Observation",
	}))

	if len(found) != 1 {
		t.Fatalf("%d observations exist", len(found))
	}

	answer := call{
		method: http.MethodGet, path: resourcePath("Observation", found[0]),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	return answer.Body.String()
}

// TestATransactionIsDeclared, so a client discovers it rather than guessing. R4
// declares it on the server and not on a type, because a transaction is not an
// interaction on a Patient.
func TestATransactionIsDeclared(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{method: http.MethodGet, path: fhir.BasePath + "/metadata"}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var statement fhir.CapabilityStatement
	if err := json.Unmarshal(answer.Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode the statement: %v", err)
	}

	declared := false

	for _, held := range statement.Rest[0].Interaction {
		if held.Code == fhir.InteractionTransaction {
			declared = true
		}
	}

	if !declared {
		t.Fatalf("the statement declares %v at the server level", statement.Rest[0].Interaction)
	}

	// And no resource claims it, because it is not an interaction on one.
	for _, resource := range statement.Rest[0].Resource {
		for _, held := range resource.Interaction {
			if held.Code == fhir.InteractionTransaction {
				t.Fatalf("%s claims the transaction interaction", resource.Type)
			}
		}
	}
}

// TestATransactionRecordsEveryEntryItPerformed. A Bundle is several
// interactions, so it is several records: one row saying "a transaction
// happened" would answer nothing an incident asks.
func TestATransactionRecordsEveryEntryItPerformed(t *testing.T) {
	routes, db := auditingServer(t)

	assertStatus(t, submitting(t, routes, transactionOf(
		entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
		entryOf("urn:uuid:two", "POST", "Organization", valid("Organization", nil)),
	)), http.StatusOK)

	var written, aboutNothing int

	for _, event := range recorded(t, db) {
		switch event.resourceType.String {
		case "Organization":
			written++
		case "":
			aboutNothing++
		}
	}

	if written != 2 {
		t.Errorf("a transaction of two writes recorded %d of them", written)
	}

	// And one row for the transaction itself, naming no resource — because it
	// is about the server rather than about any one of them.
	if aboutNothing != 1 {
		t.Errorf("%d rows name no resource, want the transaction's own", aboutNothing)
	}
}

// TestATransactionThatFailsRecordsNothingEither. The record and the write share
// a commit boundary, so a transaction that rolled away leaves no trace of
// writes that did not happen.
func TestATransactionThatFailsRecordsNothingEither(t *testing.T) {
	routes, db := auditingServer(t)

	answer := submitting(t, routes, transactionOf(
		entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
		entryOf("urn:uuid:two", "POST", "Observation", `{"resourceType":"Observation"}`),
	))

	if answer.Code < 400 {
		t.Fatalf("a failing transaction answered %d", answer.Code)
	}

	for _, event := range recorded(t, db) {
		if event.outcome == "allowed" && event.resourceType.String == "Organization" {
			t.Error("a write that rolled away was recorded as allowed")
		}
	}
}

// TestADeleteRunsBeforeACreateInTheSameTransaction, which is what lets one
// transaction replace a resource under an identity it also frees.
func TestADeleteRunsBeforeACreateInTheSameTransaction(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")

	// Submitted create-first, performed delete-first.
	answer := submitting(t, routes, transactionOf(
		entryOf("", "PUT", "Organization/"+id, valid("Organization",
			map[string]string{"id": `"` + id + `"`, "name": `"the replacement"`})),
		entryOf("", "DELETE", "Organization/"+id, ""),
	))

	assertStatus(t, answer, http.StatusOK)

	// The delete happened first, so the resource exists and holds what the PUT
	// stated rather than being a tombstone.
	read := call{method: http.MethodGet, path: resourcePath("Organization", id)}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	if !strings.Contains(read.Body.String(), "the replacement") {
		t.Errorf("the resource reads %s", read.Body)
	}
}

// TestAnUpdateInATransactionReplacesWhatIsThere. Update-as-create is what a PUT
// does at an identity nothing holds; at one that is held it is an update, and
// the entry has to say so — 200 with the next version, not 201 with a resource
// the client is told was created.
func TestAnUpdateInATransactionReplacesWhatIsThere(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	id := assertCreate(t, routes, "Organization")

	answer := submitting(t, routes, transactionOf(
		entryOf("", "PUT", "Organization/"+id, valid("Organization",
			map[string]string{"id": `"` + id + `"`, "name": `"the second name"`})),
	))

	assertStatus(t, answer, http.StatusOK)

	entries := responseBundle(t, answer).Entry
	if len(entries) != 1 || entries[0].Response == nil {
		t.Fatalf("an update entry described nothing: %s", answer.Body)
	}

	if status := entries[0].Response.Status; !strings.HasPrefix(status, "200") {
		t.Errorf("an update onto a resource that exists answered %q", status)
	}

	// The second version, so the row was replaced rather than written over.
	if etag := entries[0].Response.ETag; etag != weakETag("2") {
		t.Errorf("it reports version %q, want %q", etag, weakETag("2"))
	}

	read := call{method: http.MethodGet, path: resourcePath("Organization", id)}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	if !strings.Contains(read.Body.String(), "the second name") {
		t.Errorf("the resource still reads %s", read.Body)
	}
}

// TestATransactionEntryStatesNoPrecondition. R4 lets an entry say "only if this
// is not already here" or "only if it is still at this version". This build
// performs no conditional interaction anywhere, and a precondition quietly
// dropped is a client that asked for an idempotent write, was given an
// unconditional one, and is told nothing about the difference.
func TestATransactionEntryStatesNoPrecondition(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for _, stated := range []string{"ifMatch", "ifNoneExist", "ifNoneMatch", "ifModifiedSince"} {
		answer := submitting(t, routes,
			`{"resourceType":"Bundle","type":"transaction","entry":[{`+
				`"resource":`+valid("Organization", nil)+`,`+
				`"request":{"method":"POST","url":"Organization","`+stated+`":"anything"}}]}`)

		assertIssue(t, answer, http.StatusBadRequest, fhir.CodeNotSupported)
	}

	// And nothing was written: the Bundle is refused before any entry runs.
	if found := searchedOrganizations(t, routes); found != 0 {
		t.Errorf("a refused transaction wrote %d Organization(s)", found)
	}
}

// TestATransactionEntryCreatesOnlyWhatIsNotThere.
//
// `ifNoneExist` inside a bundle is the conditional create, and it settles
// before anything is written: an entry that matched has an identity, so every
// other entry referring to it points at the resource that is there rather than
// one made beside it.
func TestATransactionEntryCreatesOnlyWhatIsNotThere(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	organization := `{"resourceType":"Organization","name":"the only one",` +
		`"identifier":[{"system":"http://example.test/ids","value":"once"}]}`
	condition := "identifier=http://example.test/ids|once"

	bundle := `{"resourceType":"Bundle","type":"transaction","entry":[` +
		`{"fullUrl":"urn:uuid:org","resource":` + organization +
		`,"request":{"method":"POST","url":"Organization","ifNoneExist":"` + condition + `"}}` +
		`]}`

	assertStatus(t, submitting(t, routes, bundle), http.StatusOK)
	assertStatus(t, submitting(t, routes, bundle), http.StatusOK)

	if found := searchedOrganizations(t, routes); found != 1 {
		t.Errorf("%d Organizations exist after the same bundle twice", found)
	}
}

// TestATransactionResolvesAReferenceWrittenAsASearch.
//
// A bundle from another system points at resources by the identifiers that
// system knows, never by the ids this one minted. R4 writes that as
// `Patient?identifier=…`, and it is resolved before anything is stored: a
// reference to a search is one no reader could follow.
func TestATransactionResolvesAReferenceWrittenAsASearch(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	// The organization the reference will identify.
	existing := namedOrganization(t, routes, "target", "the one referred to")

	bundle := transactionOf(entryOf("", "POST", "Organization",
		`{"resourceType":"Organization","name":"the one referring",`+
			`"partOf":{"reference":"Organization?identifier=http://example.test/ids|target"}}`))

	answer := submitting(t, routes, bundle)
	assertStatus(t, answer, http.StatusOK)

	entries := responseBundle(t, answer).Entry
	if len(entries) != 1 || entries[0].Response == nil {
		t.Fatalf("the transaction answered %s", answer.Body)
	}

	id := lastSegmentOf(entries[0].FullURL)

	read := call{method: http.MethodGet, path: resourcePath("Organization", id)}.send(t, routes)
	assertStatus(t, read, http.StatusOK)

	if !strings.Contains(read.Body.String(), `"reference":"Organization/`+existing+`"`) {
		t.Errorf("the reference was stored as %s", read.Body)
	}
}

// TestAConditionalReferenceMatchingNothingFailsTheTransaction, because the
// entry said "the resource this identifies" and there is none: storing it would
// store a reference to a search.
func TestAConditionalReferenceMatchingNothingFailsTheTransaction(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := submitting(t, routes, transactionOf(entryOf("", "POST", "Organization",
		`{"resourceType":"Organization","name":"the one referring",`+
			`"partOf":{"reference":"Organization?identifier=http://example.test/ids|nobody"}}`)))

	if answer.Code < 400 {
		t.Errorf("a reference matching nothing answered %d", answer.Code)
	}

	if found := searchedOrganizations(t, routes); found != 0 {
		t.Errorf("a failed transaction wrote %d Organization(s)", found)
	}
}

// lastSegmentOf reads the id off a full URL.
func lastSegmentOf(held string) string {
	parts := strings.Split(strings.TrimSuffix(held, "/"), "/")

	return parts[len(parts)-1]
}
