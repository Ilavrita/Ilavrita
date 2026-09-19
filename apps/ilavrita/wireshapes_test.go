package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// wireShapeDirectory is where the shapes this server puts on the wire are
// written, for an independent validator to judge.
//
// The Go suite here asserts behaviour against its own understanding of R4, which
// is the understanding that wrote the server: it cannot catch the two of them
// being wrong together. scripts/conformance.sh runs the HL7 validator over what
// this writes, which is a second implementation reading the same bytes.
const wireShapeDirectory = "ILAVRITA_WIRE_DIR"

// TestWireShapesAreCaptured writes one file per distinct response shape this
// server produces, for scripts/conformance.sh to validate. It is skipped in an
// ordinary run, because writing files is not an assertion.
func TestWireShapesAreCaptured(t *testing.T) {
	into := os.Getenv(wireShapeDirectory)
	if into == "" {
		t.Skip("set " + wireShapeDirectory + " to capture; scripts/conformance.sh does")
	}

	// Every write stays under the directory that was named, which is what keeps
	// a shape's name from reaching anywhere else on the disk.
	root, err := os.OpenRoot(into)
	if err != nil {
		t.Fatalf("open %s: %v", into, err)
	}

	t.Cleanup(func() { _ = root.Close() })

	routes := servingFHIR(t, everyAction)

	organization := assertCreate(t, routes, "Organization")
	observation := assertCreate(t, routes, "Observation")

	// The profile each shape is judged against. A Bundle is a Bundle whatever
	// it carries, and $validate answers an OperationOutcome.
	for _, shape := range []struct {
		name, profile string
		sent          call
	}{
		{"read-organization", "Organization", call{
			method: http.MethodGet, path: resourcePath("Organization", organization)}},
		{"read-observation", "Observation", call{
			method: http.MethodGet, path: resourcePath("Observation", observation)}},
		{"vread-organization", "Organization", call{
			method: http.MethodGet, path: resourcePath("Organization", organization) + "/_history/1"}},
		{"searchset-bundle", "Bundle", call{
			method: http.MethodGet, path: fhir.BasePath + "/Organization"}},
		{"searchset-bundle-posted", "Bundle", call{
			method: http.MethodPost, path: fhir.BasePath + "/Organization/_search",
			body: "_id=" + organization, contentType: formContentType}},
		{"history-bundle", "Bundle", call{
			method: http.MethodGet, path: resourcePath("Organization", organization) + "/_history"}},
		{"transaction-response", "Bundle", call{
			method: http.MethodPost, path: fhir.BasePath,
			body: transactionOf(
				entryOf("urn:uuid:one", "POST", "Organization", valid("Organization", nil)),
				entryOf("", "PUT", "Organization/"+organization,
					valid("Organization", map[string]string{"id": `"` + organization + `"`})),
			)}},
		{"validate-outcome", "OperationOutcome", call{
			method: http.MethodPost, path: fhir.BasePath + "/Organization/$validate",
			body: valid("Organization", nil)}},
		{"validate-outcome-refused", "OperationOutcome", call{
			method: http.MethodPost, path: fhir.BasePath + "/Organization/$validate",
			body: `{"resourceType":"Organization","name":[]}`}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			answer := shape.sent.send(t, routes)

			if answer.Body.Len() == 0 {
				t.Fatalf("%s answered %d with no body", shape.name, answer.Code)
			}

			written := shape.name + "." + shape.profile + ".json"

			held, err := root.Create(written)
			if err != nil {
				t.Fatalf("create %s: %v", written, err)
			}

			_, err = held.Write(answer.Body.Bytes())

			if closed := held.Close(); err == nil {
				err = closed
			}

			if err != nil {
				t.Fatalf("write %s: %v", written, err)
			}
		})
	}
}

// TestEveryServedTypeIsCaptured writes back one instance of every type this
// server serves.
//
// Two of the bugs the validator found were on the two types somebody happened
// to pick. The other hundred and twenty are written by the same handlers
// through the same projection, so there is no reason to believe they are
// right — only that nobody has looked.
func TestEveryServedTypeIsCaptured(t *testing.T) {
	into := os.Getenv(wireShapeDirectory)
	if into == "" {
		t.Skip("set " + wireShapeDirectory + " to capture; scripts/conformance.sh does")
	}

	root, err := os.OpenRoot(into)
	if err != nil {
		t.Fatalf("open %s: %v", into, err)
	}

	t.Cleanup(func() { _ = root.Close() })

	var captured int

	for _, resourceType := range fhir.ServedResourceTypes() {
		t.Run(resourceType, func(t *testing.T) {
			routes := servingFHIR(t, everyAction)

			id, made := writtenInstance(t, routes, resourceType)
			if !made {
				// A type whose compartment is itself, other than the one the
				// suite's grant names. Reaching it needs a grant naming an id
				// that does not exist yet, which is the case
				// TestACompartmentSubjectIsCreatedByNamingIt covers.
				t.Skipf("%s mints a compartment this grant does not name", resourceType)
			}

			answer := call{
				method: http.MethodGet, path: resourcePath(resourceType, id),
			}.send(t, routes)
			assertStatus(t, answer, http.StatusOK)

			writeShape(t, root, "read-"+resourceType, resourceType, answer.Body.Bytes())
			captured++
		})
	}

	// The point is coverage, so a run that quietly captured a handful would be
	// worse than none: it would look like the whole surface had been judged.
	if captured < len(fhir.ServedResourceTypes())-2 {
		t.Errorf("captured %d of %d served types", captured, len(fhir.ServedResourceTypes()))
	}
}

