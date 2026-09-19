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
