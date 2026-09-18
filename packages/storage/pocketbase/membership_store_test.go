package pocketbase

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func newMembershipStore(t *testing.T) (*MembershipStore, *sql.DB) {
	t.Helper()

	_, db := newStore(t)
	seedMembershipFixtures(t, db)

	return NewMembershipStore(db), db
}

// seedMembershipFixtures writes what a membership must name: an identity, a
// policy to bind, and one another Project owns.
func seedMembershipFixtures(t *testing.T, db *sql.DB) {
	t.Helper()

	execAll(t, db, []string{
		"INSERT INTO users (id, scope, home_project_id, email_normalized, email_display," +
			" password_hash, state, created_at, updated_at) VALUES" +
			" ('usr_nurse', 'project', 'prj_a', 'nurse@example.test', 'nurse@example.test'," +
			" 'argon2id$hash', 'active', 0, 0)",

		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES ('prj_a', 'pol_ward', 'Ward', 0, 0), ('prj_b', 'pol_other', 'Other', 0, 0)",

		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param)" +
			" VALUES ('prj_a', 'pol_ward', 0, 'fhir', 'Practitioner', 'read', 1, NULL, NULL, NULL)",
	})
}

func nurseMembership(t *testing.T, state project.MembershipState, admin bool) project.Membership {
	t.Helper()

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_nurse", Project: "prj_a", ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_nurse"},
		State:     state, Admin: admin, Source: project.SourceInvite,
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	return member
}

// nurse is the principal every case below resolves.
var nurse = project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_nurse"}

// TestAnInvitedMemberHoldsNoStandingUntilItIsActivated. Before this store existed
// a membership could only be written by hand, so the lifecycle was a promise.
func TestAnInvitedMemberHoldsNoStandingUntilItIsActivated(t *testing.T) {
	store, db := newMembershipStore(t)
	resolver := NewMembershipResolver(db)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipInvited, true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	invited, found, err := resolver.Membership(t.Context(), "prj_a", nurse)
	if err != nil || !found {
		t.Fatalf("resolve the invitation: found %t, err %v", found, err)
	}

	if invited.HoldsStanding() || invited.IsAdmin() {
		t.Error("an invited membership holds standing")
	}

	if _, err := store.UpdateState(t.Context(), "prj_a", "pm_nurse", project.MembershipActive, 1); err != nil {
		t.Fatalf("activate: %v", err)
	}

	active, found, err := resolver.Membership(t.Context(), "prj_a", nurse)
	if err != nil || !found {
		t.Fatalf("resolve the activation: found %t, err %v", found, err)
	}

	if !active.HoldsStanding() || !active.IsAdmin() {
		t.Errorf("an activated membership holds %v", active)
	}
}

// TestTheAdminFlagSurvivesAnInvitation is why a store writes the stored flags and
// not the effective ones: IsAdmin gates on standing, so persisting through it
// would silently drop what an invited member was granted.
func TestTheAdminFlagSurvivesAnInvitation(t *testing.T) {
	store, db := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipInvited, true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var admin int
	if err := db.QueryRowContext(t.Context(),
		"SELECT admin FROM project_memberships WHERE id = 'pm_nurse'").Scan(&admin); err != nil {
		t.Fatalf("read the admin column: %v", err)
	}

	if admin != 1 {
		t.Error("the admin flag was dropped because the membership was not yet active")
	}
}

// TestASecondLiveMembershipForOnePrincipalIsRefused. Exactly one is what makes
// membership resolution at token issuance deterministic.
func TestASecondLiveMembershipForOnePrincipalIsRefused(t *testing.T) {
	store, _ := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipActive, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	second, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_second", Project: "prj_a", ProjectKind: project.KindStandard,
		Principal: nurse, State: project.MembershipActive, Source: project.SourceInvite,
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	if err := store.Create(t.Context(), second); !errors.Is(err, ErrMembershipRefused) {
		t.Errorf("got %v, want ErrMembershipRefused", err)
	}
}

// TestAMembershipNamingNoIdentityIsRefused, because user_id carries a foreign key
// and a membership for nobody would resolve to standing nobody holds.
func TestAMembershipNamingNoIdentityIsRefused(t *testing.T) {
	store, _ := newMembershipStore(t)

	ghost, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_ghost", Project: "prj_a", ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_ghost"},
		State:     project.MembershipActive, Source: project.SourceInvite,
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	if err := store.Create(t.Context(), ghost); !errors.Is(err, ErrMembershipRefused) {
		t.Errorf("got %v, want ErrMembershipRefused", err)
	}
}