// writtenInstance creates one resource of a type, whichever way that type is
// created, and reports whether this suite's grant could.
func writtenInstance(t *testing.T, routes http.Handler, resourceType string) (string, bool) {
	t.Helper()

	if !mintsItsOwnCompartment(t, resourceType) {
		return assertCreate(t, routes, resourceType), true
	}

	// A type that is its own compartment is provisioned by naming the id the
	// grant already holds, which is what a client has to do as well.
	id := string(conformancePatient)

	answer := call{
		method: http.MethodPut,
		path:   resourcePath(resourceType, id),
		body:   replacement(resourceType, id),
	}.send(t, routes)

	if answer.Code != http.StatusCreated && answer.Code != http.StatusOK {
		return "", false
	}

	return id, true
}

// writeShape stores one captured body under the name the validator reads its
// profile from.
func writeShape(t *testing.T, root *os.Root, label, profile string, body []byte) {
	t.Helper()

	written := label + "." + profile + ".json"

	held, err := root.Create(written)
	if err != nil {
		t.Fatalf("create %s: %v", written, err)
	}

	_, err = held.Write(body)

	if closed := held.Close(); err == nil {
		err = closed
	}

	if err != nil {
		t.Fatalf("write %s: %v", written, err)
	}
}

// TestTheBespokeShapesAreCaptured writes the ones the loop above reaches only
// through the generic fixture path.
//
// Binary, DocumentReference, Subscription and SearchParameter each have a route
// that does something particular with them — raw bytes, an attachment naming a
// Binary, a criteria that is parsed, an expression that is compiled. What those
// routes put on the wire is not what a generic fixture round-trips.
func TestTheBespokeShapesAreCaptured(t *testing.T) {
	into := os.Getenv(wireShapeDirectory)
	if into == "" {
		t.Skip("set " + wireShapeDirectory + " to capture; scripts/conformance.sh does")
	}

	root, err := os.OpenRoot(into)
	if err != nil {
		t.Fatalf("open %s: %v", into, err)
	}

	t.Cleanup(func() { _ = root.Close() })

	routes := servingFHIR(t, everyAction)
	subject := "Patient/" + string(conformancePatient)

	// A Binary written as the document it is, read back as the resource.
	stored := postPayload(t, routes, "application/pdf", aPDF, subject)
	assertStatus(t, stored, http.StatusCreated)

	binary := resourceID(t, stored)

	read := call{
		method: http.MethodGet, path: resourcePath("Binary", binary),
		accept: fhir.ContentType,
	}.send(t, routes)
	assertStatus(t, read, http.StatusOK)
	writeShape(t, root, "binary-from-payload", "Binary", read.Body.Bytes())

	// A DocumentReference whose attachment names that Binary, which is the only
	// way this server accepts one.
	document := call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference",
		body: aDocument(`"contentType":"application/pdf","url":"Binary/` + binary + `"`),
	}.send(t, routes)
	assertStatus(t, document, http.StatusCreated)
	writeShape(t, root, "document-naming-a-binary", "DocumentReference", document.Body.Bytes())

	// A Subscription whose criteria this server actually parsed.
	watching := call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription",
		body: `{"resourceType":"Subscription","status":"active","reason":"a captured shape",` +
			`"criteria":"Observation?status=final",` +
			`"channel":{"type":"rest-hook","endpoint":"https://example.com/hook"}}`,
	}.send(t, routes)
	assertStatus(t, watching, http.StatusCreated)
	writeShape(t, root, "subscription-active", "Subscription", watching.Body.Bytes())

	// A SearchParameter this server compiled rather than merely stored.
	defined := call{
		method: http.MethodPost, path: fhir.BasePath + "/SearchParameter",
		body: `{"resourceType":"SearchParameter","url":"http://example.com/sp/phone",` +
			`"name":"phone","status":"active","description":"a captured shape",` +
			`"code":"phone","type":"token","base":["Organization"],` +
			`"expression":"Organization.telecom"}`,
	}.send(t, routes)
	assertStatus(t, defined, http.StatusCreated)
	writeShape(t, root, "searchparameter-compiled", "SearchParameter", defined.Body.Bytes())

	captureRefusals(t, root, routes)
}

// captureRefusals writes one OperationOutcome per refusal a client can provoke.
//
// Every refusal renders through one constructor, and TestEveryIssueCodeIsOneR4Defines
// is what holds the codes to R4's own set. These are the bodies themselves,
// for the shapes a single caller can actually reach: a 409 needs a second
// writer racing this one, which is not a thing to contrive here.
func captureRefusals(t *testing.T, root *os.Root, routes http.Handler) {
	t.Helper()

	gone := assertCreate(t, routes, "Organization")
	assertStatus(t, call{
		method: http.MethodDelete, path: resourcePath("Organization", gone),
	}.send(t, routes), http.StatusNoContent)

	held := assertCreate(t, routes, "Organization")

	for _, shape := range []struct {
		name string
		sent call
		want int
	}{
		{"refusal-404-unknown-id", call{
			method: http.MethodGet, path: resourcePath("Organization", "nothing-here"),
		}, http.StatusNotFound},
		{"refusal-404-unknown-type", call{
			method: http.MethodGet, path: fhir.BasePath + "/NoSuchType/x",
		}, http.StatusNotFound},
		{"refusal-410-deleted", call{
			method: http.MethodGet, path: resourcePath("Organization", gone),
		}, http.StatusGone},
		{"refusal-412-stale", call{
			method: http.MethodPut, path: resourcePath("Organization", held),
			body: replacement("Organization", held), ifMatch: `W/"99"`,
		}, http.StatusPreconditionFailed},
	} {
		answer := shape.sent.send(t, routes)

		if answer.Code != shape.want {
			t.Errorf("%s answered %d, want %d: %s", shape.name, answer.Code, shape.want, answer.Body)

			continue
		}

		writeShape(t, root, shape.name, "OperationOutcome", answer.Body.Bytes())
	}
}
