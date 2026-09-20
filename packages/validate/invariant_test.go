package validate_test

import (
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/validate"
)

// TestEveryInvariantR4StatesCanBeApplied.
//
// The list is asserted rather than reported, because a rule that quietly
// stopped being checked is indistinguishable from one that passes. If this
// fails, some invariant this build used to apply no longer parses — and every
// resource that violates it has been accepted since.
func TestEveryInvariantR4StatesCanBeApplied(t *testing.T) {
	unevaluable, err := validate.Unevaluable()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	if len(unevaluable) != 0 {
		t.Errorf("%d invariant(s) cannot be applied: %v", len(unevaluable), unevaluable)
	}
}
