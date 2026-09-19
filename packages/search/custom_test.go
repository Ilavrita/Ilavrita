package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// searchParameter writes one, stating only what a test cares about.
func searchParameter(code, kind, expression string, bases ...string) string {
	quoted := make([]string, 0, len(bases))
	for _, base := range bases {
		quoted = append(quoted, `"`+base+`"`)
	}

	return `{"resourceType":"SearchParameter","url":"http://example.com/sp/` + code + `",` +
		`"name":"` + code + `","status":"active","description":"a test parameter",` +
		`"code":"` + code + `","type":"` + kind + `","base":[` + strings.Join(quoted, ",") + `],` +
		`"expression":"` + expression + `"}`
}

func onlyDefined(t *testing.T, content string) Defined {
	t.Helper()

	held, err := ReadSearchParameter([]byte(content))
	if err != nil {
		t.Fatalf("read the parameter: %v", err)
	}

	if len(held) != 1 {
		t.Fatalf("compiled %d parameter(s), want 1", len(held))
	}

	return held[0]
}

// TestATokenTakesItsHalvesFromTheDatatype. A token is a code and the system
// that defines it, and the two have to come from the same element — so which
// members hold them is read from the element's own datatype rather than stated
// by whoever wrote the SearchParameter. A definition that paired one coding's
// system with another's code is one nobody could tell was wrong.
func TestATokenTakesItsHalvesFromTheDatatype(t *testing.T) {
	for _, held := range []struct {
		named, expression, path, code, system string
	}{
		// ContactPoint keeps the value beside the system that classifies it.
		{"phone", "Patient.telecom", "telecom", "value", "system"},
		// A CodeableConcept is read one level in, at the coding, which is where
		// the two halves actually are.
		{"marital", "Patient.maritalStatus", "maritalStatus.coding", "code", "system"},
	} {
		t.Run(held.named, func(t *testing.T) {
			defined := onlyDefined(t, searchParameter(held.named, "token", held.expression, "Patient"))

			if got := strings.Join(defined.Parameter.path, "."); got != held.path {
				t.Errorf("reads %q, want %q", got, held.path)
			}

			if defined.Parameter.code != held.code || defined.Parameter.system != held.system {
				t.Errorf("pairs %q/%q, want %q/%q",
					defined.Parameter.code, defined.Parameter.system, held.code, held.system)
			}
		})
	}

	// An element that is itself the code has no system beside it.
	plain := onlyDefined(t, searchParameter("sex", "token", "Patient.gender", "Patient"))
	if plain.Parameter.code != "" || plain.Parameter.system != "" {
		t.Errorf("a plain code was given members %q/%q", plain.Parameter.code, plain.Parameter.system)
	}
}

// TestEachKindReadsOnlyWhatItCanRead. The kind and the element's datatype have
// to agree, because the kind decides both what a write projects and what a read
// compiles: a date parameter over a HumanName would index nothing and then
// match nothing, while looking like a restriction somebody stated.
func TestEachKindReadsOnlyWhatItCanRead(t *testing.T) {
	for _, held := range []struct{ named, kind, expression string }{
		{"a string over a HumanName", "string", "Patient.name"},
		{"a string over an Address", "string", "Patient.address"},
		{"a reference", "reference", "Patient.generalPractitioner"},
		{"a date", "date", "Patient.birthDate"},
		{"a date over a Period", "date", "Encounter.period"},
	} {
		t.Run(held.named, func(t *testing.T) {
			base := strings.SplitN(held.expression, ".", 2)[0]
			if _, err := ReadSearchParameter(
				[]byte(searchParameter("custom", held.kind, held.expression, base))); err != nil {
				t.Errorf("%s was refused: %v", held.named, err)
			}
		})
	}

	for _, held := range []struct{ named, kind, expression string }{
		{"a date over a name", "date", "Patient.name"},
		{"a reference over a code", "reference", "Patient.gender"},
		{"a string over a Reference", "string", "Patient.generalPractitioner"},
		{"a token over a Reference", "token", "Patient.generalPractitioner"},
	} {
		t.Run(held.named, func(t *testing.T) {
			_, err := ReadSearchParameter([]byte(searchParameter("custom", held.kind, held.expression, "Patient")))
			if !errors.Is(err, ErrParameterElement) {
				t.Errorf("%s was accepted: %v", held.named, err)
			}
		})
	}
}

