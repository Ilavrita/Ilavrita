package fhirpath_test

import (
	"encoding/json"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhirpath"
)

// against evaluates one expression over one resource.
func against(t *testing.T, resource, expression string) []any {
	t.Helper()

	var held any
	if err := json.Unmarshal([]byte(resource), &held); err != nil {
		t.Fatalf("read the resource: %v", err)
	}

	parsed, err := fhirpath.Parse(expression)
	if err != nil {
		t.Fatalf("read %q: %v", expression, err)
	}

	found, err := fhirpath.Evaluate(parsed, fhirpath.Context{This: held, Resource: held})
	if err != nil {
		t.Fatalf("evaluate %q: %v", expression, err)
	}

	return found
}

// holds evaluates an expression the way an invariant is judged.
func holds(t *testing.T, resource, expression string) bool {
	t.Helper()

	var held any
	if err := json.Unmarshal([]byte(resource), &held); err != nil {
		t.Fatalf("read the resource: %v", err)
	}

	parsed, err := fhirpath.Parse(expression)
	if err != nil {
		t.Fatalf("read %q: %v", expression, err)
	}

	settled, err := fhirpath.Holds(parsed, fhirpath.Context{This: held, Resource: held})
	if err != nil {
		t.Fatalf("evaluate %q: %v", expression, err)
	}

	return settled
}

const anOrganization = `{
  "resourceType":"Organization",
  "id":"one",
  "name":"Ward Clinic",
  "alias":["The Ward","Ward"],
  "telecom":[{"system":"phone","value":"555","use":"work"},{"system":"email","value":"a@b.c"}],
  "address":[{"use":"work","city":"Berlin"}],
  "active":true
}`

// TestNavigationCrossesArraysTheSameWayItCrossesValues, because FHIR models one
// element as repeating and the next as single and an invariant's author should
// not have to know which.
func TestNavigationCrossesArraysTheSameWayItCrossesValues(t *testing.T) {
	for expression, want := range map[string]int{
		"name":          1,
		"alias":         2,
		"telecom":       2,
		"telecom.value": 2,
		"address.city":  1,
		"nothing":       0,
		"name.nothing":  0,
	} {
		if found := against(t, anOrganization, expression); len(found) != want {
			t.Errorf("%s produced %d item(s), want %d", expression, len(found), want)
		}
	}
}

// TestTheExistenceFunctions.
func TestTheExistenceFunctions(t *testing.T) {
	for expression, want := range map[string]bool{
		"name.exists()":                        true,
		"nothing.exists()":                     false,
		"nothing.empty()":                      true,
		"name.empty()":                         false,
		"alias.count() = 2":                    true,
		"telecom.where(use = 'work').exists()": true,
		"telecom.where(use = 'home').exists()": false,
		"telecom.exists(use = 'work')":         true,
		"alias.isDistinct()":                   true,
		"telecom.all(value.exists())":          true,
		"telecom.all(use.exists())":            false,
		"name.exists().not()":                  false,
	} {
		if found := against(t, anOrganization, expression); len(found) != 1 || found[0] != want {
			t.Errorf("%s produced %v, want [%v]", expression, found, want)
		}
	}
}

// TestTheLogicalOperatorsAreThreeValued. FHIRPath says `false and {}` is false
// because nothing the empty side could be would change it, and `true and {}` is
// empty because something would. Invariants written `a implies b` rest on it.
func TestTheLogicalOperatorsAreThreeValued(t *testing.T) {
	for expression, want := range map[string]any{
		"false and nothing.exists()": false,
		"true and true":              true,
		"true or nothing":            true,
		"false or false":             false,
		"true implies false":         false,
		"false implies false":        true,
		"true xor false":             true,
		"nothing implies true":       true,
	} {
		found := against(t, anOrganization, expression)
		if len(found) != 1 || found[0] != want {
			t.Errorf("%s produced %v, want [%v]", expression, found, want)
		}
	}

	// And an expression that decides nothing leaves a resource alone: refusing
	// on what could not be judged would refuse resources nobody showed wrong.
	if !holds(t, anOrganization, "nothing") {
		t.Error("an expression producing nothing was treated as a failure")
	}
}