// TestABoundPolicyIsWhatTheDecisionResolvesTo closes the loop: a binding written
// here is a Grant the authorization path compiles.
func TestABoundPolicyIsWhatTheDecisionResolvesTo(t *testing.T) {
	store, db := newMembershipStore(t)
	resolver := NewMembershipResolver(db)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipActive, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	before, _, err := resolver.Membership(t.Context(), "prj_a", nurse)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(before.Policies()) != 0 {
		t.Fatalf("a fresh membership already resolves to %d bindings", len(before.Policies()))
	}

	if err := store.BindPolicy(t.Context(), "prj_a", "pm_nurse", project.PolicyAttachment{
		Policy: "pol_ward", Parameters: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("BindPolicy: %v", err)
	}

	after, _, err := resolver.Membership(t.Context(), "prj_a", nurse)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(after.Policies()) != 1 || after.Policies()[0].Policy().ID() != "pol_ward" {
		t.Fatalf("the membership resolves to %v", after.Policies())
	}

	// Unbinding takes the grant away again on the very next decision.
	if err := store.UnbindPolicy(t.Context(), "prj_a", "pm_nurse", "pol_ward"); err != nil {
		t.Fatalf("UnbindPolicy: %v", err)
	}

	withdrawn, _, err := resolver.Membership(t.Context(), "prj_a", nurse)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(withdrawn.Policies()) != 0 {
		t.Errorf("a withdrawn binding still resolves: %v", withdrawn.Policies())
	}
}

// TestAMembershipCannotBindAPolicyAnotherProjectOwns. A restriction resolves only
// in its owner, so a binding reaching across would be a grant nobody wrote.
func TestAMembershipCannotBindAPolicyAnotherProjectOwns(t *testing.T) {
	store, _ := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipActive, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := store.BindPolicy(t.Context(), "prj_a", "pm_nurse",
		project.PolicyAttachment{Policy: "pol_other"})
	if !errors.Is(err, ErrBindingRefused) {
		t.Errorf("got %v, want ErrBindingRefused", err)
	}
}

// TestBindingAPolicyInvalidatesCachedAuthorization. The binding lives in another
// table, so nothing else would bump the counter a cache keys on.
func TestBindingAPolicyInvalidatesCachedAuthorization(t *testing.T) {
	store, db := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipActive, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	before := authzVersion(t, db)

	if err := store.BindPolicy(t.Context(), "prj_a", "pm_nurse",
		project.PolicyAttachment{Policy: "pol_ward"}); err != nil {
		t.Fatalf("BindPolicy: %v", err)
	}

	if after := authzVersion(t, db); after <= before {
		t.Errorf("authz_version stayed at %d, so a cache would serve the old decision", after)
	}
}

func authzVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var version int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT authz_version FROM project_memberships WHERE id = 'pm_nurse'").Scan(&version); err != nil {
		t.Fatalf("read authz_version: %v", err)
	}

	return version
}

// TestARevokedMembershipIsTerminal. A returning member gets a new membership, not
// a revived one, because reviving would restore every binding it ever held.
func TestARevokedMembershipIsTerminal(t *testing.T) {
	store, _ := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipActive, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	version, err := store.UpdateState(t.Context(), "prj_a", "pm_nurse", project.MembershipRevoked, 1)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = store.UpdateState(t.Context(), "prj_a", "pm_nurse", project.MembershipActive, version)
	if !errors.Is(err, project.ErrInvalidTransition) {
		t.Errorf("got %v, want ErrInvalidTransition", err)
	}
}

// TestUpdateMembershipStateDetectsAVersionConflict, so a decision made against a
// row that has since moved is refused rather than overwriting it.
func TestUpdateMembershipStateDetectsAVersionConflict(t *testing.T) {
	store, _ := newMembershipStore(t)

	if err := store.Create(t.Context(), nurseMembership(t, project.MembershipInvited, false)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.UpdateState(t.Context(), "prj_a", "pm_nurse", project.MembershipActive, 1); err != nil {
		t.Fatalf("activate: %v", err)
	}

	_, err := store.UpdateState(t.Context(), "prj_a", "pm_nurse", project.MembershipRevoked, 1)
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Errorf("got %v, want ErrVersionConflict", err)
	}
}

// TestCreatingAMembershipIsUndoneWhenTheTransactionRollsBack. The row and its
// bindings are one commit boundary, so a member never exists holding standing
// nobody scoped.
func TestCreatingAMembershipIsUndoneWhenTheTransactionRollsBack(t *testing.T) {
	store, db := newMembershipStore(t)

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_nurse", Project: "prj_a", ProjectKind: project.KindStandard,
		Principal: nurse, State: project.MembershipActive, Source: project.SourceInvite,
		Policies: []project.PolicyAttachment{{Policy: "pol_other"}},
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	// The binding names a policy another Project owns, so the whole create fails.
	if err := store.Create(t.Context(), member); !errors.Is(err, ErrBindingRefused) {
		t.Fatalf("got %v, want ErrBindingRefused", err)
	}

	var rows int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM project_memberships WHERE id = 'pm_nurse'").Scan(&rows); err != nil {
		t.Fatalf("count memberships: %v", err)
	}

	if rows != 0 {
		t.Error("a membership survived a create whose binding was refused")
	}
}
