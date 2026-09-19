package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/Ilavrita/Ilavrita/packages/terminology"
)

// aLoadedServer wires a server that holds one small code system, which is what
// a deployment has after importing its own licensed release.
func aLoadedServer(t *testing.T) http.Handler {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	serving.terminology = sqlite.NewTerminologyStore(db)

	const table = `"LOINC_NUM","LONG_COMMON_NAME","SHORTNAME","STATUS"
"2339-0","Glucose [Mass/volume] in Blood","Glucose Bld-mCnc","ACTIVE"
"99992-2","A measurement nobody makes any more","Old","DEPRECATED"
`

	if _, err := serving.terminology.Import(t.Context(),
		terminology.NewLOINCRelease(strings.NewReader(table), "2.77"),
		"Loinc.csv", time.Now()); err != nil {
		t.Fatalf("load the release: %v", err)
	}

	return fhirRoutes(t)
}

// parametersOf reads the Parameters an operation answered.
func parametersOf(t *testing.T, body []byte) map[string]fhir.Parameter {
	t.Helper()

	var held fhir.Parameters
	if err := json.Unmarshal(body, &held); err != nil {
		t.Fatalf("decode the parameters: %v (%s)", err, body)
	}

	if held.ResourceType != "Parameters" {
		t.Fatalf("the answer is a %s", held.ResourceType)
	}

	named := map[string]fhir.Parameter{}
	for _, parameter := range held.Parameter {
		named[parameter.Name] = parameter
	}

	return named
}

// TestALoadedCodeResolvesLocally, which is the whole point of importing a
// release: a deployment that loaded LOINC resolves a LOINC code without asking
// anybody.
func TestALoadedCodeResolvesLocally(t *testing.T) {
	routes := aLoadedServer(t)

	answer := call{
		method: http.MethodGet,
		path:   fhir.BasePath + "/CodeSystem/$lookup?system=http://loinc.org&code=2339-0",
	}.send(t, routes)

	assertStatus(t, answer, http.StatusOK)

	held := parametersOf(t, answer.Body.Bytes())
	if held["display"].ValueString != "Glucose [Mass/volume] in Blood" {
		t.Errorf("it resolves to %q", held["display"].ValueString)
	}

	if held["inactive"].ValueBoolean == nil || *held["inactive"].ValueBoolean {
		t.Errorf("an active code reads inactive=%v", held["inactive"].ValueBoolean)
	}
}

// TestASystemNobodyLoadedIsADifferentAnswerFromAWrongCode. Telling a client
// their code is wrong, when it is this install that is empty, sends them to fix
// something that is not broken.
func TestASystemNobodyLoadedIsADifferentAnswerFromAWrongCode(t *testing.T) {
	routes := aLoadedServer(t)

	// SNOMED was never imported here.
	notHeld := call{
		method: http.MethodGet,
		path: fhir.BasePath +
			"/CodeSystem/$lookup?system=http://snomed.info/sct&code=73211009",
	}.send(t, routes)

	if notHeld.Code != http.StatusNotImplemented {
		t.Errorf("a system nobody loaded answered %d: %s", notHeld.Code, notHeld.Body)
	}

	if !strings.Contains(notHeld.Body.String(), "licensed") {
		t.Errorf("the answer does not say why: %s", notHeld.Body)
	}

	// LOINC is loaded, and this code is genuinely not in it.
	wrong := call{
		method: http.MethodGet,
		path:   fhir.BasePath + "/CodeSystem/$lookup?system=http://loinc.org&code=00000-0",
	}.send(t, routes)

	if wrong.Code != http.StatusNotFound {
		t.Errorf("a code nobody loaded answered %d", wrong.Code)
	}
}

// TestValidateCodeAnswersWhicheverWayItWent. R4 answers a Parameters holding
// `result`: the operation was performed, and what it found is the answer.
func TestValidateCodeAnswersWhicheverWayItWent(t *testing.T) {
	routes := aLoadedServer(t)

	for described, held := range map[string]struct {
		code string
		want bool
	}{
		"a code that is in the system": {"2339-0", true},
		"a code that is not":           {"00000-0", false},
		"a retired code that is in it": {"99992-2", true},
	} {
		answer := call{
			method: http.MethodGet,
			path: fhir.BasePath + "/CodeSystem/$validate-code?system=http://loinc.org&code=" +
				held.code,
		}.send(t, routes)

		assertStatus(t, answer, http.StatusOK)

		result := parametersOf(t, answer.Body.Bytes())["result"]
		if result.ValueBoolean == nil || *result.ValueBoolean != held.want {
			t.Errorf("%s answered %v, want %v", described, result.ValueBoolean, held.want)
		}
	}
}

// TestALookupNamesBothOrIsRefused.
func TestALookupNamesBothOrIsRefused(t *testing.T) {
	routes := aLoadedServer(t)

	for _, asked := range []string{
		"", "?system=http://loinc.org", "?code=2339-0", "?system=&code=2339-0",
	} {
		answer := call{
			method: http.MethodGet, path: fhir.BasePath + "/CodeSystem/$lookup" + asked,
		}.send(t, routes)

		if answer.Code != http.StatusBadRequest {
			t.Errorf("a lookup %q answered %d", asked, answer.Code)
		}
	}
}

// TestALookupIsAuthorizedLikeAnyRead, so the directory is not a way around the
// decision every other route rests on.
func TestALookupIsAuthorizedLikeAnyRead(t *testing.T) {
	routes := servingFHIR(t, writeActions)

	serving.terminology = nil

	assertStatus(t, call{
		method:    http.MethodGet,
		path:      fhir.BasePath + "/CodeSystem/$lookup?system=http://loinc.org&code=2339-0",
		anonymous: true,
	}.send(t, routes), http.StatusUnauthorized)
}

// TestAnInstallHoldingNothingSaysSo rather than reporting every code as wrong.
func TestAnInstallHoldingNothingSaysSo(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	serving.terminology = nil

	assertStatus(t, call{
		method: http.MethodGet,
		path:   fhir.BasePath + "/CodeSystem/$lookup?system=http://loinc.org&code=2339-0",
	}.send(t, routes), http.StatusNotImplemented)
}