// TestComparisonAndArithmetic.
func TestComparisonAndArithmetic(t *testing.T) {
	for expression, want := range map[string]any{
		"alias.count() > 1":                     true,
		"alias.count() >= 2":                    true,
		"alias.count() < 2":                     false,
		"(alias.count() + telecom.count()) = 4": true,
		"name = 'Ward Clinic'":                  true,
		"name != 'Other'":                       true,
		"'a' + 'b' = 'ab'":                      true,
		"name.startsWith('Ward')":               true,
		"name.matches('^Ward')":                 true,
		"name.contains('Clinic')":               true,
	} {
		found := against(t, anOrganization, expression)
		if len(found) != 1 || found[0] != want {
			t.Errorf("%s produced %v, want [%v]", expression, found, want)
		}
	}
}

// TestAChoiceIsNavigatedByItsNameWithoutTheType, which is how R4 writes every
// invariant about one: the expression says `value`, the resource says
// `valueQuantity`.
func TestAChoiceIsNavigatedByItsNameWithoutTheType(t *testing.T) {
	const observation = `{"resourceType":"Observation","status":"final",
	  "code":{"text":"x"},"valueString":"a reading"}`

	if found := against(t, observation, "value"); len(found) != 1 || found[0] != "a reading" {
		t.Errorf("value produced %v", found)
	}

	if found := against(t, observation, "value.exists()"); len(found) != 1 || found[0] != true {
		t.Errorf("value.exists() produced %v", found)
	}

	// obs-6 in R4's own words.
	if !holds(t, observation, "dataAbsentReason.empty() or value.empty()") {
		t.Error("obs-6 refused an Observation with a value and no dataAbsentReason")
	}
}

// TestAnInvariantR4StatesIsJudgedTheWayR4MeansIt.
func TestAnInvariantR4StatesIsJudgedTheWayR4MeansIt(t *testing.T) {
	const orgOne = "(identifier.count() + name.count()) > 0"

	if !holds(t, anOrganization, orgOne) {
		t.Error("org-1 refused an Organization that has a name")
	}

	if holds(t, `{"resourceType":"Organization","id":"x"}`, orgOne) {
		t.Error("org-1 accepted an Organization with neither a name nor an identifier")
	}

	// org-2: an Organization's address may not be a home address. It is stated
	// about the address, so it is evaluated with the address as the context.
	parsed, err := fhirpath.Parse("where(use = 'home').empty()")
	if err != nil {
		t.Fatalf("read org-2: %v", err)
	}

	for _, held := range []struct {
		address string
		want    bool
	}{
		{`{"use":"work","city":"Berlin"}`, true},
		{`{"use":"home","city":"Berlin"}`, false},
	} {
		var address any
		if err := json.Unmarshal([]byte(held.address), &address); err != nil {
			t.Fatal(err)
		}

		settled, err := fhirpath.Holds(parsed, fhirpath.Context{This: address, Resource: address})
		if err != nil {
			t.Fatalf("evaluate org-2: %v", err)
		}

		if settled != held.want {
			t.Errorf("org-2 on %s answered %v, want %v", held.address, settled, held.want)
		}
	}
}

// TestSomethingOutsideTheSubsetIsAnErrorRatherThanAPass. The whole reason this
// package draws a line is so a caller can tell "this resource is fine" from
// "nobody checked".
func TestSomethingOutsideTheSubsetIsAnErrorRatherThanAPass(t *testing.T) {
	for _, expression := range []string{
		"name.resolve().exists()",
		"alias.aggregate($this)",
		"name.intersect(alias)",
	} {
		parsed, err := fhirpath.Parse(expression)
		if err != nil {
			continue
		}

		var held any
		if err := json.Unmarshal([]byte(anOrganization), &held); err != nil {
			t.Fatal(err)
		}

		if _, err := fhirpath.Evaluate(parsed, fhirpath.Context{This: held, Resource: held}); err == nil {
			t.Errorf("%q evaluated instead of reporting that it cannot be", expression)
		}
	}
}
