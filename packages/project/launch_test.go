package project

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// launchedAt is the instant every session in this file is minted at.
var launchedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// TestALaunchContextGrantingNothingIsRefused.
//
// A session that is an app's and says nothing about what the app was granted
// would be the same value as a session no app holds, and that one is narrowed
// by nothing. So the empty grant is unrepresentable rather than merely unusual.
func TestALaunchContextGrantingNothingIsRefused(t *testing.T) {
	for name, scopes := range map[string]string{
		"nothing at all": "",
		"blank":          "   ",
		"tabs and lines": "\t\n ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewLaunchContext("pat_7", scopes); !errors.Is(err, ErrInvalidLaunch) {
				t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
			}
		})
	}
}

// TestAStoredLaunchContextIsTheScopesAsGranted, folded to single spaces so the
// text the token response reported and the text this server narrows by compare
// equal rather than merely mean the same.
func TestAStoredLaunchContextIsTheScopesAsGranted(t *testing.T) {
	held, err := NewLaunchContext(" pat_7 ", "  patient/Observation.read\t patient/Condition.read\n")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	if want := "patient/Observation.read patient/Condition.read"; held.Scopes() != want {
		t.Errorf("scopes are %q, want %q", held.Scopes(), want)
	}

	if held.Patient() != "pat_7" {
		t.Errorf("patient is %q, want pat_7", held.Patient())
	}

	if held.IsZero() {
		t.Error("a context granting two scopes reported itself as empty")
	}
}

// TestALaunchContextRefusesWhatCannotBeOneLogicalID, because a patient carrying
// a space is two ids or none, and either way it is not the one the app was
// launched for.
func TestALaunchContextRefusesWhatCannotBeOneLogicalID(t *testing.T) {
	if _, err := NewLaunchContext("pat_7 pat_8", "patient/Observation.read"); !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestAScopeStringLongerThanASessionCarriesIsRefused, which is a caller filling
// the table rather than an app describing itself.
func TestAScopeStringLongerThanASessionCarriesIsRefused(t *testing.T) {
	long := strings.Repeat("user/Observation.read ", 400)

	if _, err := NewLaunchContext("", long); !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestAnOrdinaryLoginCarriesNoLaunchContext, so nothing narrows it: nobody
// asked for a subset of what the person already holds.
func TestAnOrdinaryLoginCarriesNoLaunchContext(t *testing.T) {
	session, _, err := IssueSession("prj_a", "ses_1", "usr_1", "mbr_1", launchedAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	if !session.Launch().IsZero() {
		t.Errorf("an ordinary login carried %v", session.Launch())
	}
}

// TestAnAppsSessionCannotBeIssuedWithoutSayingWhatItWasGranted.
//
// The launch context is an argument rather than something attached afterwards,
// so there is no order of calls that mints an app's session nothing narrows.
func TestAnAppsSessionCannotBeIssuedWithoutSayingWhatItWasGranted(t *testing.T) {
	_, _, err := IssueAppSession(
		"prj_a", "ses_1", "usr_1", "mbr_1", LaunchContext{}, "", launchedAt, time.Hour, rand.Reader)

	if !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestAnAppsSessionCarriesWhatItWasGranted from minting through to the value a
// store would write.
func TestAnAppsSessionCarriesWhatItWasGranted(t *testing.T) {
	granted, err := NewLaunchContext("pat_7", "patient/Observation.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	session, _, err := IssueAppSession(
		"prj_a", "ses_1", "usr_1", "mbr_1", granted, "", launchedAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueAppSession: %v", err)
	}

	if session.Launch() != granted {
		t.Errorf("the session carries %v, want %v", session.Launch(), granted)
	}
}

// TestARowNamingAPatientAndGrantingNothingIsRefused.
//
// It is the dangerous shape: it would rebuild as an ordinary login, which is
// narrowed by nothing, while looking like an app's session to anyone reading
// the table. The schema refuses it too; this is the same refusal in the one
// place a row becomes a Session.
func TestARowNamingAPatientAndGrantingNothingIsRefused(t *testing.T) {
	_, err := NewSession("prj_a", SessionRecord{
		ID: "ses_1", User: "usr_1", Membership: "mbr_1",
		Digest: "abc", State: SessionActive,
		LaunchPatient: "pat_7",
		CreatedAt:     launchedAt, ExpiresAt: launchedAt.Add(time.Hour),
	})

	if !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestAPersistedLaunchContextRebuildsThroughItsOwnConstructor, so a row nothing
// could have written is refused rather than served.
func TestAPersistedLaunchContextRebuildsThroughItsOwnConstructor(t *testing.T) {
	session, err := NewSession("prj_a", SessionRecord{
		ID: "ses_1", User: "usr_1", Membership: "mbr_1",
		Digest: "abc", State: SessionActive,
		LaunchPatient: "pat_7", GrantedScopes: "patient/Observation.read",
		CreatedAt: launchedAt, ExpiresAt: launchedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	held := session.Launch()
	if held.Patient() != "pat_7" || held.Scopes() != "patient/Observation.read" {
		t.Errorf("rebuilt %v, want the patient and scope the row states", held)
	}
}

// TestAPersistedRowThatCouldNotHaveBeenWrittenIsRefused, catching a row edited
// underneath the server rather than issued by it.
func TestAPersistedRowThatCouldNotHaveBeenWrittenIsRefused(t *testing.T) {
	_, err := NewSession("prj_a", SessionRecord{
		ID: "ses_1", User: "usr_1", Membership: "mbr_1",
		Digest: "abc", State: SessionActive,
		LaunchPatient: "pat_7 pat_8", GrantedScopes: "patient/Observation.read",
		CreatedAt: launchedAt, ExpiresAt: launchedAt.Add(time.Hour),
	})

	if !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestASessionSaysWhetherAnAppHoldsIt when rendered, because a log line that
// omitted it would read the same for a narrowed session and an open one.
func TestASessionSaysWhetherAnAppHoldsIt(t *testing.T) {
	granted, err := NewLaunchContext("pat_7", "patient/Observation.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	app, _, err := IssueAppSession(
		"prj_a", "ses_1", "usr_1", "mbr_1", granted, "", launchedAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueAppSession: %v", err)
	}

	login, _, err := IssueSession("prj_a", "ses_2", "usr_1", "mbr_1", launchedAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	if !strings.Contains(app.String(), "patient/Observation.read") {
		t.Errorf("an app's session rendered as %q, naming no grant", app.String())
	}

	if strings.Contains(login.String(), "granted") {
		t.Errorf("an ordinary login rendered as %q, claiming a grant", login.String())
	}
}
