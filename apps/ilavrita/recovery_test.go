package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// The identity a recovery is performed on, and the Project it holds standing in.
const (
	strandedID      = project.UserID("usr_stranded")
	strandedEmail   = "stranded@example.test"
	strandedProject = project.ID("prj_super")
)

// recoveringServer is an administered install that can hold second factors and
// account for what it does with them.
func recoveringServer(t *testing.T) (http.Handler, string, *sql.DB) {
	t.Helper()

	routes, token, db := administeredServer(t)

	encoded, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("mint a sealing key: %v", err)
	}

	key, err := project.ParseSealingKey(encoded)
	if err != nil {
		t.Fatalf("parse a sealing key: %v", err)
	}

	serving.factors = sqlite.NewFactorStore(db, key)
	serving.audits = sqlite.NewAuditStore(db)

	return routes, token, db
}

// strandedIdentity writes an identity with standing and a second factor in
// force, which is who a recovery is for.
func strandedIdentity(t *testing.T, db *sql.DB) project.PrincipalRef {
	t.Helper()

	ctx := context.Background()

	memberOfTheProject(t, db, strandedID, "pm_stranded", strandedEmail,
		"a rather long password of their own")

	putFactorInForce(t, strandedID)

	_ = ctx

	return project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(strandedID)}
}

// memberOfTheProject writes an identity with ordinary standing, which is
// standing enough to log in and not enough to administer anything.
func memberOfTheProject(
	t *testing.T, db *sql.DB, id project.UserID, standing project.MembershipID,
	address, password string,
) {
	t.Helper()

	principal := credentialledIdentity(t, db, strandedProject, id, address, password)

	member, err := project.NewMembership(project.MembershipConfig{
		ID: standing, Project: strandedProject, ProjectKind: project.KindSuper,
		Principal: principal, State: project.MembershipActive, Source: project.SourceAPI,
	})
	if err != nil {
		t.Fatalf("author the standing: %v", err)
	}

	if err := sqlite.NewMembershipStore(db).Create(context.Background(), member); err != nil {
		t.Fatalf("grant the standing: %v", err)
	}
}

// putFactorInForce enrols a factor and proves it, which is the state somebody
// who then loses the phone is in.
func putFactorInForce(t *testing.T, user project.UserID) {
	t.Helper()

	ctx := context.Background()
	at := serving.clock()

	secret, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		t.Fatalf("mint a secret: %v", err)
	}

	factor, err := project.EnrolSecondFactor(user, secret, at)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}

	if err := serving.factors.Enrol(ctx, factor); err != nil {
		t.Fatalf("store the enrolment: %v", err)
	}

	confirmed, err := factor.Confirm(secret.Code(project.Step(at)), at)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if err := serving.factors.Confirm(ctx, confirmed); err != nil {
		t.Fatalf("store the confirmation: %v", err)
	}
}

// recovering asks for one identity's factor to be taken off.
func recovering(
	t *testing.T, routes http.Handler, token string, owner project.ID, user project.UserID,
) *httptest.ResponseRecorder {
	t.Helper()

	return call{
		method: http.MethodDelete,
		path: controlBasePath + "/projects/" + string(owner) + "/users/" + string(user) +
			"/second-factor",
		bearer: token,
	}.send(t, routes)
}

// factorsHeld counts the second factors stored.
func factorsHeld(t *testing.T, db *sql.DB) int {
	t.Helper()

	var held int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM user_second_factors").Scan(&held); err != nil {
		t.Fatalf("count the factors: %v", err)
	}

	return held
}

// TestAnAdministratorTakesALostFactorOff. Replacing a factor needs a code from
// the factor it replaces, which is what stops a stolen session switching MFA
// off — and leaves somebody who lost the phone with nothing to present. This is
// what answers that: another person with standing to act.
func TestAnAdministratorTakesALostFactorOff(t *testing.T) {
	routes, token, db := recoveringServer(t)

	strandedIdentity(t, db)

	if factorsHeld(t, db) != 1 {
		t.Fatal("the identity holds no factor, so this proves nothing")
	}

	assertStatus(t, recovering(t, routes, token, strandedProject, strandedID),
		http.StatusNoContent)

	if held := factorsHeld(t, db); held != 0 {
		t.Errorf("%d factors are still held", held)
	}
}

// TestARecoveryIsRecordedAgainstWhoDidIt. A reset is the one way a factor comes
// off without the phone that answers for it, so the trail has to say who did it
// and to whom.
func TestARecoveryIsRecordedAgainstWhoDidIt(t *testing.T) {
	routes, token, db := recoveringServer(t)

	strandedIdentity(t, db)

	assertStatus(t, recovering(t, routes, token, strandedProject, strandedID),
		http.StatusNoContent)

	var found bool

	for _, event := range recorded(t, db) {
		if event.resourceType.String != string(recoveredResource) {
			continue
		}

		found = true

		if event.resourceID.String != string(strandedID) {
			t.Errorf("the record names %q, and the factor taken off was %q",
				event.resourceID.String, strandedID)
		}

		if event.principalID != "usr_founder" {
			t.Errorf("the record names %q as having done it", event.principalID)
		}
	}

	if !found {
		t.Error("a recovery left no record of itself")
	}
}

