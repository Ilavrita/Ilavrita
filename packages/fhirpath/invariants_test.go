package fhirpath_test

import (
	"sort"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhirpath"
)

// everyInvariant returns R4's own invariants, keyed by the name it gives them.
func everyInvariant(t *testing.T) map[string]conformance.Constraint {
	t.Helper()

	model, err := conformance.Definitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	held := map[string]conformance.Constraint{}

	for _, name := range model.Types() {
		structure, defined := model.Structure(name)
		if !defined {
			continue
		}

		for _, one := range structure.Root.Constraints {
			held[one.Key] = one
		}

		for _, element := range structure.Elements {
			for _, one := range element.Constraints {
				held[one.Key] = one
			}
		}
	}

	return held
}

// TestEveryInvariantR4StatesCanBeRead.
//
// The point of this package is to evaluate R4's invariants, so the measure of
// it is R4's invariants — not a grammar somebody thought was representative.
// One that cannot be read is reported here by name, because the alternative is
// a rule silently never checked.
func TestEveryInvariantR4StatesCanBeRead(t *testing.T) {
	invariants := everyInvariant(t)
	if len(invariants) == 0 {
		t.Fatal("no invariants were found, so this measures nothing")
	}

	var unread []string

	for key, one := range invariants {
		if !one.Required() {
			continue
		}

		if _, err := fhirpath.Parse(one.Expression); err != nil {
			unread = append(unread, key+": "+err.Error())
		}
	}

	sort.Strings(unread)

	var required int

	for _, one := range invariants {
		if one.Required() {
			required++
		}
	}

	t.Logf("read %d of %d required invariants", required-len(unread), required)

	for _, held := range unread {
		t.Logf("  unread %s", held)
	}

	if len(unread) > 0 {
		t.Errorf("%d required invariant(s) could not be read", len(unread))
	}
}
