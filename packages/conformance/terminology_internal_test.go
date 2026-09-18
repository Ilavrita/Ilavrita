package conformance

import (
	"encoding/json"
	"testing"
)

// aSystem is one complete code system holding two codes.
func aSystem() map[string]definedCodeSystem {
	return map[string]definedCodeSystem{
		"http://example.test/cs": {
			URL: "http://example.test/cs", Content: "complete",
			Concept: []nestedConcept{
				{Code: "one"},
				{Code: "two", Concept: []nestedConcept{{Code: "two-a"}}},
			},
		},

		// Defined here and not fully: R4 says a fragment lists some of the
		// codes, so expanding it would produce a set that refuses the rest.
		"http://example.test/partial": {
			URL: "http://example.test/partial", Content: "fragment",
			Concept: []nestedConcept{{Code: "one"}},
		},
	}
}

// including builds a value set from one include.
func including(held composeInclude) composedValueSet {
	set := composedValueSet{URL: "http://example.test/vs"}
	set.Compose.Include = []composeInclude{held}

	return set
}

// TestASetThisBuildCannotWorkOutIsRefusedRatherThanGuessed.
//
// Every shape here would expand to something — a set missing its exclusions, a
// filtered include read as if it listed nothing — and every one of those is a
// set that admits the wrong codes. A half-worked-out set is worse than none:
// none decides nothing, and half of one decides wrongly.
func TestASetThisBuildCannotWorkOutIsRefusedRatherThanGuessed(t *testing.T) {
	complete := including(composeInclude{System: "http://example.test/cs"})

	excluded := complete
	excluded.Compose.Exclude = []composeInclude{{
		System: "http://example.test/cs",
		Concept: []struct {
			Code string `json:"code"`
		}{{Code: "two"}},
	}}

	for described, set := range map[string]composedValueSet{
		"an exclusion this build does not apply": excluded,
		"an include with a filter": including(composeInclude{
			System: "http://example.test/cs", Filter: []json.RawMessage{[]byte(`{}`)},
		}),
		"an include naming another value set": including(composeInclude{
			System: "http://example.test/cs", ValueSet: []string{"http://example.test/other"},
		}),
		"an include naming no system": including(composeInclude{}),
		"a system this build does not hold": including(composeInclude{
			System: "http://snomed.info/sct",
		}),
		"a system that is defined and not complete": including(composeInclude{
			System: "http://example.test/partial",
		}),
		"a system this build holds nothing about": including(composeInclude{
			System: "http://example.test/unknown",
		}),
		"no include at all": {URL: "http://example.test/vs"},
	} {
		if _, resolved := expand(set, aSystem()); resolved {
			t.Errorf("%s was expanded anyway", described)
		}
	}
}

// TestASetThisBuildCanWorkOutHoldsEveryCode, including the ones nested beneath
// another: a code that is a child of another is still a code.
func TestASetThisBuildCanWorkOutHoldsEveryCode(t *testing.T) {
	admitted, resolved := expand(
		including(composeInclude{System: "http://example.test/cs"}), aSystem())
	if !resolved {
		t.Fatal("a complete system was not expanded")
	}

	if admitted.Size() != 3 {
		t.Errorf("it holds %d codes, want one, two and two-a", admitted.Size())
	}

	for _, code := range []string{"one", "two", "two-a"} {
		if !admitted.Holds(Coded{System: "http://example.test/cs", Code: code}) {
			t.Errorf("%q is not in the set", code)
		}

		// A bare code carries no system: the binding is what says which one it
		// is in, so it matches on the code alone.
		if !admitted.Holds(Coded{Code: code}) {
			t.Errorf("%q is not held as a bare code", code)
		}
	}

	if admitted.Holds(Coded{System: "http://example.test/cs", Code: "three"}) {
		t.Error("a code nobody defined is in the set")
	}

	// A code from another system is not in it, even when the code matches.
	if admitted.Holds(Coded{System: "http://snomed.info/sct", Code: "one"}) {
		t.Error("a code from another system is in the set")
	}
}

// TestAnExplicitConceptListIsTakenAsWritten, which is the other shape R4's
// required bindings come in.
func TestAnExplicitConceptListIsTakenAsWritten(t *testing.T) {
	admitted, resolved := expand(including(composeInclude{
		System: "http://example.test/cs",
		Concept: []struct {
			Code string `json:"code"`
		}{{Code: "one"}},
	}), aSystem())

	if !resolved {
		t.Fatal("a listed set was not expanded")
	}

	if admitted.Size() != 1 || !admitted.Holds(Coded{Code: "one"}) {
		t.Errorf("it holds %d codes", admitted.Size())
	}

	// The system holds "two"; the value set does not list it, so it is out.
	if admitted.Holds(Coded{Code: "two"}) {
		t.Error("a code the system holds and the set does not list is in the set")
	}
}
