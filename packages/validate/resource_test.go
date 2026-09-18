package validate_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/validate"
)

// required is what Observation's own definition says every one of them carries.
// These cases are about the rules that hold whatever the definition says, so
// each fixture states it and the two do not have to be read together.
const required = `"status":"final","code":{"text":"a reading"}`

// an builds one Observation carrying the elements it must, plus whatever the
// case is about.
func an(extra string) string {
	if extra != "" {
		extra = "," + extra
	}

	return `{"resourceType":"Observation",` + required + extra + `}`
}

// faultedAs reports whether a body was faulted at one expression for a reason
// whose wording contains what the caller names.
//
// The expression alone is not enough to assert on: several rules can fault the
// same element, so a case meaning to prove that an undeclared element is
// refused would pass on a cardinality complaint at the same path.
func faultedAs(body, where, because string) bool {
	for _, issue := range validate.Resource("Observation", []byte(body)).Issues() {
		if issue.Severity != validate.SeverityError || issue.Expression != where {
			continue
		}

		if strings.Contains(issue.Detail, because) {
			return true
		}
	}

	return false
}

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
		"the least a resource can be": an(""),
		"an ordinary one": an(`"id":"obs-1","subject":{"reference":"Patient/pat-1"},` +
			`"effectiveDateTime":"2026-03-01",` +
			`"category":[{"coding":[{"system":"http://loinc.org","code":"vital-signs"}]}]`),
		"a reference to a type this build does not serve": an(
			`"subject":{"reference":"Group/grp-1"}`),
		"an absolute reference": an(
			`"subject":{"reference":"https://example.test/fhir/Patient/pat-1"}`),
		"a contained reference": an(`"subject":{"reference":"#p1"}`),
		"an extension on a primitive": an(
			`"_status":{"extension":[{"url":"http://example.test/x","valueString":"why"}]}`),
		"a choice written with its type": an(`"valueQuantity":{"value":7}`),
		"a full instant":                 an(`"effectiveDateTime":"2026-03-01T09:30:00.5Z"`),
		"an offset instant":              an(`"effectiveDateTime":"2026-03-01T09:30:00+05:30"`),
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
			an(`"note":null`), "Observation.note"},
		"a null nested": {
			an(`"subject":{"display":null}`), "Observation.subject.display"},
		"a null inside an array": {
			an(`"performer":[null]`), "Observation.performer[0]"},
		"an empty string": {
			`{"resourceType":"Observation","status":"","code":{"text":"a reading"}}`,
			"Observation.status"},
		"a string of spaces": {
			`{"resourceType":"Observation","status":"   ","code":{"text":"a reading"}}`,
			"Observation.status"},
		"an empty array": {
			an(`"performer":[]`), "Observation.performer"},
		"an empty array nested": {
			`{"resourceType":"Observation","status":"final","code":{"coding":[]}}`,
			"Observation.code.coding"},
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
		if where := checked(an(`"id":"` + id + `"`)); !slices.Contains(where, "Observation.id") {
			t.Errorf("%s was accepted as an id", described)
		}
	}

	for described, id := range map[string]string{
		"letters and digits": "obs1",
		"a hyphen":           "obs-1",
		"a dot":              "obs.1",
		"sixty-four long":    strings.Repeat("a", 64),
	} {
		if where := checked(an(`"id":"` + id + `"`)); len(where) != 0 {
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
		body := an(`"subject":{"reference":"` + reference + `"}`)
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
		body := an(`"effectiveDateTime":"` + held + `"`)
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
		[]byte(an(`"meta":{"versionId":"7","lastUpdated":"2020-01-01T00:00:00Z"}`)))

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
	body := an(`"a":` + strings.Repeat(`{"a":`, 200) + `"deep"` + strings.Repeat(`}`, 200))

	if report := validate.Resource("Observation", []byte(body)); report.OK() {
		t.Error("a body nested two hundred deep validated")
	}
}

// TestACleanReportStillCarriesAnIssue. An OperationOutcome with no issue in it
// is not a valid OperationOutcome, so a validation that found nothing says so.
func TestACleanReportStillCarriesAnIssue(t *testing.T) {
	outcome := validate.Resource("Observation", []byte(an(""))).Outcome()

	if len(outcome.Issue) != 1 || outcome.Issue[0].Severity != "information" {
		t.Errorf("a clean report rendered as %+v", outcome)
	}
}

// TestAnOutcomeSaysWhereEachIssueIs, because an issue a client cannot locate is
// one they have to find by reading the whole body back.
func TestAnOutcomeSaysWhereEachIssueIs(t *testing.T) {
	outcome := validate.Resource("Observation", []byte(an(`"note":null,"performer":[]`))).Outcome()

	if len(outcome.Issue) == 0 {
		t.Fatal("a resource with two faults in it validated")
	}

	for _, issue := range outcome.Issue {
		if len(issue.Expression) != 1 || issue.Expression[0] == "" {
			t.Errorf("an issue names no element: %+v", issue)
		}
	}
}

