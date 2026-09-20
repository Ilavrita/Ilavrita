package authz_test

import (
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// granting parses the scopes an app was given, failing the test on one this
// build refuses — the refusals have their own test.
func granting(t *testing.T, stated ...string) []authz.SmartScope {
	t.Helper()

	held := make([]authz.SmartScope, 0, len(stated))

	for _, one := range stated {
		parsed, err := authz.ParseScope(one)
		if err != nil {
			t.Fatalf("parse %q: %v", one, err)
		}

		held = append(held, parsed)
	}

	return held
}

// reading is one Grant on one type, as a policy would have produced it.
func reading(resourceType storage.ResourceType, action storage.Action) storage.Grant {
	return storage.Grant{
		Project: "prj_a", Kind: storage.KindFHIR,
		Type: resourceType, Action: action, Source: storage.SourceMembership,
	}
}

// confinedTo is that Grant bounded to one patient's compartment.
func confinedTo(grant storage.Grant, patient storage.LogicalID) storage.Grant {
	grant.Compartment = &storage.Compartment{Type: "Patient", ID: patient}

	return grant
}

// TestAnAppNeverHoldsMoreThanThePersonWhoApprovedIt.
//
// This is the whole security argument. An app acting for somebody must not
// reach anything that somebody cannot reach, so what comes out is what went in,
// narrowed — never anything built from the scope strings themselves.
func TestAnAppNeverHoldsMoreThanThePersonWhoApprovedIt(t *testing.T) {
	// A person who may read and search Observations, and nothing else.
	person := storage.NewScope(
		reading("Observation", storage.ActionRead),
		reading("Observation", storage.ActionSearch),
	)

	// An app asking for everything on every type gets what the person has.
	held := authz.Narrow(person, granting(t, "user/*.*"), "")

	if len(held.Grants()) != 2 {
		t.Fatalf("user/*.* produced %d grant(s), want the 2 the person holds", len(held.Grants()))
	}

	for _, grant := range held.Grants() {
		if grant.Type != "Observation" {
			t.Errorf("an app reached %s, which the person cannot", grant.Type)
		}
	}
}

// TestAStarExpandsAgainstWhatThePersonHoldsRatherThanTheServedTypes, because a
// Scope carrying a thousand Grants compiles into a statement with a thousand
// arms — and the person could not reach those types anyway.
func TestAStarExpandsAgainstWhatThePersonHoldsRatherThanTheServedTypes(t *testing.T) {
	person := storage.NewScope(
		reading("Observation", storage.ActionRead),
		reading("Condition", storage.ActionRead),
	)

	held := authz.Narrow(person, granting(t, "user/*.read"), "")

	if len(held.Grants()) != 2 {
		t.Errorf("user/*.read produced %d grant(s), want 2", len(held.Grants()))
	}
}

// TestAnAppIsNarrowedToWhatItAskedFor, which is the other direction: a person
// who may do more than the app asked for does not lend it the rest.
func TestAnAppIsNarrowedToWhatItAskedFor(t *testing.T) {
	person := storage.NewScope(
		reading("Observation", storage.ActionRead),
		reading("Observation", storage.ActionWrite),
		reading("Patient", storage.ActionRead),
	)

	held := authz.Narrow(person, granting(t, "user/Observation.rs"), "")

	if len(held.Grants()) != 1 {
		t.Fatalf("produced %d grant(s), want the one read on Observation", len(held.Grants()))
	}

	if grant := held.Grants()[0]; grant.Type != "Observation" || grant.Action != storage.ActionRead {
		t.Errorf("produced %s/%s", grant.Type, grant.Action)
	}
}

// TestAPatientScopeWithNoLaunchAuthorizesNothing.
//
// Not everything, and not the person's own. The scope names a patient and there
// is not one, so there is no set of resources it describes — and this is the
// single most dangerous place to be permissive.
func TestAPatientScopeWithNoLaunchAuthorizesNothing(t *testing.T) {
	person := storage.NewScope(reading("Observation", storage.ActionRead))

	if held := authz.Narrow(person, granting(t, "patient/Observation.read"), ""); len(held.Grants()) != 0 {
		t.Errorf("a patient scope with no launch produced %d grant(s)", len(held.Grants()))
	}
}

// TestAPatientScopeConfinesAnUnconfinedGrant, which is what makes one worth
// asking for: a clinician who may read every Observation, in an app launched
// for one patient, reads that patient's.
func TestAPatientScopeConfinesAnUnconfinedGrant(t *testing.T) {
	person := storage.NewScope(reading("Observation", storage.ActionRead))

	held := authz.Narrow(person, granting(t, "patient/Observation.read"), "pat-1")

	if len(held.Grants()) != 1 {
		t.Fatalf("produced %d grant(s), want 1", len(held.Grants()))
	}

	compartment := held.Grants()[0].Compartment
	if compartment == nil {
		t.Fatal("the grant was not confined to the launch patient")
	}

	if compartment.Type != "Patient" || compartment.ID != "pat-1" {
		t.Errorf("confined to %s/%s", compartment.Type, compartment.ID)
	}
}

// TestAPatientScopeNamingAnotherPatientAuthorizesNothing.
//
// The person is confined to one patient and the app asked for another. Neither
// is the answer: the app may not have the one it asked for, and must not be
// handed the one it did not.
func TestAPatientScopeNamingAnotherPatientAuthorizesNothing(t *testing.T) {
	person := storage.NewScope(confinedTo(reading("Observation", storage.ActionRead), "pat-mine"))

	held := authz.Narrow(person, granting(t, "patient/Observation.read"), "pat-theirs")

	if len(held.Grants()) != 0 {
		t.Fatalf("produced %d grant(s), want none", len(held.Grants()))
	}

	// And launching for the patient they are confined to is the ordinary case.
	same := authz.Narrow(person, granting(t, "patient/Observation.read"), "pat-mine")
	if len(same.Grants()) != 1 {
		t.Errorf("the person's own patient produced %d grant(s)", len(same.Grants()))
	}
}

// TestAnAppGrantedNothingHoldsNothing, rather than holding whatever the person
// does.
func TestAnAppGrantedNothingHoldsNothing(t *testing.T) {
	person := storage.NewScope(reading("Observation", storage.ActionRead))

	if held := authz.Narrow(person, nil, "pat-1"); len(held.Grants()) != 0 {
		t.Errorf("an app granted nothing holds %d grant(s)", len(held.Grants()))
	}
}

// TestNoScopeNamesAPlatformResource. SMART is about clinical data, and the
// platform tables hold logins and policies: a scope that reached one would be a
// scope that reached the authorization system through the thing it authorizes.
func TestNoScopeNamesAPlatformResource(t *testing.T) {
	platform := storage.Grant{
		Project: "prj_a", Kind: storage.KindPlatform,
		Type: "AccessPolicy", Action: storage.ActionRead,
	}

	held := authz.Narrow(storage.NewScope(platform), granting(t, "user/*.*"), "")

	if len(held.Grants()) != 0 {
		t.Errorf("a scope reached a platform resource")
	}
}

// TestWhatThisBuildWillNotGrant.
//
// SMART lets a server grant fewer scopes than were asked for, and requires a
// client to read what it was granted. Refusing is visible; granting something
// adjacent and calling it the same is not.
func TestWhatThisBuildWillNotGrant(t *testing.T) {
	for named, stated := range map[string]string{
		"a backend service, which nothing here can authenticate":      "system/Observation.read",
		"a type this server does not serve":                           "user/Appointment.read",
		"a search restriction this build does not apply":              "patient/Observation.rs?category=lab",
		"creating without updating, which this build cannot separate": "user/Observation.c",
		"updating without creating, likewise":                         "user/Observation.u",
	} {
		if _, err := authz.ParseScope(stated); !errors.Is(err, authz.ErrUnsupportedScope) {
			t.Errorf("%s: %q was not refused (%v)", named, stated, err)
		}
	}

	for named, stated := range map[string]string{
		"no context":               "Observation.read",
		"no access":                "user/Observation",
		"a made-up letter":         "user/Observation.z",
		"an empty access":          "user/Observation.",
		"a context nobody defines": "everyone/Observation.read",
	} {
		if _, err := authz.ParseScope(stated); !errors.Is(err, authz.ErrMalformedScope) {
			t.Errorf("%s: %q was not refused as malformed (%v)", named, stated, err)
		}
	}
}

// TestCreatingAndUpdatingTogetherIsOneWrite, which is what this build has.
func TestCreatingAndUpdatingTogetherIsOneWrite(t *testing.T) {
	held := granting(t, "user/Observation.cruds")

	if len(held) != 1 {
		t.Fatalf("parsed %d scope(s)", len(held))
	}

	for _, want := range []storage.Action{
		storage.ActionWrite, storage.ActionRead, storage.ActionDelete, storage.ActionSearch,
	} {
		if !containsAction(held[0].Actions, want) {
			t.Errorf("cruds does not cover %s", want)
		}
	}
}

// TestV1ReadCoversTheWholeReadSide, because there is no v1 way to ask for a
// history without it and a client that could read a resource could always read
// how it got that way.
func TestV1ReadCoversTheWholeReadSide(t *testing.T) {
	held := granting(t, "user/Observation.read")[0]

	for _, want := range []storage.Action{
		storage.ActionRead, storage.ActionSearch, storage.ActionHistory,
	} {
		if !containsAction(held.Actions, want) {
			t.Errorf("v1 read does not cover %s", want)
		}
	}

	if containsAction(held.Actions, storage.ActionWrite) {
		t.Error("v1 read covers writing")
	}
}

func containsAction(held []storage.Action, wanted storage.Action) bool {
	for _, one := range held {
		if one == wanted {
			return true
		}
	}

	return false
}
