package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// aDocument is one DocumentReference about the conformance patient, with
// whatever the caller puts in its single content entry.
func aDocument(attachment string) string {
	return `{"resourceType":"DocumentReference","status":"current"` +
		`,"subject":{"reference":"Patient/` + string(conformancePatient) + `"}` +
		`,"content":[{"attachment":{` + attachment + `}}]}`
}

// TestADocumentCarryingItsOwnBytesIsRefused. The bytes belong outside the row
// for the same reason a Binary's payload does, and R4 already says where: the
// attachment's url names the Binary holding them.
func TestADocumentCarryingItsOwnBytesIsRefused(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference",
		body: aDocument(`"contentType":"application/pdf","data":"aGVsbG8="`),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	if !strings.Contains(answer.Body.String(), "Binary") {
		t.Errorf("the refusal does not say where the bytes go: %s", answer.Body)
	}
}

// TestADocumentReferencingItsBytesIsAccepted, which is the shape that replaces
// the refused one: the document is a Binary, and the reference names it.
func TestADocumentReferencingItsBytesIsAccepted(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	held := postPayload(t, routes, "application/pdf", aPDF,
		"Patient/"+string(conformancePatient))
	assertStatus(t, held, http.StatusCreated)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference",
		body: aDocument(`"contentType":"application/pdf","url":"Binary/` +
			resourceID(t, held) + `"`),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)
}

// TestAnUpdateCannotInlineBytesEither. A create and an update state the same
// content, so a rule checked only on the way in would be one PUT away from
// being no rule at all.
func TestAnUpdateCannotInlineBytesEither(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	created := call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference",
		body: aDocument(`"contentType":"application/pdf","url":"Binary/somewhere"`),
	}.send(t, routes)

	assertStatus(t, created, http.StatusCreated)

	answer := call{
		method: http.MethodPut, path: resourcePath("DocumentReference", resourceID(t, created)),
		body: aDocument(`"contentType":"application/pdf","data":"aGVsbG8="`),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)
}

// TestADocumentIsRefusedWhicheverEntryCarriesTheBytes. A DocumentReference holds
// a list of content, so a check reading only the first would let the second
// through — and one attachment in the row is the whole problem.
func TestADocumentIsRefusedWhicheverEntryCarriesTheBytes(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	body := `{"resourceType":"DocumentReference","status":"current"` +
		`,"subject":{"reference":"Patient/` + string(conformancePatient) + `"}` +
		`,"content":[{"attachment":{"contentType":"text/plain","url":"Binary/first"}}` +
		`,{"attachment":{"contentType":"application/pdf","data":"aGVsbG8="}}]}`

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference", body: body,
	}.send(t, routes), http.StatusBadRequest)
}

// TestADocumentWithNoAttachmentBytesIsUntouched. The check is about inlined
// bytes and nothing else: an attachment stating what it is, or naming where the
// bytes are, is an ordinary resource.
//
// A DocumentReference with no content at all is not one of these cases. R4
// requires at least one, so it is refused before this rule is ever reached —
// which TestADocumentStatesWhatItRefersTo is what asserts.
func TestADocumentWithNoAttachmentBytesIsUntouched(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	for name, body := range map[string]string{
		"a url":            aDocument(`"contentType":"text/plain","url":"Binary/held"`),
		"nothing but type": aDocument(`"contentType":"text/plain"`),
	} {
		answer := call{
			method: http.MethodPost, path: fhir.BasePath + "/DocumentReference", body: body,
		}.send(t, routes)

		if answer.Code != http.StatusCreated {
			t.Errorf("%s answered %d: %s", name, answer.Code, answer.Body)
		}
	}
}

// TestThisRuleIsDocumentReferencesAlone. A Binary's whole purpose is to carry a
// data member. Every other type's attachments still land in the row, bounded
// only by what one request body may be — Media here stands for all of them. That
// is a limitation rather than a decision, and it is recorded as one: what makes
// DocumentReference different is that R4 gave its attachment a url for naming
// the document, so there is somewhere to send a client.
func TestOnlyADocumentReferenceIsCheckedForInlinedBytes(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Binary",
		body: `{"resourceType":"Binary","contentType":"application/pdf","data":"aGVsbG8="` +
			`,"securityContext":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes), http.StatusCreated)

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Media",
		body: `{"resourceType":"Media","status":"completed"` +
			`,"subject":{"reference":"Patient/` + string(conformancePatient) + `"}` +
			`,"content":{"contentType":"image/png","data":"aGVsbG8="}}`,
	}.send(t, routes), http.StatusCreated)
}

// TestTheRuleReadsTheTypeAndNotTheShape. A body shaped like a DocumentReference
// under another type's name is that type's resource, whatever it looks like:
// this rule is about what DocumentReference means, not about any list called
// content.
func TestTheRuleReadsTheTypeAndNotTheShape(t *testing.T) {
	body := json.RawMessage(aDocument(`"contentType":"application/pdf","data":"aGVsbG8="`))

	if err := refuseInlineAttachment(
		storage.ResourceKey{Type: documentType, ID: "one"}, body); !errors.Is(err, errInlineAttachment) {
		t.Errorf("a DocumentReference carrying bytes answered %v", err)
	}

	for _, other := range []storage.ResourceType{"Media", "Communication", "Binary", ""} {
		if err := refuseInlineAttachment(storage.ResourceKey{Type: other, ID: "one"}, body); err != nil {
			t.Errorf("the same body under %q answered %v", other, err)
		}
	}
}

// TestADocumentStatesWhatItRefersTo. R4 requires at least one content, because a
// reference to a document that names no document is not one.
func TestADocumentStatesWhatItRefersTo(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/DocumentReference",
		body: `{"resourceType":"DocumentReference","status":"current"` +
			`,"subject":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusBadRequest)

	if !strings.Contains(answer.Body.String(), "DocumentReference.content") {
		t.Errorf("the refusal does not name what is missing: %s", answer.Body)
	}
}
