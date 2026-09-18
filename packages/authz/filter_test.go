package authz

import (
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func mustElementFilter(t *testing.T, path string, values ...string) storage.Filter {
	t.Helper()

	comparator := storage.ComparatorIn
	if len(values) == 1 {
		comparator = storage.ComparatorEqual
	}

	filter, err := storage.NewFilter(path, comparator, values...)
	if err != nil {
		t.Fatalf("NewFilter(%q): %v", path, err)
	}

	return filter
}

// finalOnlyPolicy narrows a patient's own chart to the Observations that have
// been finalised, which is the restriction a reviewing clinician holds when
// preliminary results are not theirs to see.
func finalOnlyPolicy(t *testing.T) AccessPolicy {
	t.Helper()

	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")).
		WithFilter(mustElementFilter(t, "status", "final"))

	return mustPolicy(t, PolicyConfig{
		Project:    clinic,
		ID:         "pol_final_only",
		Parameters: []ParameterName{"patient"},
		Rules:      []Rule{rule},
	})
}

// TestAFilteredRuleCompilesItsFilterOntoTheGrant. The rule states the
// restriction and storage enforces it, so a filter the compilation drops is one
// nothing else will apply.
func TestAFilteredRuleCompilesItsFilterOntoTheGrant(t *testing.T) {
	grants, err := finalOnlyPolicy(t).Compile(readRequest("Observation", Parameters{"patient": "pat-1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 1 {
		t.Fatalf("compiled %d grants, want one", len(grants))
	}

	if grants[0].Filter == nil {
		t.Fatal("the compiled grant carries no filter, so the rule's restriction reaches nothing")
	}

	want := mustElementFilter(t, "status", "final")
	if !grants[0].Filter.Equal(want) {
		t.Errorf("the compiled grant carries %q, want %q", grants[0].Filter, want)
	}

	// The compartment is unaffected: the two narrow different things.
	if grants[0].Compartment == nil || *grants[0].Compartment !=
		(storage.Compartment{Type: "Patient", ID: "pat-1"}) {
		t.Errorf("the compiled grant's compartment is %v, want the bound patient", grants[0].Compartment)
	}
}

// TestAnUnfilteredRuleCompilesToAGrantWithNoFilter, because absent is what
// storage reads as unrestricted in that dimension.
func TestAnUnfilteredRuleCompilesToAGrantWithNoFilter(t *testing.T) {
	grants, err := ownChartPolicy(t).Compile(readRequest("Observation", Parameters{"patient": "pat-1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for _, grant := range grants {
		if grant.Filter != nil {
			t.Errorf("an unfiltered rule compiled a grant carrying %q", grant.Filter)
		}
	}
}

// TestACompiledGrantCannotRewriteThePolicyItCameFrom. Grant.Filter is an
// exported pointer, so handing out the rule's own would let anyone holding one
// compiled Grant change what every later compilation of that policy means.
func TestACompiledGrantCannotRewriteThePolicyItCameFrom(t *testing.T) {
	policy := finalOnlyPolicy(t)
	request := readRequest("Observation", Parameters{"patient": "pat-1"})

	first, err := policy.Compile(request)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	*first[0].Filter = mustElementFilter(t, "status", "preliminary")

	second, err := policy.Compile(request)
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}

	if want := mustElementFilter(t, "status", "final"); !second[0].Filter.Equal(want) {
		t.Errorf("a compiled grant rewrote its policy: recompiling gives %q, want %q", second[0].Filter, want)
	}
}

// TestAFilterDoesNotLiftTheGuardOnUnrestrictedClinicalRules. Narrowing by
// status says nothing about whose data it is, so a rule filtered to final
// Observations still reaches every patient and LNK-5 still refuses it.
func TestAFilterDoesNotLiftTheGuardOnUnrestrictedClinicalRules(t *testing.T) {
	_, err := NewUnrestrictedRule(storage.KindFHIR, "Observation", storage.ActionRead)
	if !errors.Is(err, ErrUnrestrictedClinicalType) {
		t.Fatalf("an unrestricted clinical rule: err = %v, want %v", err, ErrUnrestrictedClinicalType)
	}

	// The guard is on authoring the rule, so there is no order of calls that
	// reaches a filtered unrestricted clinical rule: the rule never exists.
	directory, err := NewUnrestrictedRule(storage.KindFHIR, "Organization", storage.ActionRead)
	if err != nil {
		t.Fatalf("an unrestricted rule over directory data: %v", err)
	}

	filtered := directory.WithFilter(mustElementFilter(t, "active", "true"))
	if _, carried := filtered.Filter(); !carried {
		t.Error("an unrestricted rule dropped the filter narrowing it")
	}

	if _, restricted := filtered.Subject(); restricted {
		t.Error("a filter turned an unrestricted rule into a compartment-restricted one")
	}
}

// TestWithFilterLeavesTheRuleItWasCalledOnAlone, so a rule shared between
// policies cannot acquire a restriction from whichever one narrowed it.
func TestWithFilterLeavesTheRuleItWasCalledOnAlone(t *testing.T) {
	original := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient"))

	narrowed := original.WithFilter(mustElementFilter(t, "status", "final"))

	if _, carried := original.Filter(); carried {
		t.Error("narrowing one rule changed the rule it was derived from")
	}

	if _, carried := narrowed.Filter(); !carried {
		t.Error("the narrowed rule carries no filter")
	}
}

// TestARuleHandsOutACopyOfItsFilter, so reading a rule cannot rewrite it.
func TestARuleHandsOutACopyOfItsFilter(t *testing.T) {
	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")).
		WithFilter(mustElementFilter(t, "category.coding.code", "vital-signs", "laboratory"))

	held, carried := rule.Filter()
	if !carried {
		t.Fatal("the rule carries no filter")
	}

	values := held.Values()
	values[0] = "everything"

	again, _ := rule.Filter()
	if !again.Equal(mustElementFilter(t, "category.coding.code", "vital-signs", "laboratory")) {
		t.Errorf("a reader widened the rule's filter to %q", again)
	}
}

func mustRuleProjection(t *testing.T, elements ...string) storage.Projection {
	t.Helper()

	projection, err := storage.NewProjection(elements...)
	if err != nil {
		t.Fatalf("NewProjection(%v): %v", elements, err)
	}

	return projection
}

// TestAProjectedRuleCompilesItsProjectionOntoTheGrant. The rule states how much
// it returns and storage applies it, so a projection the compilation drops is
// one nothing else will apply.
func TestAProjectedRuleCompilesItsProjectionOntoTheGrant(t *testing.T) {
	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")).
		Returning(mustRuleProjection(t, "status", "valueQuantity"))

	policy := mustPolicy(t, PolicyConfig{
		Project: clinic, ID: "pol_summary",
		Parameters: []ParameterName{"patient"}, Rules: []Rule{rule},
	})

	grants, err := policy.Compile(readRequest("Observation", Parameters{"patient": "pat-1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 1 || grants[0].Projection == nil {
		t.Fatalf("the rule's projection did not reach the grant: %v", grants)
	}

	if want := mustRuleProjection(t, "status", "valueQuantity"); !grants[0].Projection.Equal(want) {
		t.Errorf("the grant returns %q, want %q", grants[0].Projection, want)
	}
}

// TestAnUnprojectedRuleCompilesToAGrantReturningEverything, because absent is
// what storage reads as unrestricted in that dimension.
func TestAnUnprojectedRuleCompilesToAGrantReturningEverything(t *testing.T) {
	grants, err := ownChartPolicy(t).Compile(readRequest("Observation", Parameters{"patient": "pat-1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for _, grant := range grants {
		if grant.Projection != nil {
			t.Errorf("an unprojected rule compiled a grant returning only %q", grant.Projection)
		}
	}
}

// TestACompiledGrantCannotRewriteThePolicysProjection, for the reason it cannot
// rewrite its filter: the field is exported and the pointer would be shared.
func TestACompiledGrantCannotRewriteThePolicysProjection(t *testing.T) {
	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")).
		Returning(mustRuleProjection(t, "status"))

	policy := mustPolicy(t, PolicyConfig{
		Project: clinic, ID: "pol_summary",
		Parameters: []ParameterName{"patient"}, Rules: []Rule{rule},
	})

	request := readRequest("Observation", Parameters{"patient": "pat-1"})

	first, err := policy.Compile(request)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	*first[0].Projection = mustRuleProjection(t, "note")

	second, err := policy.Compile(request)
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}

	if want := mustRuleProjection(t, "status"); !second[0].Projection.Equal(want) {
		t.Errorf("a compiled grant rewrote its policy: recompiling returns %q, want %q",
			second[0].Projection, want)
	}
}

// TestReturningLeavesTheRuleItWasCalledOnAlone, so a rule shared between
// policies cannot acquire a projection from whichever one narrowed it.
func TestReturningLeavesTheRuleItWasCalledOnAlone(t *testing.T) {
	original := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient"))

	narrowed := original.Returning(mustRuleProjection(t, "status"))

	if _, carried := original.Projection(); carried {
		t.Error("narrowing one rule changed the rule it was derived from")
	}

	if _, carried := narrowed.Projection(); !carried {
		t.Error("the narrowed rule returns everything")
	}
}

// TestARuleCanCarryBothRestrictionsAtOnce, because they narrow different things:
// which resources are reached, and how much of one is returned.
func TestARuleCanCarryBothRestrictionsAtOnce(t *testing.T) {
	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")).
		WithFilter(mustElementFilter(t, "status", "final")).
		Returning(mustRuleProjection(t, "status", "valueQuantity"))

	policy := mustPolicy(t, PolicyConfig{
		Project: clinic, ID: "pol_final_summary",
		Parameters: []ParameterName{"patient"}, Rules: []Rule{rule},
	})

	grants, err := policy.Compile(readRequest("Observation", Parameters{"patient": "pat-1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 1 {
		t.Fatalf("compiled %d grants, want one", len(grants))
	}

	switch {
	case grants[0].Compartment == nil:
		t.Error("the grant lost its compartment")
	case grants[0].Filter == nil:
		t.Error("the grant lost its filter")
	case grants[0].Projection == nil:
		t.Error("the grant lost its projection")
	}
}
