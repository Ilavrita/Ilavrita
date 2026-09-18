package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// aPDF is a payload that is plainly not JSON, so what happens to it is what
// happens to a document rather than to a resource.
const aPDF = "%PDF-1.7\nthis is not a resource\n%%EOF"

// postPayload submits raw bytes as a Binary.
func postPayload(t *testing.T, routes http.Handler, media, body, governs string) *httptest.ResponseRecorder {
	t.Helper()

	return call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: body, contentType: media, securityContext: governs,
	}.send(t, routes)
}

// TestARawPayloadIsStoredAsTheDocumentItIs.
func TestARawPayloadIsStoredAsTheDocumentItIs(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	// Read as FHIR: the resource says what the bytes are, and carries them.
	asResource := call{
		method: http.MethodGet, path: resourcePath("Binary", id), accept: fhir.ContentType,
	}.send(t, routes)

	assertStatus(t, asResource, http.StatusOK)

	var held struct {
		ResourceType string `json:"resourceType"`
		ContentType  string `json:"contentType"`
		Data         string `json:"data"`
	}

	if err := json.Unmarshal(asResource.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the Binary: %v", err)
	}

	if held.ResourceType != "Binary" || held.ContentType != "application/pdf" {
		t.Errorf("the resource is %+v", held)
	}

	decoded, err := base64.StdEncoding.DecodeString(held.Data)
	if err != nil {
		t.Fatalf("decode the data member: %v", err)
	}

	if string(decoded) != aPDF {
		t.Errorf("the data member holds %q", decoded)
	}
}

// TestAPayloadComesBackAsItsOwnMediaTypeWhenAskedFor, which is the whole point
// of the route: a client that wants the document gets the document.
func TestAPayloadComesBackAsItsOwnMediaTypeWhenAskedFor(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	for _, accept := range []string{"application/pdf", "application/*"} {
		answer := call{
			method: http.MethodGet, path: resourcePath("Binary", id), accept: accept,
		}.send(t, routes)

		assertStatus(t, answer, http.StatusOK)

		if got := answer.Body.String(); got != aPDF {
			t.Errorf("Accept %s answered %q", accept, got)
		}

		if got := answer.Header().Get(contentTypeField); got != "application/pdf" {
			t.Errorf("Accept %s answered content type %q", accept, got)
		}
	}
}

// TestAWildcardStillAnswersAResource. This server answers JSON for */*
// everywhere else, and a route that read a wildcard as "give me the document"
// would hand a browser a download where every other route hands it a resource.
func TestAWildcardStillAnswersAResource(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	answer := call{
		method: http.MethodGet, path: resourcePath("Binary", resourceID(t, created)), accept: "*/*",
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if got := answer.Header().Get(contentTypeField); got != fhir.ContentType {
		t.Errorf("*/* answered %q, want the resource", got)
	}
}

// TestAPayloadIsNotInTheRow. Putting megabytes in the row makes every read of
// the metadata pay for them and every backup of the database carry them.
func TestAPayloadIsNotInTheRow(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)

	created := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	var content string
	if err := db.QueryRowContext(t.Context(),
		"SELECT content FROM fhir_resource WHERE res_type = 'Binary'").Scan(&content); err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if strings.Contains(content, "PDF") || strings.Contains(content, base64.StdEncoding.EncodeToString([]byte(aPDF))) {
		t.Errorf("the row carries the payload: %s", content)
	}

	if !strings.Contains(content, "application/pdf") {
		t.Errorf("the row does not say what the payload is: %s", content)
	}
}

