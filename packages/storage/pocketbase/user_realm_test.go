package pocketbase

import (
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// A link-minted row is the second shape a foreign membership can take, and it
// reaches a different rebuild path than the direct row the suite already covers.
func TestAForeignIdentityGainsNothingFromALinkMintedRow(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"INSERT INTO project_links (grantee_project, grantor_project, kind, status," +
			" activated_at, grantor_approved_by, grantor_approved_at," +
			" grantee_approved_by, grantee_approved_at, created_at, updated_at)" +
			" VALUES ('prj_a', 'prj_b', 'administrative', 'active', 10, 'pm_x', 10, 'pm_y', 10, 1, 1)",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, via_link_grantee_project, via_link_kind, created_at, updated_at, activated_at)" +
			" VALUES ('prj_b', 'pm_linked_intruder', 'standard', 'usr_clinician', 'active', 'link'," +
			" 'prj_a', 'administrative', 0, 0, 0)",
	})

	membership, found, err := NewMembershipResolver(f.db).Membership(t.Context(), grantorProject, clinician)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatalf("a prj_a identity resolved into prj_b through %s", membership.ID())
	}
}

// The Super Project is the worst destination for a foreign identity: a row there
// may carry super_admin, which no standard Project can.
func TestAForeignIdentityNeverResolvesIntoTheSuperProject(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, allow_clinical_data," +
			" created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_super', 'super', 'super', 'Super', 'active', 0, 0, 0, 0)",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, admin, super_admin, created_at, updated_at, activated_at)" +
			" VALUES ('prj_super', 'pm_escalate', 'super', 'usr_clinician', 'active', 'api', 1, 1, 0, 0, 0)",
	})

	membership, found, err := NewMembershipResolver(f.db).Membership(t.Context(), "prj_super", clinician)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatalf("a prj_a identity resolved into the Super Project: admin %v, super admin %v",
			membership.IsAdmin(), membership.IsSuperAdmin())
	}
}

// The revoked-last ordering picks one row per Project. A revoked row at home
// must not make a live foreign row the better answer anywhere.
func TestARevokedRowAtHomeDoesNotSurfaceAForeignOne(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE project_memberships SET state = 'revoked', revoked_at = 1 WHERE id = 'pm_clinician'",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, admin, created_at, updated_at, activated_at)" +
			" VALUES ('prj_b', 'aaa_live', 'standard', 'usr_clinician', 'active', 'api', 1, 0, 0, 0)",
	})

	resolver := NewMembershipResolver(f.db)

	membership, found, err := resolver.Membership(t.Context(), grantorProject, clinician)
	if err != nil {
		t.Fatalf("resolve membership in prj_b: %v", err)
	}

	if found {
		t.Fatalf("a live foreign row answered in prj_b: %s", membership.ID())
	}

	membership, found, err = resolver.Membership(t.Context(), homeProject, clinician)
	if err != nil || !found || membership.ID() != "pm_clinician" || membership.HoldsStanding() {
		t.Fatalf("home lookup: %s, found %v, standing %v, err %v",
			membership.ID(), found, membership.HoldsStanding(), err)
	}
}

// An identity that has never accepted its invitation is as absent as a disabled
// one, which the state predicate must cover and not only the disabled case.
func TestAnInvitedIdentityHoldsNoMembership(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE users SET state = 'invited', password_hash = NULL WHERE id = 'usr_clinician'",
	})

	_, found, err := NewMembershipResolver(f.db).Membership(t.Context(), homeProject, clinician)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatal("an identity that never accepted its invitation holds standing")
	}
}

// The realm reaches SQLite as a bound value, so a pattern or a quote in it names
// one realm that does not exist rather than matching several that do.
func TestARealmIsComparedWholeAndNeverAsAPattern(t *testing.T) {
	store, _ := newUserStore(t)

	const raw = "target@clinic.example"

	mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_a", raw))

	realms := []project.IdentityRealm{"prj_%", "prj__", "%", "_", "prj_a' OR '1'='1", "PRJ_A", " prj_a"}
	for _, realm := range realms {
		user, _, found, err := store.ByRealmEmail(t.Context(), realm, address(t, raw))
		if err != nil {
			t.Fatalf("lookup in %q: %v", realm, err)
		}

		if found {
			t.Errorf("realm %q resolved %s, an identity homed in prj_a", realm, user.ID())
		}
	}
}
