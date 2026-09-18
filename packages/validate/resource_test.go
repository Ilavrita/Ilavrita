package validate_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/validate"
)

// checked reports the expressions a body was faulted on.
func checked(body string) []string {
	var where []string

	for _, issue := range validate.Resource("Observation", []byte(body)).Issues() {
		if issue.Severity == validate.SeverityError {
			where = append(where, issue.Expression)
		}
	}

	return where
}

// TestAWellFormedResourceRaisesNothing, so validation refuses what is wrong
// rather than what is merely unfamiliar.
func TestAWellFormedResourceRaisesNothing(t *testing.T) {
	for described, body := range map[string]string{
		"the least a resource can be": `{"resourceType":"Observation"}`,
		"an ordinary one": `{"resourceType":"Observation","id":"obs-1","status":"final",` +
			`"subject":{"reference":"Patient/pat-1"},"effectiveDateTime":"2026-03-01",` +
			`"category":[{"coding":[{"system":"http://loinc.org","code":"vital-signs"}]}]}`,
		"a reference to a type this build does not serve": `{"resourceType":"Observation",` +
			`"subject":{"reference":"Group/grp-1"}}`,
		"an absolute reference": `{"resourceType":"Observation",` +
			`"subject":{"reference":"https://example.test/fhir/Patient/pat-1"}}`,
		"a contained reference": `{"resourceType":"Observation","subject":{"reference":"#p1"}}`,
		"an element nobody here has heard of": `{"resourceType":"Observation",` +
			`"somethingR4DefinesAndThisBuildDoesNot":{"nested":"value"}}`,
		"a full instant":    `{"resourceType":"Observation","effectiveDateTime":"2026-03-01T09:30:00.5Z"}`,
		"an offset instant": `{"resourceType":"Observation","effectiveDateTime":"2026-03-01T09:30:00+05:30"}`,
	} {
		if where := checked(body); len(where) != 0 {
			t.Errorf("%s was faulted at %v", described, where)
		}
	}
}

// TestTheRepresentationRulesHoldEverywhere. These are rules about how JSON
// represents FHIR rather than about any one resource, which is exactly why they
// can be held without an element model: no StructureDefinition permits a null,
// an empty string or an empty array anywhere.
func TestTheRepresentationRulesHoldEverywhere(t *testing.T) {
	for described, held := range map[string]struct {
		body string
		want string
	}{
		"a null at the top": {
			`{"resourceType":"Observation","status":null}`, "Observation.status"},
		"a null nested": {
			`{"resourceType":"Observation","subject":{"display":null}}`, "Observation.subject.display"},
		"a null inside an array": {
			`{"resourceType":"Observation","performer":[null]}`, "Observation.performer[0]"},
		"an empty string": {
			`{"resourceType":"Observation","status":""}`, "Observation.status"},
		"a string of spaces": {
			`{"resourceType":"Observation","status":"   "}`, "Observation.status"},
		"an empty array": {
			`{"resourceType":"Observation","performer":[]}`, "Observation.performer"},
		"an empty array nested": {
			`{"resourceType":"Observation","code":{"coding":[]}}`, "Observation.code.coding"},
	} {
		where := checked(held.body)
		if !slices.Contains(where, held.want) {
			t.Errorf("%s was faulted at %v, want %s", described, where, held.want)
		}
	}
}

// TestAnIdIsTheIdDatatype, which every logical id is and which a URL has to be
// able to carry.
func TestAnIdIsTheIdDatatype(t *testing.T) {
	for described, id := range map[string]string{
		"a slash":         "obs/1",
		"a space":         "obs 1",
		"an underscore":   "obs_1",
		"sixty-five long": strings.Repeat("a", 65),
	} {
		body := `{"resourceType":"Observation","id":"` + id + `"}`
		if where := checked(body); !slices.Contains(where, "Observation.id") {
			t.Errorf("%s was accepted as an id", described)
		}
	}

	for described, id := range map[string]string{
		"letters and digits": "obs1",
		"a hyphen":           "obs-1",
		"a dot":              "obs.1",
		"sixty-four long":    strings.Repeat("a", 64),
	} {
		body := `{"resourceType":"Observation","id":"` + id + `"}`
		if where := checked(body); len(where) != 0 {
			t.Errorf("%s was refused as an id: %v", described, where)
		}
	}
}