// TestARecoverySignsTheIdentityOut. The reset is also what an operator reaches
// for when the phone was stolen rather than lost, and a session already open on
// that phone would otherwise outlive the factor.
func TestARecoverySignsTheIdentityOut(t *testing.T) {
	routes, token, db := recoveringServer(t)

	strandedIdentity(t, db)

	issued, err := project.NewSession(strandedProject, project.SessionRecord{
		ID: "ses_stranded", User: strandedID, Membership: "pm_stranded",
		Digest: "a digest", State: project.SessionActive,
		CreatedAt: serving.clock(), ExpiresAt: serving.clock().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("author the session: %v", err)
	}

	if err := sqlite.NewSessionStore(db).Issue(context.Background(), issued); err != nil {
		t.Fatalf("issue the session: %v", err)
	}

	assertStatus(t, recovering(t, routes, token, strandedProject, strandedID),
		http.StatusNoContent)

	live, err := sqlite.NewSessionStore(db).Live(
		context.Background(), strandedProject, "ses_stranded", serving.clock())
	if err != nil {
		t.Fatalf("read the session: %v", err)
	}

	if live {
		t.Error("the identity is still signed in after its factor was taken off")
	}
}

// TestAnAdministratorDoesNotRecoverTheirOwn. That is the hole the whole design
// closes: withdrawing your own factor needs a code from it, and an
// administrator who could reach around that with their own session would make a
// stolen administrator session enough to disable the factor it was meant to
// survive.
func TestAnAdministratorDoesNotRecoverTheirOwn(t *testing.T) {
	routes, token, db := recoveringServer(t)

	putFactorInForce(t, "usr_founder")

	assertStatus(t, recovering(t, routes, token, "prj_super", "usr_founder"),
		http.StatusForbidden)

	if held := factorsHeld(t, db); held != 1 {
		t.Errorf("%d factors are held, want the administrator's own still in force", held)
	}
}

// TestARecoveryNeedsStandingToAdminister, so a member is not enough.
func TestARecoveryNeedsStandingToAdminister(t *testing.T) {
	routes, _, db := recoveringServer(t)

	strandedIdentity(t, db)

	// An ordinary member's token: standing in the Project, and none to
	// administer it.
	const onlookerPassword = "a rather long onlooker password"

	memberOfTheProject(t, db, "usr_onlooker", "pm_onlooker", "onlooker@example.test", onlookerPassword)

	member := tokenFrom(t, logInAs(t, routes, superSlug, "onlooker@example.test", onlookerPassword))

	assertStatus(t, recovering(t, routes, member, strandedProject, strandedID),
		http.StatusForbidden)

	if held := factorsHeld(t, db); held != 1 {
		t.Errorf("%d factors are held, want the one a member could not take off", held)
	}
}

// TestARecoveryReachesNoIdentityOutsideTheProject. An administrator of one
// Project administers that one, and an identity holding no standing there is
// the same answer as one that does not exist: telling them apart tells an
// administrator who belongs to another Project.
func TestARecoveryReachesNoIdentityOutsideTheProject(t *testing.T) {
	routes, token, db := recoveringServer(t)

	strandedIdentity(t, db)

	// An identity with a factor and no standing in the Project the request
	// names. It is not enough that the answer is 404 — an identity nobody ever
	// wrote answers that too, from further down. What is asserted is that its
	// factor is still there afterwards.
	credentialledIdentity(t, db, strandedProject, "usr_elsewhere",
		"elsewhere@example.test", "a rather long password from elsewhere")
	putFactorInForce(t, "usr_elsewhere")

	for described, user := range map[string]project.UserID{
		"holding no standing": "usr_elsewhere",
		"nobody ever wrote":   "usr_nobody",
	} {
		if answer := recovering(t, routes, token, strandedProject, user); answer.Code != http.StatusNotFound {
			t.Errorf("an identity %s answered %d", described, answer.Code)
		}
	}

	if held := factorsHeld(t, db); held != 2 {
		t.Errorf("%d factors are held, want both the ones nobody could reach", held)
	}
}

// TestRecoveringWhatIsNotThereIsNotFound, so a recovery says what happened
// rather than reporting success for an identity that had no factor.
func TestRecoveringWhatIsNotThereIsNotFound(t *testing.T) {
	routes, token, db := recoveringServer(t)

	memberOfTheProject(t, db, strandedID, "pm_stranded", strandedEmail, "a long enough password")

	assertStatus(t, recovering(t, routes, token, strandedProject, strandedID),
		http.StatusNotFound)
}
