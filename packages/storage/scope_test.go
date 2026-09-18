package storage

import "testing"

func patientRead(project ProjectID) Grant {
	return Grant{
		Project: project, Kind: KindFHIR, Type: "Patient",
		Action: ActionRead, Source: SourceMembership,
	}
}

func TestZeroScopeAuthorizesNothing(t *testing.T) {
	var scope Scope

	if scope.Allows("prj_a", KindFHIR, "Patient", ActionRead) {
		t.Error("the zero Scope must deny; a forgotten Scope has to fail closed")
	}
}

func TestZeroScopeReachesNoProject(t *testing.T) {
	var scope Scope

	if len(scope.Projects()) != 0 {
		t.Error("the zero Scope must reach no Project")
	}
}

func TestGrantsCannotBeAppendedThroughTheAccessor(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	escaped := scope.Grants()
	escaped[0].Project = "prj_b"

	if scope.Allows("prj_b", KindFHIR, "Patient", ActionRead) {
		t.Error("mutating the returned slice widened the Scope")
	}
}

func TestConstructorCopiesItsInput(t *testing.T) {
	grants := []Grant{patientRead("prj_a")}
	scope := NewScope(grants...)

	grants[0].Project = "prj_b"

	if scope.Allows("prj_b", KindFHIR, "Patient", ActionRead) {
		t.Error("mutating the caller's slice widened the Scope")
	}
}

func TestAllowsDistinguishesResourceType(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	if scope.Allows("prj_a", KindFHIR, "Observation", ActionRead) {
		t.Error("a Patient grant must not authorize Observation; that is how _include widens a read")
	}
}

func TestAllowsDistinguishesAction(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	if scope.Allows("prj_a", KindFHIR, "Patient", ActionWrite) {
		t.Error("a read grant must not authorize a write")
	}
}

func TestAllowsDistinguishesKind(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	if scope.Allows("prj_a", KindPlatform, "Patient", ActionRead) {
		t.Error("a FHIR grant must not authorize a platform resource")
	}
}

func TestAllowsDistinguishesProject(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	if scope.Allows("prj_b", KindFHIR, "Patient", ActionRead) {
		t.Error("a grant in one Project must not authorize another")
	}
}

func TestNarrowOnlyRemoves(t *testing.T) {
	scope := NewScope(
		patientRead("prj_a"),
		Grant{Project: "prj_a", Kind: KindFHIR, Type: "Patient", Action: ActionWrite},
	)

	narrowed := scope.Narrow(KindFHIR, ActionRead)

	if narrowed.Allows("prj_a", KindFHIR, "Patient", ActionWrite) {
		t.Error("Narrow must not retain a grant outside the requested action")
	}
}

func TestNarrowToAnUnheldActionDeniesEverything(t *testing.T) {
	scope := NewScope(patientRead("prj_a"))

	if !scope.Narrow(KindFHIR, ActionDelete).IsEmpty() {
		t.Error("narrowing to an action the principal does not hold must empty the Scope")
	}
}

func TestNewResourceKeyRejectsAnEmptyProject(t *testing.T) {
	if _, err := NewResourceKey("", "Patient", "123"); err == nil {
		t.Error("an empty Project must be rejected, not carried into a query")
	}
}

func TestNewResourceKeyAcceptsACompleteKey(t *testing.T) {
	key, err := NewResourceKey("prj_a", "Patient", "123")
	if err != nil {
		t.Fatalf("a complete key was rejected: %v", err)
	}

	if key.Project != "prj_a" {
		t.Errorf("Project = %q, want prj_a", key.Project)
	}
}

// TestAdmitsIsNarrowerThanAllows. Allows answers whether a decision was
// authorized at all; Admits answers whether the authorization is unconditional,
// which is the only thing that can be said about a resource there is no row to
// evaluate a narrowing against.
func TestAdmitsIsNarrowerThanAllows(t *testing.T) {
	const (
		owner = ProjectID("prj_a")
		named = ResourceType("StructureDefinition")
	)

	unrestricted := Grant{Project: owner, Kind: KindFHIR, Type: named, Action: ActionRead}

	compartment := unrestricted
	compartment.Compartment = &Compartment{Type: "Patient", ID: "pat-1"}

	filter, err := NewFilter("status", ComparatorEqual, "active")
	if err != nil {
		t.Fatalf("author a filter: %v", err)
	}

	filtered := unrestricted
	filtered.Filter = &filter

	projection, err := NewProjection("id")
	if err != nil {
		t.Fatalf("author a projection: %v", err)
	}

	projected := unrestricted
	projected.Projection = &projection

	for described, held := range map[string]struct {
		grant  Grant
		admits bool
	}{
		"narrowing nothing": {unrestricted, true},
		"by a compartment":  {compartment, false},
		"by a filter":       {filtered, false},
		"by a projection":   {projected, false},
	} {
		scope := NewScope(held.grant)

		// Every one of them authorizes the decision; only one admits every
		// resource of the type.
		if !scope.Allows(owner, KindFHIR, named, ActionRead) {
			t.Errorf("a grant %s does not allow the read it is for", described)
		}

		if admits := scope.Admits(owner, KindFHIR, named, ActionRead); admits != held.admits {
			t.Errorf("a grant %s admits=%v, want %v", described, admits, held.admits)
		}
	}

	// And the triple still has to match, the same as it does for Allows.
	scope := NewScope(unrestricted)

	for described, asked := range map[string]struct {
		project ProjectID
		named   ResourceType
		action  Action
	}{
		"another Project": {"prj_b", named, ActionRead},
		"another type":    {owner, "Observation", ActionRead},
		"another action":  {owner, named, ActionWrite},
	} {
		if scope.Admits(asked.project, KindFHIR, asked.named, asked.action) {
			t.Errorf("it admits %s", described)
		}
	}
}
