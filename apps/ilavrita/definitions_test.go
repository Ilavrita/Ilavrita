package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// definedServer wires a server whose FHIR base definitions have been seeded,
// which is what startup does before the first request.
func definedServer(t *testing.T) http.Handler {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	serving.definitions = sqlite.NewCanonicalStore(db)

	if err := seedDefinitions(t.Context(), serving.definitions); err != nil {
		t.Fatalf("seed the definitions: %v", err)
	}

	return fhirRoutes(t)
}

// TestADefinitionIsReadableOverTheAPI. Seeding them is only worth something if a
// client can ask what a resource is; this is the route that answers.
func TestADefinitionIsReadableOverTheAPI(t *testing.T) {
	routes := definedServer(t)

	answer := call{
		method: http.MethodGet, path: resourcePath("StructureDefinition", "Observation"),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	var held struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		URL          string `json:"url"`
		Version      string `json:"version"`
		Kind         string `json:"kind"`
	}

	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the definition: %v", err)
	}

	if held.ResourceType != "StructureDefinition" || held.ID != "Observation" {
		t.Errorf("the answer is %s/%s", held.ResourceType, held.ID)
	}

	if held.URL != "http://hl7.org/fhir/StructureDefinition/Observation" ||
		held.Version != conformance.Release || held.Kind != "resource" {
		t.Errorf("it reads %s %s (%s)", held.URL, held.Version, held.Kind)
	}
}

// TestEveryServedTypeCanBeAskedAbout. A CapabilityStatement that declares a type
// beside a definition nobody can read is one a client cannot act on.
func TestEveryServedTypeCanBeAskedAbout(t *testing.T) {
	routes := definedServer(t)

	for _, name := range fhir.ServedResourceTypes() {
		answer := call{
			method: http.MethodGet, path: resourcePath("StructureDefinition", name),
		}.send(t, routes)

		if answer.Code != http.StatusOK {
			t.Errorf("%s's definition answered %d", name, answer.Code)
		}
	}
}

// TestADefinitionIsStillAuthorized. It is the specification rather than
// anybody's data, and it is still reached through the same decision: a caller
// with no read of the type reads no definition either.
func TestADefinitionIsStillAuthorized(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	// Write and nothing else. writeActions carries a read with it, because a
	// write reads the row back, so it is not the absence of one.
	serveProject(t, db, homeProject, []storage.Action{storage.ActionWrite})

	serving.definitions = sqlite.NewCanonicalStore(db)

	if err := seedDefinitions(t.Context(), serving.definitions); err != nil {
		t.Fatalf("seed: %v", err)
	}

	routes := fhirRoutes(t)

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("StructureDefinition", "Observation"),
	}.send(t, routes), http.StatusForbidden)
}

// TestAProjectsOwnDefinitionWins. The Project's store is asked first, so a
// Project that wrote its own StructureDefinition for a type serves that one
// rather than the specification's.
func TestAProjectsOwnDefinitionWins(t *testing.T) {
	routes := definedServer(t)

	const theirs = `{"resourceType":"StructureDefinition","id":"Observation",` +
		`"url":"http://example.test/StructureDefinition/OurObservation",` +
		`"name":"OurObservation","status":"draft","kind":"resource","abstract":false,` +
		`"type":"Observation"}`

	assertStatus(t, call{
		method: http.MethodPut, path: resourcePath("StructureDefinition", "Observation"),
		body: theirs,
	}.send(t, routes), http.StatusCreated)

	answer := call{
		method: http.MethodGet, path: resourcePath("StructureDefinition", "Observation"),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	var held struct {
		URL string `json:"url"`
	}

	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if held.URL != "http://example.test/StructureDefinition/OurObservation" {
		t.Errorf("the Project's own definition reads %q", held.URL)
	}
}

// TestAnUnseededDeploymentAnswersAsItAlwaysDid, so a build that holds no
// definitions is a 404 rather than a failure.
func TestAnUnseededDeploymentAnswersAsItAlwaysDid(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	serving.definitions = nil

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("StructureDefinition", "Observation"),
	}.send(t, routes), http.StatusNotFound)
}