// TestAPayloadSubmittedAsAResourceLandsInTheSamePlace, so there is one place a
// Binary's bytes live however they arrived.
func TestAPayloadSubmittedAsAResourceLandsInTheSamePlace(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: `{"resourceType":"Binary","contentType":"text/plain","data":"` +
			base64.StdEncoding.EncodeToString([]byte("a note")) +
			`","securityContext":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, created, http.StatusCreated)

	var content string
	if err := db.QueryRowContext(t.Context(),
		"SELECT content FROM fhir_resource WHERE res_type = 'Binary'").Scan(&content); err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if strings.Contains(content, "data") {
		t.Errorf("the row kept the data member: %s", content)
	}

	// And it reads back as its own media type.
	answer := call{
		method: http.MethodGet, path: resourcePath("Binary", resourceID(t, created)),
		accept: "text/plain",
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if answer.Body.String() != "a note" {
		t.Errorf("read back %q", answer.Body.String())
	}
}

// TestAPayloadNamingNoAccessContextIsRefused. An unattributed blob in a
// clinical server is a document nobody can say whose it is.
func TestAPayloadNamingNoAccessContextIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertIssue(t, postPayload(t, routes, "application/pdf", aPDF, ""),
		http.StatusForbidden, fhir.CodeForbidden)

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: `{"resourceType":"Binary","contentType":"text/plain","data":"aGk="}`,
	}.send(t, routes), http.StatusForbidden, fhir.CodeForbidden)
}

// TestAPayloadReachesNoOtherPatient, which is what placing it by its access
// context is for.
func TestAPayloadReachesNoOtherPatient(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertIssue(t, postPayload(t, routes, "application/pdf", aPDF, "Patient/someone-else"),
		http.StatusForbidden, fhir.CodeForbidden)
}

// TestAPayloadStatesWhatItIs. What a client called the bytes is what this
// server hands back, so there is nothing to hand back without it.
func TestAPayloadStatesWhatItIs(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: `{"resourceType":"Binary","data":"aGk=","securityContext":{"reference":"Patient/` +
			string(conformancePatient) + `"}}`,
	}.send(t, routes), http.StatusBadRequest, fhir.CodeInvalid)

	assertIssue(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: `{"resourceType":"Binary","contentType":"text/plain","data":"not base64!",` +
			`"securityContext":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes), http.StatusBadRequest, fhir.CodeInvalid)
}

// TestAnUnidentifiedCallerStoresNoPayload, and is refused before anything
// reaches the disk.
func TestAnUnidentifiedCallerStoresNoPayload(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: aPDF, contentType: "application/pdf", anonymous: true,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusUnauthorized)
}

// TestAnAcceptThisServerCannotSatisfyIsRefused. Answering the resource to a
// caller who said they could not read it is answering something nobody asked
// for.
func TestAnAcceptThisServerCannotSatisfyIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	assertIssue(t, call{
		method: http.MethodGet, path: resourcePath("Binary", resourceID(t, created)),
		accept: "image/png",
	}.send(t, routes), http.StatusNotAcceptable, fhir.CodeNotSupported)
}

// TestEachVersionServesItsOwnPayload. An updated Binary must not answer with
// bytes its earlier version never held.
func TestEachVersionServesItsOwnPayload(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "text/plain", "the first",
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	// The current version reads as what was written.
	answer := call{
		method: http.MethodGet, path: resourcePath("Binary", id), accept: "text/plain",
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	if answer.Body.String() != "the first" {
		t.Errorf("read back %q", answer.Body.String())
	}
}

// TestADeploymentWithNowhereToPutPayloadsSaysSo, rather than accepting a
// document it has nowhere to keep.
func TestADeploymentWithNowhereToPutPayloadsSaysSo(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	serving.payloads = nil

	assertIssue(t, postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient)), http.StatusNotImplemented, fhir.CodeNotSupported)
}

// TestAPayloadPastTheLimitIsRefused, and nothing is stored for it.
func TestAPayloadPastTheLimitIsRefused(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)

	// A body past what a JSON resource may carry, submitted as a document.
	oversized := strings.Repeat("x", maximumPayloadBytes+1)

	answer := postPayload(t, routes, "application/octet-stream", oversized,
		"Patient/"+string(conformancePatient))

	if answer.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", answer.Code)
	}

	assertStoredCount(t, db, "Binary", 0)
}

// TestAVersionReadServesItsOwnPayload. A Binary's history answering with a
// resource that describes bytes it does not carry is a record of something
// nobody can read.
func TestAVersionReadServesItsOwnPayload(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := postPayload(t, routes, "text/plain", "the first",
		"Patient/"+string(conformancePatient))
	assertStatus(t, created, http.StatusCreated)

	id := resourceID(t, created)

	replaced := call{
		method: http.MethodPut, path: resourcePath("Binary", id),
		body: `{"resourceType":"Binary","id":"` + id + `","contentType":"text/plain","data":"` +
			base64.StdEncoding.EncodeToString([]byte("the second")) +
			`","securityContext":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, replaced, http.StatusOK)

	// The current version is the new bytes.
	current := call{
		method: http.MethodGet, path: resourcePath("Binary", id), accept: "text/plain",
	}.send(t, routes)

	assertStatus(t, current, http.StatusOK)

	if current.Body.String() != "the second" {
		t.Errorf("the current version reads %q", current.Body.String())
	}

	// And the first version still serves what it held.
	first := call{
		method: http.MethodGet, path: resourcePath("Binary", id) + "/_history/1", accept: "text/plain",
	}.send(t, routes)

	assertStatus(t, first, http.StatusOK)

	if first.Body.String() != "the first" {
		t.Errorf("version 1 reads %q, want the bytes it was written with", first.Body.String())
	}
}
