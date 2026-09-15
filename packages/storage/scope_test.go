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