// TestADefinitionNobodySeededIsNotFound. The fallback answers what the store
// holds, and a seeded install holding no such definition is still a 404 — not an
// empty resource under the name that was asked for.
func TestADefinitionNobodySeededIsNotFound(t *testing.T) {
	routes := definedServer(t)

	for _, id := range []string{"NotARealDefinition", "observation", "Observation.status"} {
		answer := call{
			method: http.MethodGet, path: resourcePath("StructureDefinition", id),
		}.send(t, routes)

		if answer.Code != http.StatusNotFound {
			t.Errorf("StructureDefinition/%s answered %d: %s", id, answer.Code, answer.Body)
		}
	}
}

// TestSeedingAtStartupIsIdempotent, through the function startup calls. A server
// restarts far more often than the specification changes.
func TestSeedingAtStartupIsIdempotent(t *testing.T) {
	db := preparedDatabase(t)
	store := sqlite.NewCanonicalStore(db)

	for attempt := range 3 {
		if err := seedDefinitions(t.Context(), store); err != nil {
			t.Fatalf("seed %d: %v", attempt, err)
		}
	}

	held, err := store.Holding(t.Context())
	if err != nil {
		t.Fatalf("read what was seeded: %v", err)
	}

	if held.Digest != conformance.Digest() || held.Held < 200 {
		t.Errorf("three starts left %+v", held)
	}
}

// TestAConfinedCallerIsNotHandedTheDefinitions. A canonical resource belongs to
// no Project, so there is no row for a compartment to be evaluated against and a
// confined Grant cannot be said to admit it.
//
// What makes this worth asserting is that the Project's store answers ErrNotFound
// both for a row that is not there and for one this caller may not see — an id
// must not be probeable — so a fallback that read that answer would be handing
// resources out on an inference rather than a decision. Today it would hand out
// the published specification; the rule is what stops the next thing seeded
// there being handed out the same way.
func TestAConfinedCallerIsNotHandedTheDefinitions(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveUnder(t, db, homeProject, confinedReadPolicy(t, homeProject))

	serving.definitions = sqlite.NewCanonicalStore(db)

	if err := seedDefinitions(t.Context(), serving.definitions); err != nil {
		t.Fatalf("seed: %v", err)
	}

	routes := fhirRoutes(t)

	// The definitions are there — an unrestricted caller reads them — and this
	// caller does not get them.
	if _, found, err := serving.definitions.Read(
		t.Context(), "StructureDefinition", "Observation"); err != nil || !found {
		t.Fatalf("the install holds no definition to withhold: %v (found=%v)", err, found)
	}

	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("StructureDefinition", "Observation"),
	}.send(t, routes), http.StatusNotFound)
}

// TestTheTerminologyIsReadableToo. The value sets are what a code is judged
// against, so a client refused for one has somewhere to look it up.
func TestTheTerminologyIsReadableToo(t *testing.T) {
	routes := definedServer(t)

	for _, held := range []struct {
		resourceType string
		id           string
	}{
		{"ValueSet", "observation-status"},
		{"CodeSystem", "observation-status"},
		{"ValueSet", "administrative-gender"},
	} {
		answer := call{
			method: http.MethodGet, path: resourcePath(held.resourceType, held.id),
		}.send(t, routes)

		if answer.Code != http.StatusOK {
			t.Errorf("%s/%s answered %d", held.resourceType, held.id, answer.Code)

			continue
		}

		var read struct {
			ResourceType string `json:"resourceType"`
			URL          string `json:"url"`
		}

		if err := json.Unmarshal(answer.Body.Bytes(), &read); err != nil {
			t.Fatalf("decode %s: %v", held.id, err)
		}

		if read.ResourceType != held.resourceType || read.URL == "" {
			t.Errorf("it reads as %s at %q", read.ResourceType, read.URL)
		}
	}
}

// TestARefusedCodeNamesASetTheClientCanRead, which is what makes the refusal
// actionable rather than a dead end.
func TestARefusedCodeNamesASetTheClientCanRead(t *testing.T) {
	routes := definedServer(t)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: `{"resourceType":"Observation","status":"banana","code":{"text":"a reading"},` +
			`"subject":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	if !strings.Contains(answer.Body.String(), "observation-status") {
		t.Fatalf("the refusal names no set: %s", answer.Body)
	}

	// And that set is one this server serves.
	assertStatus(t, call{
		method: http.MethodGet, path: resourcePath("ValueSet", "observation-status"),
	}.send(t, routes), http.StatusOK)
}
