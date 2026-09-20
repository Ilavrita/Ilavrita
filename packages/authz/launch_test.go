package authz_test

import (
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// launched is the context a token endpoint would have stored, failing the test
// on one this build refuses — the refusals have their own test.
func launched(t *testing.T, patient, scopes string) project.LaunchContext {
	t.Helper()

	held, err := project.NewLaunchContext(patient, scopes)
	if err != nil {
		t.Fatalf("launch context %q for %q: %v", scopes, patient, err)
	}

	return held
}

// TestASessionNobodysAppHoldsIsNarrowedByNothing.
//
// An ordinary login asked for no subset of anything, so there is no restriction
// to apply and the person reaches exactly what their own standing reaches.
func TestASessionNobodysAppHoldsIsNarrowedByNothing(t *testing.T) {
	person := storage.NewScope(
		reading("Observation", storage.ActionRead),
		reading("Condition", storage.ActionRead),
	)

	launch, err := authz.ParseLaunch(project.LaunchContext{})
	if err != nil {
		t.Fatalf("parse an absent launch context: %v", err)
	}

	if launch.App() {
		t.Error("a session with no launch context was read as an app's")
	}

	if got := len(launch.Narrow(person).Grants()); got != 2 {
		t.Errorf("an ordinary login kept %d of its 2 grants", got)
	}
}

// TestAnAppsSessionIsNarrowedToWhatTheAppWasGranted, which is the whole point of
// storing the grant with the session rather than trusting the token.
func TestAnAppsSessionIsNarrowedToWhatTheAppWasGranted(t *testing.T) {
	person := storage.NewScope(
		reading("Observation", storage.ActionRead),
		reading("Condition", storage.ActionRead),
	)

	launch, err := authz.ParseLaunch(launched(t, "", "user/Observation.read"))
	if err != nil {
		t.Fatalf("parse a launch context: %v", err)
	}

	if !launch.App() {
		t.Fatal("a session carrying granted scopes was not read as an app's")
	}

	held := launch.Narrow(person).Grants()
	if len(held) != 1 {
		t.Fatalf("the app kept %d grants, want only the Observation read", len(held))
	}

	if held[0].Type != "Observation" {
		t.Errorf("the app reached %s, which it was not granted", held[0].Type)
	}
}

// TestALaunchPatientConfinesAnAppToThatPatient, taking the patient from the
// launch rather than from the scope: the scope says "the patient", the launch
// says which one.
func TestALaunchPatientConfinesAnAppToThatPatient(t *testing.T) {
	person := storage.NewScope(reading("Observation", storage.ActionRead))

	launch, err := authz.ParseLaunch(launched(t, "pat_7", "patient/Observation.read"))
	if err != nil {
		t.Fatalf("parse a launch context: %v", err)
	}

	if launch.Patient() != "pat_7" {
		t.Errorf("launch patient is %q, want pat_7", launch.Patient())
	}

	held := launch.Narrow(person).Grants()
	if len(held) != 1 {
		t.Fatalf("the app kept %d grants, want 1", len(held))
	}

	if held[0].Compartment == nil {
		t.Fatal("a patient-context scope left the grant unconfined")
	}

	if held[0].Compartment.ID != "pat_7" {
		t.Errorf("the app was confined to %s, not the patient it was launched for",
			held[0].Compartment.ID)
	}
}

// TestOneScopeThisBuildRefusesFailsTheWholeLaunch.
//
// A scope quietly dropped here is a restriction quietly dropped, so what
// survives is wider than what the app was granted. The whole launch fails
// instead, and the request is denied rather than over-served.
func TestOneScopeThisBuildRefusesFailsTheWholeLaunch(t *testing.T) {
	// The first is honoured; the second is a search restriction this build does
	// not apply, and dropping it would hand the app every Condition.
	context := launched(t, "pat_7", "patient/Observation.read patient/Condition.rs?category=problem")

	launch, err := authz.ParseLaunch(context)
	if !errors.Is(err, authz.ErrUnsupportedScope) {
		t.Fatalf("error: got %v, want ErrUnsupportedScope", err)
	}

	if launch.App() {
		t.Error("a launch that failed to parse still reported an app")
	}

	// And the value that comes back is the one BuildScope refuses, so a caller
	// ignoring the error still cannot serve the request.
	person := storage.NewScope(reading("Observation", storage.ActionRead))
	if got := len(launch.Narrow(person).Grants()); got != 0 {
		t.Errorf("a launch that failed to parse narrowed to %d grants, want 0", got)
	}
}