// TestARelativeReferenceNamesATypeAndAnId. A reference that is not one is a
// link nothing can follow, and this server stores compartments from exactly
// these elements.
func TestARelativeReferenceNamesATypeAndAnId(t *testing.T) {
	for described, reference := range map[string]string{
		"no type":                   "pat-1",
		"a lowercase type":          "patient/pat-1",
		"a type R4 does not define": "Patients/pat-1",
		"no id":                     "Patient/",
		"an id with a space":        "Patient/pat 1",
	} {
		body := `{"resourceType":"Observation","subject":{"reference":"` + reference + `"}}`
		if where := checked(body); !slices.Contains(where, "Observation.subject.reference") {
			t.Errorf("%q was accepted as a reference", described)
		}
	}
}

// TestAnIndexedDateIsADate. The search registry is where this build says what
// an element is, so a resource whose date is not one would be stored and then
// quietly missing from every search for it.
func TestAnIndexedDateIsADate(t *testing.T) {
	for _, held := range []string{"the first of March", "2026-13-01", "2026-03-01T09:30", "01/03/2026"} {
		body := `{"resourceType":"Observation","effectiveDateTime":"` + held + `"}`
		if where := checked(body); len(where) == 0 {
			t.Errorf("%q was accepted as a date", held)
		}
	}
}

// TestServerOwnedMembersAreAWarningAndNotAnError. The write path ignores them
// and stamps its own, so the resource is stored correctly either way; what the
// client needs to know is that what they sent was not kept.
func TestServerOwnedMembersAreAWarningAndNotAnError(t *testing.T) {
	report := validate.Resource("Observation",
		[]byte(`{"resourceType":"Observation","meta":{"versionId":"7","lastUpdated":"2020-01-01T00:00:00Z"}}`))

	if !report.OK() {
		t.Errorf("a submitted meta stopped the write: %v", report.Issues())
	}

	if len(report.Issues()) != 2 {
		t.Errorf("it raised %v, want one warning for each member", report.Issues())
	}
}

// TestABodyThatIsNotAResourceIsOneIssue, rather than a list of everything that
// follows from it.
func TestABodyThatIsNotAResourceIsOneIssue(t *testing.T) {
	for described, body := range map[string]string{
		"not JSON":       `{`,
		"a JSON array":   `[{"resourceType":"Observation"}]`,
		"a bare string":  `"Observation"`,
		"another type":   `{"resourceType":"Patient"}`,
		"no type at all": `{"status":"final"}`,
	} {
		report := validate.Resource("Observation", []byte(body))

		if report.OK() {
			t.Errorf("%s validated", described)
		}

		if len(report.Issues()) != 1 {
			t.Errorf("%s raised %d issues, want one", described, len(report.Issues()))
		}
	}
}

// TestABodyNestedBeyondReadingIsRefused, rather than costing this server a
// stack to find out what is in it.
func TestABodyNestedBeyondReadingIsRefused(t *testing.T) {
	body := `{"resourceType":"Observation","a":` +
		strings.Repeat(`{"a":`, 200) + `"deep"` + strings.Repeat(`}`, 200) + `}`

	if report := validate.Resource("Observation", []byte(body)); report.OK() {
		t.Error("a body nested two hundred deep validated")
	}
}

// TestACleanReportStillCarriesAnIssue. An OperationOutcome with no issue in it
// is not a valid OperationOutcome, so a validation that found nothing says so.
func TestACleanReportStillCarriesAnIssue(t *testing.T) {
	outcome := validate.Resource("Observation", []byte(`{"resourceType":"Observation"}`)).Outcome()

	if len(outcome.Issue) != 1 || outcome.Issue[0].Severity != "information" {
		t.Errorf("a clean report rendered as %+v", outcome)
	}
}

// TestAnOutcomeSaysWhereEachIssueIs, because an issue a client cannot locate is
// one they have to find by reading the whole body back.
func TestAnOutcomeSaysWhereEachIssueIs(t *testing.T) {
	outcome := validate.Resource("Observation",
		[]byte(`{"resourceType":"Observation","status":null,"performer":[]}`)).Outcome()

	if len(outcome.Issue) != 2 {
		t.Fatalf("the outcome carries %d issues", len(outcome.Issue))
	}

	for _, issue := range outcome.Issue {
		if len(issue.Expression) != 1 || issue.Expression[0] == "" {
			t.Errorf("an issue names no element: %+v", issue)
		}
	}
}