// TestAResourceIsCheckedAgainstItsOwnDefinition. This is what the base
// definitions are for: the rules above hold for every resource whatever it is,
// and these hold because of what an Observation is.
func TestAResourceIsCheckedAgainstItsOwnDefinition(t *testing.T) {
	for described, held := range map[string]struct {
		body string
		want string
	}{
		"a single element written as an array": {
			`{"resourceType":"Observation","status":["final"],"code":{"text":"a reading"}}`,
			"Observation.status"},
		"a repeating element written as one": {
			an(`"performer":{"reference":"Practitioner/prac-1"}`), "Observation.performer"},
		"a nested element R4 does not define": {
			an(`"subject":{"notAReferenceMember":"held"}`), "Observation.subject.notAReferenceMember"},
		"a backbone's element R4 does not define": {
			an(`"component":[{"code":{"text":"a part"},"notAComponentMember":1}]`),
			"Observation.component[0].notAComponentMember"},
		"a number where a string belongs": {
			`{"resourceType":"Observation","status":7,"code":{"text":"a reading"}}`,
			"Observation.status"},
		"a string where a number belongs": {
			an(`"valueQuantity":{"value":"seven"}`), "Observation.valueQuantity.value"},
	} {
		if where := checked(held.body); !slices.Contains(where, held.want) {
			t.Errorf("%s was faulted at %v, want %s", described, where, held.want)
		}
	}
}

// TestAnElementNobodyDeclaredIsRefusedAsOne. Several rules can fault the same
// element, so what is asserted here is the reason as well as the place: this is
// the check that needs the definition, and a cardinality complaint at the same
// path would not be it.
func TestAnElementNobodyDeclaredIsRefusedAsOne(t *testing.T) {
	for described, held := range map[string]struct {
		body string
		want string
	}{
		"an element R4 does not define": {
			an(`"somethingR4DefinesAndThisBuildDoesNot":"held"`),
			"Observation.somethingR4DefinesAndThisBuildDoesNot"},
		"an element misspelled": {
			an(`"Status":"final"`), "Observation.Status"},
		"a choice with no such type": {
			an(`"valueNotAType":7`), "Observation.valueNotAType"},
		"a nested element R4 does not define": {
			an(`"subject":{"notAReferenceMember":"held"}`),
			"Observation.subject.notAReferenceMember"},
		"a backbone's element R4 does not define": {
			an(`"component":[{"code":{"text":"a part"},"notAComponentMember":1}]`),
			"Observation.component[0].notAComponentMember"},
	} {
		if !faultedAs(held.body, held.want, "No such element") {
			t.Errorf("%s was not refused as an element nobody declared: %v",
				described, checked(held.body))
		}
	}
}

// TestARequiredElementIsRequired, which is the cardinality half of what a
// definition says.
func TestARequiredElementIsRequired(t *testing.T) {
	for described, held := range map[string]struct {
		body string
		want string
	}{
		"no status": {`{"resourceType":"Observation","code":{"text":"a reading"}}`,
			"Observation.status"},
		"no code": {`{"resourceType":"Observation","status":"final"}`, "Observation.code"},
		"a backbone's own requirement": {
			an(`"component":[{"valueString":"held"}]`), "Observation.component[0].code"},
	} {
		if where := checked(held.body); !slices.Contains(where, held.want) {
			t.Errorf("%s was accepted: faulted at %v, want %s", described, where, held.want)
		}
	}
}

// TestAChoiceIsWrittenOnce. An element written "value[x]" holds one value; two
// of them is a resource saying two things where it may say one, and nothing
// downstream could tell which was meant.
func TestAChoiceIsWrittenOnce(t *testing.T) {
	body := an(`"valueQuantity":{"value":7},"valueString":"seven"`)

	if where := checked(body); !slices.Contains(where, "Observation.value[x]") {
		t.Errorf("two values in one choice were faulted at %v", where)
	}

	// And one of them is an ordinary resource.
	if where := checked(an(`"valueString":"seven"`)); len(where) != 0 {
		t.Errorf("one value was faulted at %v", where)
	}
}

// TestATypeWithNoDefinitionIsCheckedNoFurther. A definition is what this server
// knows, not what a client did wrong, so a type it has none for is held to the
// rules that need none and nothing else.
func TestATypeWithNoDefinitionIsCheckedNoFurther(t *testing.T) {
	report := validate.Resource("SomethingNobodyDefined",
		[]byte(`{"resourceType":"SomethingNobodyDefined","anything":"at all"}`))

	if !report.OK() {
		t.Errorf("a type with no definition was faulted: %v", report.Issues())
	}
}

// TestNestingPastTheBoundIsUncheckedRatherThanRefused.
//
// R4 lets an element hold its own kind, which a definition cannot write out
// because it would not end, so the model expands it only so far. What is past
// that has no children in the model — and an element with no children would
// otherwise have every element it does hold reported as one nobody declared,
// which is a refusal for a resource that is right.
//
// Nothing the specification itself publishes nests that deep, so this is the
// only thing that holds the rule.
func TestNestingPastTheBoundIsUncheckedRatherThanRefused(t *testing.T) {
	// A Questionnaire item inside an item, twelve deep, with ordinary elements
	// at the bottom.
	const depth = 12

	body := `{"linkId":"deep","type":"display","text":"the bottom"}`
	for range depth {
		body = `{"linkId":"held","type":"group","item":[` + body + `]}`
	}

	body = `{"resourceType":"Questionnaire","status":"active","item":[` + body + `]}`

	report := validate.Resource("Questionnaire", []byte(body))
	if !report.OK() {
		t.Errorf("a questionnaire nested %d deep was refused: %v", depth, report.Issues())
	}

	// And it is not that Questionnaire is unchecked altogether: an element
	// nobody declared, at a depth the model does reach, is still refused.
	shallow := `{"resourceType":"Questionnaire","status":"active",` +
		`"item":[{"linkId":"held","type":"group","notAnItemMember":1}]}`

	if validate.Resource("Questionnaire", []byte(shallow)).OK() {
		t.Error("an element nobody declared was accepted inside an item")
	}
}
