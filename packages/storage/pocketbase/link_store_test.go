package pocketbase

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// linkedResolvers wires every port a decision reads, with the real link resolver
// in place of the stub that answered with nothing.
func linkedResolvers(db *sql.DB) authz.Resolvers {
	return authz.Resolvers{
		Memberships: NewMembershipResolver(db),
		Projects:    NewProjectResolver(db),
		Policies:    NewPolicyResolver(db),
		Links:       NewLinkStore(db),
	}
}

// linkedDatabase seeds the whole control plane one cross-Project decision reads,
// including the capability rows the shared fixture leaves out: an administrative
// link naming no capability confers nothing, which is correct but proves nothing.
func linkedDatabase(t *testing.T) *sql.DB {
	t.Helper()

	_, db := newStore(t)
	seedControlPlane(t, db)

	execAll(t, db, []string{
		"INSERT INTO users (id, scope, home_project_id, email_normalized, email_display," +
			" password_hash, state, created_at, updated_at)" +
			" VALUES ('usr_steward', 'project', 'prj_b', 'steward@example.test'," +
			" 'steward@example.test', 'argon2id$hash', 'active', 0, 0)",

		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_b', 'pm_steward', 'standard', 'usr_steward', 'active', 'api', 0, 0, 0)",

		// The holder is a member of the grantee Project, named individually.
		"INSERT INTO project_link_capabilities (grantee_project, grantor_project, kind," +
			" capability, principal_membership_project, principal_membership_id)" +
			" VALUES ('prj_b', 'prj_a', 'administrative', 'project.membership.read', 'prj_b', 'pm_steward')",
	})

	return db
}

// reachRequest is one cross-Project read: the caller stands in prj_a and names
// the grantors it opted into.
func reachRequest(db *sql.DB, grantors ...project.ID) authz.Request {
	return authz.Request{
		Principal:      clinician,
		Project:        homeProject,
		LinkedProjects: grantors,
		Kind:           storage.KindFHIR,
		Type:           "Observation",
		Action:         storage.ActionRead,
		Now:            decidedAt,
		Resolvers:      linkedResolvers(db),
	}
}

// reaches reports whether a Scope carries any Grant from the grantor Project.
func reaches(scope storage.Scope) bool {
	for _, grant := range scope.Grants() {
		if grant.Project == grantorProject {
			return true
		}
	}

	return false
}

// TestAnActiveLinkGrantsWhatItShares. Before this resolver existed the port
// answered with nothing, so a link was a row no decision could ever read.
func TestAnActiveLinkGrantsWhatItShares(t *testing.T) {
	db := linkedDatabase(t)

	scope, err := authz.BuildScope(t.Context(), reachRequest(db, grantorProject))
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	if !reaches(scope) {
		t.Fatalf("an active link granted nothing: %v", scope.Grants())
	}
}

// TestALinkNobodyNamedGrantsNothing. The grantor is opted into per request, so an
// effective link nobody named contributes nothing.
func TestALinkNobodyNamedGrantsNothing(t *testing.T) {
	db := linkedDatabase(t)

	scope, err := authz.BuildScope(t.Context(), reachRequest(db))
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	if reaches(scope) {
		t.Errorf("a link nobody named granted %v", scope.Grants())
	}
}

// TestALinkThatIsNotActiveGrantsNothing. The resolver reports the link in whatever
// state it holds and Effective decides, so a revoke takes hold on the next
// decision rather than when something remembers to prune a cache.
func TestALinkThatIsNotActiveGrantsNothing(t *testing.T) {
	for _, status := range []string{"suspended", "revoked", "proposed"} {
		db := linkedDatabase(t)

		execAll(t, db, []string{
			"UPDATE project_links SET status = '" + status + "'" +
				" WHERE grantee_project = 'prj_a' AND grantor_project = 'prj_b'",
		})

		scope, err := authz.BuildScope(t.Context(), reachRequest(db, grantorProject))
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}

		if reaches(scope) {
			t.Errorf("a %s link granted %v", status, scope.Grants())
		}
	}
}

// TestALinkGrantsNothingOutsideTheTypesItNames. A share is enumerated, so a type
// the link does not name is not reachable through it.
func TestALinkGrantsNothingOutsideTheTypesItNames(t *testing.T) {
	db := linkedDatabase(t)

	request := reachRequest(db, grantorProject)
	request.Type = "Patient"

	scope, err := authz.BuildScope(t.Context(), request)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	if reaches(scope) {
		t.Errorf("a link shared a type it does not name: %v", scope.Grants())
	}
}

// TestAnExpiredLinkGrantsNothing, without anything having to revoke it.
func TestAnExpiredLinkGrantsNothing(t *testing.T) {
	db := linkedDatabase(t)

	expired := strconv.FormatInt(decidedAt.Add(-time.Hour).UnixMilli(), 10)

	execAll(t, db, []string{
		"UPDATE project_links SET expires_at = " + expired +
			" WHERE grantee_project = 'prj_a' AND grantor_project = 'prj_b'",
	})

	scope, err := authz.BuildScope(t.Context(), reachRequest(db, grantorProject))
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	if reaches(scope) {
		t.Errorf("an expired link granted %v", scope.Grants())
	}
}

// TestInboundReadsOnlyLinksIntoTheProjectAsked. A link reaches one way, so the
// resolver must not answer with the Project's own outbound links.
func TestInboundReadsOnlyLinksIntoTheProjectAsked(t *testing.T) {
	db := linkedDatabase(t)
	store := NewLinkStore(db)

	for _, grantee := range []project.ID{homeProject, grantorProject} {
		links, err := store.Inbound(t.Context(), grantee)
		if err != nil {
			t.Fatalf("Inbound(%s): %v", grantee, err)
		}

		if len(links) == 0 {
			t.Errorf("no link reaches into %s, so this proves nothing", grantee)
		}

		for _, link := range links {
			if link.Grantee() != grantee {
				t.Errorf("a link into %s answered for %s", link.Grantee(), grantee)
			}
		}
	}
}

// TestAnAdministrativeLinkCarriesNoDataShare. FR-052 holds structurally: a link
// that confers administrative capability shares no resource type at all.
func TestAnAdministrativeLinkCarriesNoDataShare(t *testing.T) {
	db := linkedDatabase(t)

	links, err := NewLinkStore(db).Inbound(t.Context(), grantorProject)
	if err != nil {
		t.Fatalf("Inbound: %v", err)
	}

	var administrative int

	for _, link := range links {
		if link.Kind() != project.LinkKindAdministrative {
			continue
		}

		administrative++

		if _, shares := link.Share(decidedAt); shares {
			t.Errorf("an administrative link carries a data share: %v", link)
		}
	}

	if administrative == 0 {
		t.Error("no administrative link was read back, so this proves nothing")
	}
}