// TestAnExpressionThisBuildCannotWalkIsRefused. R4 states where a parameter
// reads from in FHIRPath and this build has no FHIRPath engine. Approximating
// one would be worse than refusing: a parameter that matched something other
// than what it says comes back looking answered.
func TestAnExpressionThisBuildCannotWalkIsRefused(t *testing.T) {
	for _, expression := range []string{
		"Patient.name.where(use='official')",
		"Patient.telecom | Patient.address",
		"Patient.contact.name.given[0]",
		"(Patient.name)",
		"Observation.value as Quantity",
		"name.family",
	} {
		_, err := ReadSearchParameter([]byte(searchParameter("custom", "string", expression, "Patient")))
		if !errors.Is(err, ErrParameterExpression) {
			t.Errorf("%q was accepted: %v", expression, err)
		}
	}
}

// TestAChoiceElementNamesNoOneDatatype, so it cannot be indexed as one. R4
// writes each type of a choice under its own member name, so the path does not
// say which one a value was written as.
func TestAChoiceElementNamesNoOneDatatype(t *testing.T) {
	_, err := ReadSearchParameter([]byte(searchParameter("gone", "token", "Patient.deceased", "Patient")))
	if !errors.Is(err, ErrParameterElement) {
		t.Errorf("a choice element was indexed as one datatype: %v", err)
	}
}

// TestACodeAlreadyAnsweredIsRefused. Shadowing a built-in would change what an
// existing query means, silently, for everyone in the Project.
func TestACodeAlreadyAnsweredIsRefused(t *testing.T) {
	for _, code := range []string{"gender", "family", "_id", "_lastUpdated"} {
		_, err := ReadSearchParameter([]byte(searchParameter(code, "string", "Patient.address", "Patient")))
		if !errors.Is(err, ErrParameterReserved) {
			t.Errorf("%q shadowed a built-in: %v", code, err)
		}
	}
}

// TestOnlyTheKindsThisBuildIndexes. number, quantity, uri, composite and
// special are R4 types this build does not project, and a parameter stating one
// is refused rather than stored as something that answers nothing.
func TestOnlyTheKindsThisBuildIndexes(t *testing.T) {
	for _, kind := range []string{"number", "quantity", "uri", "composite", "special"} {
		_, err := ReadSearchParameter([]byte(searchParameter("custom", kind, "Patient.address", "Patient")))
		if !errors.Is(err, ErrParameterKindUnsupported) {
			t.Errorf("%q was accepted: %v", kind, err)
		}
	}
}

// TestABaseThisServerDoesNotServeIsRefused, because a parameter on a type no
// route answers is a parameter nothing could ever use.
func TestABaseThisServerDoesNotServeIsRefused(t *testing.T) {
	_, err := ReadSearchParameter([]byte(searchParameter("custom", "string", "Appointment.description", "Appointment")))
	if !errors.Is(err, ErrParameterBase) {
		t.Errorf("a type this server does not serve was accepted: %v", err)
	}
}

// TestEveryBaseIsCompiledOnItsOwn. One SearchParameter may name several, and
// the same expression reads a different element on each.
func TestEveryBaseIsCompiledOnItsOwn(t *testing.T) {
	held, err := ReadSearchParameter(
		[]byte(searchParameter("when", "date", "Patient.birthDate", "Patient")))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(held) != 1 || held[0].Type != storage.ResourceType("Patient") {
		t.Fatalf("compiled %+v", held)
	}

	// A base whose element does not exist fails the whole parameter rather than
	// being dropped: half a definition is not what anyone wrote.
	_, err = ReadSearchParameter(
		[]byte(searchParameter("when", "date", "Patient.birthDate", "Patient", "Observation")))
	if !errors.Is(err, ErrParameterExpression) && !errors.Is(err, ErrParameterElement) {
		t.Errorf("a base the expression does not read from was accepted: %v", err)
	}
}

// TestADraftParameterIndexesNothing. A Project drafting one should be able to
// store it; what it must not do is quietly start indexing.
func TestADraftParameterIndexesNothing(t *testing.T) {
	for _, status := range []string{"draft", "retired", "unknown"} {
		body := strings.Replace(
			searchParameter("custom", "string", "Patient.address", "Patient"),
			`"status":"active"`, `"status":"`+status+`"`, 1)

		held, err := ReadSearchParameter([]byte(body))
		if err != nil {
			t.Errorf("a %s parameter was an error: %v", status, err)
		}

		if len(held) != 0 {
			t.Errorf("a %s parameter compiled %d parameter(s)", status, len(held))
		}
	}
}

// TestSomethingThatIsNotASearchParameterIsRefused.
func TestSomethingThatIsNotASearchParameterIsRefused(t *testing.T) {
	for _, body := range []string{`{"resourceType":"Patient"}`, `{`, `{"resourceType":"SearchParameter"}`} {
		if _, err := ReadSearchParameter([]byte(body)); err == nil {
			t.Errorf("%s was read as a parameter", body)
		}
	}
}
