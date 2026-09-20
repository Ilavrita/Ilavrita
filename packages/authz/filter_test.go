package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"

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

// TestARuleNamingSeveralSubjectsMintsOneGrantEach. Naming a set is the same
// restriction as one rule per id, written once: storage holds Grants together,
// so what a set reaches is exactly what the separate rules reached.
func TestARuleNamingSeveralSubjectsMintsOneGrantEach(t *testing.T) {
	roster, err := LiteralSubject("Patient", "pat-1", "pat-2", "pat-3")
	if err != nil {
		t.Fatalf("LiteralSubject: %v", err)
	}

	policy := mustPolicy(t, PolicyConfig{
		Project: clinic, ID: "pol_roster",
		Rules: []Rule{mustRule(t, "Observation", storage.ActionRead, roster)},
	})

	grants, err := policy.Compile(readRequest("Observation", nil))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 3 {
		t.Fatalf("a rule naming three subjects compiled %d grants, want one each", len(grants))
	}

	reached := map[storage.Compartment]bool{}

	for _, grant := range grants {
		if grant.Compartment == nil {
			t.Fatal("a rule naming subjects compiled an unconfined grant")
		}

		reached[*grant.Compartment] = true
	}

	for _, id := range []storage.LogicalID{"pat-1", "pat-2", "pat-3"} {
		if !reached[storage.Compartment{Type: "Patient", ID: id}] {
			t.Errorf("no grant reaches %s", id)
		}
	}
}

// TestASetCompilesToWhatSeparateRulesCompileTo, which is the claim that makes a
// set an ergonomic change rather than a change in what a policy can say.
func TestASetCompilesToWhatSeparateRulesCompileTo(t *testing.T) {
	roster, err := LiteralSubject("Patient", "pat-1", "pat-2")
	if err != nil {
		t.Fatalf("LiteralSubject: %v", err)
	}

	together := mustPolicy(t, PolicyConfig{
		Project: clinic, ID: "pol_together",
		Rules: []Rule{mustRule(t, "Observation", storage.ActionRead, roster)},
	})

	var separate []Rule

	for _, id := range []storage.LogicalID{"pat-1", "pat-2"} {
		subject, err := LiteralSubject("Patient", id)
		if err != nil {
			t.Fatalf("LiteralSubject(%s): %v", id, err)
		}

		separate = append(separate, mustRule(t, "Observation", storage.ActionRead, subject))
	}

	apart := mustPolicy(t, PolicyConfig{Project: clinic, ID: "pol_apart", Rules: separate})

	one, err := together.Compile(readRequest("Observation", nil))
	if err != nil {
		t.Fatalf("compile the set: %v", err)
	}

	many, err := apart.Compile(readRequest("Observation", nil))
	if err != nil {
		t.Fatalf("compile the separate rules: %v", err)
	}

	if len(one) != len(many) {
		t.Fatalf("the set compiled %d grants and the separate rules %d", len(one), len(many))
	}

	for index := range one {
		if *one[index].Compartment != *many[index].Compartment {
			t.Errorf("grant %d: the set reaches %v, the separate rules %v",
				index, *one[index].Compartment, *many[index].Compartment)
		}
	}
}

// TestASubjectSetRefusesWhatNamesNoSubject, because a set nobody filled would
// restrict a rule to nothing while looking like a restriction to something.
func TestASubjectSetRefusesWhatNamesNoSubject(t *testing.T) {
	refused := map[string][]storage.LogicalID{
		"no id at all":           nil,
		"an empty id":            {""},
		"an empty id among many": {"pat-1", ""},
	}

	for name, ids := range refused {
		if _, err := LiteralSubject("Patient", ids...); !errors.Is(err, ErrMissingSubject) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrMissingSubject)
		}
	}
}

// TestARepeatedSubjectIsNamedOnce, so a roster with a duplicate costs one
// predicate rather than two that say the same thing.
func TestARepeatedSubjectIsNamedOnce(t *testing.T) {
	roster, err := LiteralSubject("Patient", "pat-1", "pat-2", "pat-1")
	if err != nil {
		t.Fatalf("LiteralSubject: %v", err)
	}

	if got := roster.IDs(); len(got) != 2 {
		t.Errorf("a roster naming pat-1 twice holds %v", got)
	}
}

// TestAParameterStillResolvesToOneSubject. A binding supplies one value per
// name, so the set shape does not change what a parameterized rule reaches.
func TestAParameterStillResolvesToOneSubject(t *testing.T) {
	grants, err := ownChartPolicy(t).Compile(readRequest("Observation", Parameters{"patient": "pat-9"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 1 {
		t.Fatalf("a parameterized rule compiled %d grants, want one", len(grants))
	}

	if want := (storage.Compartment{Type: "Patient", ID: "pat-9"}); *grants[0].Compartment != want {
		t.Errorf("the grant reaches %v, want %v", *grants[0].Compartment, want)
	}
}

// TestAPrincipalNobodyHoldsBuildsNoScope.
//
// Everything that resolves standing rests on this: a request naming a principal
// that is not there must reach nothing, rather than reaching whatever a Scope
// built from an empty name would. The notifier leans on it in particular — a
// subscription whose standing has been withdrawn asks for a Scope with nothing
// behind it, and must get one that authorizes nothing.
func TestAPrincipalNobodyHoldsBuildsNoScope(t *testing.T) {
	empty := map[string]project.PrincipalRef{
		"nothing at all":         {},
		"a kind with no id":      {Kind: project.PrincipalUser},
		"an id with no kind":     {ID: "usr_1"},
		"a kind nobody declared": {Kind: project.PrincipalKind("wishful"), ID: "usr_1"},
	}

	for name, principal := range empty {
		scope, err := BuildScope(t.Context(), Request{
			Principal: principal,
			Project:   clinic,
			Kind:      storage.KindFHIR,
			Type:      "Observation",
			Action:    storage.ActionRead,
			Now:       time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC),
			Launch:    NoLaunch(),
			Resolvers: Resolvers{
				Memberships: standingNobodyHolds{},
				Projects:    activeEverywhere{},
			},
		})

		// Either answer is safe. What must not happen is a Scope that reaches
		// something.
		if err == nil && !scope.IsEmpty() {
			t.Errorf("%s built a Scope holding %d grants", name, len(scope.Grants()))
		}
	}
}

// standingNobodyHolds answers every lookup with no membership, which is what a
// withdrawn one looks like.
type standingNobodyHolds struct{}

func (standingNobodyHolds) Membership(
	context.Context, project.ID, project.PrincipalRef,
) (project.Membership, bool, error) {
	return project.Membership{}, false, nil
}

// activeEverywhere reports every Project as one that serves requests.
type activeEverywhere struct{}

func (activeEverywhere) State(context.Context, project.ID) (project.State, error) {
	return project.StateActive, nil
}
