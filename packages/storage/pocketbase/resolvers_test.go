package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// The Projects newStore seeds: prj_a holds the membership under test, prj_b is
// the grantor reached through a data link.
const (
	homeProject    = project.ID("prj_a")
	grantorProject = project.ID("prj_b")
)

var (
	clinician = project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_clinician"}
	operator  = project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_operator"}
	disabled  = project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_disabled"}

	decidedAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

// seedControlPlane writes the whole control plane one decision reads: two
// identities in prj_a and one server-scoped one, their memberships, a bound
// policy, and the two links prj_a stands at either end of.
func seedControlPlane(t *testing.T, db *sql.DB) {
	t.Helper()

	execAll(t, db, []string{
		"INSERT INTO users (id, scope, home_project_id, email_normalized, email_display," +
			" password_hash, state, created_at, updated_at) VALUES" +
			" ('usr_clinician', 'project', 'prj_a', 'clinician@example.test', 'Clinician@example.test'," +
			" 'argon2id$hash', 'active', 0, 0)," +
			" ('usr_disabled', 'project', 'prj_a', 'disabled@example.test', 'disabled@example.test'," +
			" 'argon2id$hash', 'disabled', 0, 0)," +
			" ('usr_operator', 'server', NULL, 'operator@example.test', 'operator@example.test'," +
			" 'argon2id$hash', 'active', 0, 0)",

		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at) VALUES" +
			" ('prj_a', 'pol_chart', 'Own chart', 0, 0), ('prj_b', 'pol_share', 'Shared cohort', 0, 0)",

		"INSERT INTO access_policy_parameters (project_id, policy_id, name)" +
			" VALUES ('prj_a', 'pol_chart', 'patient')",

		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param) VALUES" +
			" ('prj_a', 'pol_chart', 0, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, 'patient')," +
			" ('prj_a', 'pol_chart', 1, 'fhir', 'Practitioner', 'search', 1, NULL, NULL, NULL)," +
			" ('prj_b', 'pol_share', 0, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat_shared', NULL)",

		"INSERT INTO project_links (grantee_project, grantor_project, kind, status," +
			" grantor_access_policy_id, activated_at, grantor_approved_by, grantor_approved_at," +
			" grantee_approved_by, grantee_approved_at, created_at, updated_at) VALUES" +
			" ('prj_a', 'prj_b', 'data', 'active', 'pol_share', 10, 'pm_grantor', 10, 'pm_clinician', 10, 1, 1)," +
			" ('prj_b', 'prj_a', 'administrative', 'active', NULL, 10, 'pm_grantor', 10, 'pm_clinician', 10, 1, 1)",

		"INSERT INTO project_link_types (grantee_project, grantor_project, kind, res_type, action)" +
			" VALUES ('prj_a', 'prj_b', 'data', 'Observation', 'read')",

		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at) VALUES" +
			" ('prj_a', 'pm_clinician', 'standard', 'usr_clinician', 'active', 'api', 0, 0, 0)," +
			" ('prj_a', 'pm_disabled', 'standard', 'usr_disabled', 'active', 'api', 0, 0, 0)",

		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, via_link_grantee_project, via_link_kind, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_operator', 'standard', 'usr_operator', 'active', 'link'," +
			" 'prj_b', 'administrative', 0, 0, 0)",

		"INSERT INTO project_membership_policies (project_id, membership_id, policy_id, ordinal, policy_params)" +
			` VALUES ('prj_a', 'pm_clinician', 'pol_chart', 0, '{"patient":"pat_1"}')`,
	})
}

func execAll(t *testing.T, db *sql.DB, statements []string) {
	t.Helper()

	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("run %q: %v", summarize(statement), err)
		}
	}
}

// dbLinks resolves inbound links from project_links at query time. It lives in
// the test because project_link_types pairs each type with an action that
// project.DataShare cannot carry, an open decision the resolvers do not settle.
type dbLinks struct {
	db     *sql.DB
	action storage.Action
	asked  []project.ID
}

// linkRow is one active data link, read before any child query runs so the
// single pooled connection is free again.
type linkRow struct {
	grantor     string
	policy      string
	activatedAt int64
	expiresAt   sql.NullInt64
	grantorBy   string
	grantorAt   int64
	granteeBy   string
	granteeAt   int64
}

func (l *dbLinks) Inbound(ctx context.Context, grantee project.ID) ([]project.Link, error) {
	l.asked = append(l.asked, grantee)

	const query = "SELECT grantor_project, grantor_access_policy_id, activated_at, expires_at," +
		" grantor_approved_by, grantor_approved_at, grantee_approved_by, grantee_approved_at" +
		" FROM project_links WHERE grantee_project = ? AND kind = 'data' AND status = 'active'"

	rows, err := l.db.QueryContext(ctx, query, string(grantee))
	if err != nil {
		return nil, err
	}

	var found []linkRow

	for rows.Next() {
		var row linkRow
		if err := rows.Scan(&row.grantor, &row.policy, &row.activatedAt, &row.expiresAt,
			&row.grantorBy, &row.grantorAt, &row.granteeBy, &row.granteeAt); err != nil {
			_ = rows.Close()

			return nil, err
		}

		found = append(found, row)
	}

	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return l.build(ctx, grantee, found)
}

func (l *dbLinks) build(ctx context.Context, grantee project.ID, rows []linkRow) ([]project.Link, error) {
	links := make([]project.Link, 0, len(rows))

	for _, row := range rows {
		grantor := project.ID(row.grantor)

		types, err := l.types(ctx, grantee, grantor)
		if err != nil {
			return nil, err
		}

		if len(types) == 0 {
			continue
		}

		id := linkIdentifier(grantee, grantor, project.LinkKindData)

		link, err := project.NewDataLink(id, grantor, grantee, storage.LogicalID(row.policy), types...)
		if err != nil {
			return nil, err
		}

		link, err = l.activate(link, row)
		if err != nil {
			return nil, err
		}

		links = append(links, link)
	}

	return links, nil
}

func (l *dbLinks) activate(link project.Link, row linkRow) (project.Link, error) {
	link, err := link.ApproveGrantor(project.MembershipID(row.grantorBy), time.UnixMilli(row.grantorAt).UTC())
	if err != nil {
		return project.Link{}, err
	}

	link, err = link.ApproveGrantee(project.MembershipID(row.granteeBy), time.UnixMilli(row.granteeAt).UTC())
	if err != nil {
		return project.Link{}, err
	}

	link, err = link.TransitionTo(project.LinkActive, time.UnixMilli(row.activatedAt).UTC())
	if err != nil {
		return project.Link{}, err
	}

	if row.expiresAt.Valid {
		link = link.WithExpiry(time.UnixMilli(row.expiresAt.Int64).UTC())
	}

	return link, nil
}

func (l *dbLinks) types(ctx context.Context, grantee, grantor project.ID) ([]storage.ResourceType, error) {
	const query = "SELECT res_type FROM project_link_types" +
		" WHERE grantee_project = ? AND grantor_project = ? AND kind = 'data' AND action = ?"

	rows, err := l.db.QueryContext(ctx, query, string(grantee), string(grantor), string(l.action))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var types []storage.ResourceType

	for rows.Next() {
		var resourceType string
		if err := rows.Scan(&resourceType); err != nil {
			return nil, err
		}

		types = append(types, storage.ResourceType(resourceType))
	}

	return types, rows.Err()
}

// fixture is one database with the three resolvers wired against it.
type fixture struct {
	db    *sql.DB
	links *dbLinks
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	_, db := newStore(t)
	seedControlPlane(t, db)

	return &fixture{db: db}
}

// request wires one decision. The link port is rebuilt per request because it
// resolves the shared types for that request's own action.
func (f *fixture) request(
	principal project.PrincipalRef,
	proj project.ID,
	resourceType storage.ResourceType,
	action storage.Action,
	linked ...project.ID,
) authz.Request {
	f.links = &dbLinks{db: f.db, action: action}

	return authz.Request{
		Principal:      principal,
		Project:        proj,
		LinkedProjects: linked,
		Kind:           storage.KindFHIR,
		Type:           resourceType,
		Action:         action,
		Now:            decidedAt,
		Launch:         authz.NoLaunch(),
		Resolvers: authz.Resolvers{
			Memberships: NewMembershipResolver(f.db),
			Projects:    NewProjectResolver(f.db),
			Policies:    NewPolicyResolver(f.db),
			Links:       f.links,
		},
	}
}

func (f *fixture) scope(t *testing.T, req authz.Request) storage.Scope {
	t.Helper()

	scope, err := authz.BuildScope(t.Context(), req)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}

	return scope
}

// describe renders a Grant as the facts an audit reads, so a test names the
// Project, triple, provenance and compartment it expects rather than a struct.
func describe(grant storage.Grant) string {
	compartment := "unrestricted"
	if grant.Compartment != nil {
		compartment = string(grant.Compartment.Type) + "/" + string(grant.Compartment.ID)
	}

	return fmt.Sprintf("%s %s %s %s via %s on %s",
		grant.Project, grant.Kind, grant.Type, grant.Action, grant.Source, compartment)
}

func describeAll(scope storage.Scope) []string {
	described := make([]string, 0, len(scope.Grants()))
	for _, grant := range scope.Grants() {
		described = append(described, describe(grant))
	}

	slices.Sort(described)

	return described
}

func assertGrants(t *testing.T, scope storage.Scope, want ...string) {
	t.Helper()

	slices.Sort(want)

	if got := describeAll(scope); !slices.Equal(got, want) {
		t.Fatalf("grants = %v, want %v", got, want)
	}
}

func TestBuildScopeCompilesAMembersBoundPolicy(t *testing.T) {
	f := newFixture(t)

	scope := f.scope(t, f.request(clinician, homeProject, "Observation", storage.ActionRead))

	assertGrants(t, scope, "prj_a fhir Observation read via membership on Patient/pat_1")
}

func TestBuildScopeAnswersOnlyTheRequestedTriple(t *testing.T) {
	f := newFixture(t)

	unrestricted := f.scope(t, f.request(clinician, homeProject, "Practitioner", storage.ActionSearch))
	assertGrants(t, unrestricted, "prj_a fhir Practitioner search via membership on unrestricted")

	for _, tc := range []struct {
		name         string
		resourceType storage.ResourceType
		action       storage.Action
	}{
		{name: "an action no rule names", resourceType: "Observation", action: storage.ActionWrite},
		{name: "a type no rule names", resourceType: "Condition", action: storage.ActionRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := f.scope(t, f.request(clinician, homeProject, tc.resourceType, tc.action))
			if !scope.IsEmpty() {
				t.Fatalf("scope = %v, want empty", describeAll(scope))
			}
		})
	}
}

func TestAnActiveMemberReachesTheGrantorThroughTheLink(t *testing.T) {
	f := newFixture(t)

	scope := f.scope(t, f.request(clinician, homeProject, "Observation", storage.ActionRead, grantorProject))

	assertGrants(t, scope,
		"prj_a fhir Observation read via membership on Patient/pat_1",
		"prj_b fhir Observation read via link on Patient/pat_shared")
}

// TestARevokedMemberReachesNoGrantor is the control for the test above: the same
// live link, the same named grantor, and standing that was revoked.
func TestARevokedMemberReachesNoGrantor(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE project_memberships SET state = 'revoked', revoked_at = 1" +
			" WHERE project_id = 'prj_a' AND id = 'pm_clinician'",
	})

	req := f.request(clinician, homeProject, "Observation", storage.ActionRead, grantorProject)

	scope := f.scope(t, req)
	if !scope.IsEmpty() {
		t.Fatalf("scope = %v, want empty", describeAll(scope))
	}

	if len(f.links.asked) != 0 {
		t.Fatalf("link resolver asked for %v, want no lookup at all", f.links.asked)
	}
}

// TestARevokedMembershipRebuildsRevoked pins the reason the test above holds: the
// row rebuilds carrying its own revoked state, rather than being filtered into
// looking like a membership nobody ever held.
func TestARevokedMembershipRebuildsRevoked(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE project_memberships SET state = 'revoked', revoked_at = 1" +
			" WHERE project_id = 'prj_a' AND id = 'pm_clinician'",
	})

	membership, found, err := NewMembershipResolver(f.db).Membership(t.Context(), homeProject, clinician)
	if err != nil || !found {
		t.Fatalf("membership found = %v, err = %v, want a revoked membership", found, err)
	}

	if membership.State() != project.MembershipRevoked {
		t.Errorf("state = %q, want %q", membership.State(), project.MembershipRevoked)
	}

	if membership.HoldsStanding() {
		t.Error("a revoked membership reported standing")
	}

	if len(membership.Policies()) != 0 {
		t.Error("a revoked membership resolved to policy bindings")
	}
}

func TestALinkMintedMembershipGrantsNothing(t *testing.T) {
	f := newFixture(t)

	membership, found, err := NewMembershipResolver(f.db).Membership(t.Context(), homeProject, operator)
	if err != nil || !found {
		t.Fatalf("membership found = %v, err = %v, want a link-minted membership", found, err)
	}

	via, minted := membership.ViaLink()
	if !minted {
		t.Fatalf("membership reports no minting link, want one")
	}

	if via != linkIdentifier(grantorProject, homeProject, project.LinkKindAdministrative) {
		t.Errorf("minting link = %q, want the administrative link prj_b holds into prj_a", via)
	}

	if membership.HoldsStanding() || membership.IsAdmin() || membership.IsSuperAdmin() {
		t.Error("a link-minted membership reported standing")
	}

	scope := f.scope(t, f.request(operator, homeProject, "Observation", storage.ActionRead, grantorProject))
	if !scope.IsEmpty() {
		t.Fatalf("scope = %v, want empty", describeAll(scope))
	}

	if len(f.links.asked) != 0 {
		t.Fatalf("link resolver asked for %v, want no lookup at all", f.links.asked)
	}
}

func TestADisabledIdentityHoldsNoMembership(t *testing.T) {
	f := newFixture(t)

	_, found, err := NewMembershipResolver(f.db).Membership(t.Context(), homeProject, disabled)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatal("an active membership behind a disabled identity resolved")
	}

	scope := f.scope(t, f.request(disabled, homeProject, "Observation", storage.ActionRead))
	if !scope.IsEmpty() {
		t.Fatalf("scope = %v, want empty", describeAll(scope))
	}
}

// TestAMembershipOutsideItsIdentityRealmDoesNotResolve writes the row the schema
// cannot refuse today: a prj_a identity given standing in prj_b. The read path
// must not turn it into a Scope while no constraint stops the write.
func TestAMembershipOutsideItsIdentityRealmDoesNotResolve(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES ('prj_b', 'pol_all', 'Everything', 0, 0)",
		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param)" +
			" VALUES ('prj_b', 'pol_all', 0, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat_other', NULL)",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_b', 'pm_intruder', 'standard', 'usr_clinician', 'active', 'api', 0, 0, 0)",
		"INSERT INTO project_membership_policies (project_id, membership_id, policy_id, ordinal)" +
			" VALUES ('prj_b', 'pm_intruder', 'pol_all', 0)",
	})

	_, found, err := NewMembershipResolver(f.db).Membership(t.Context(), grantorProject, clinician)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatal("an identity homed in prj_a resolved into prj_b")
	}

	scope := f.scope(t, f.request(clinician, grantorProject, "Observation", storage.ActionRead))
	if !scope.IsEmpty() {
		t.Fatalf("scope = %v, want empty", describeAll(scope))
	}
}

func TestAMembershipIsNotFoundWhereNoRowExists(t *testing.T) {
	f := newFixture(t)

	membership, found, err := NewMembershipResolver(f.db).Membership(t.Context(), grantorProject, operator)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}

	if found {
		t.Fatal("a principal with no row in prj_b resolved to a membership")
	}

	if membership.HoldsStanding() {
		t.Error("the zero membership reported standing")
	}
}

func TestASuspendedProjectAdmitsNothing(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{"UPDATE projects SET state = 'suspended' WHERE id = 'prj_a'"})

	scope := f.scope(t, f.request(clinician, homeProject, "Observation", storage.ActionRead, grantorProject))
	if !scope.IsEmpty() {
		t.Fatalf("scope = %v, want empty", describeAll(scope))
	}
}

func TestProjectStateReadsTheRowAndRefusesAnAbsentProject(t *testing.T) {
	f := newFixture(t)

	resolver := NewProjectResolver(f.db)

	state, err := resolver.State(t.Context(), homeProject)
	if err != nil || state != project.StateActive {
		t.Fatalf("state = %q, err = %v, want %q", state, err, project.StateActive)
	}

	state, err = resolver.State(t.Context(), "prj_ghost")
	if err == nil {
		t.Fatalf("state of an absent project = %q with no error, want an error", state)
	}

	if !errors.Is(err, storage.ErrNotFound) || !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("error = %v, want one reporting a missing row", err)
	}

	if state != "" {
		t.Errorf("state = %q, want the empty state alongside the error", state)
	}
}

func TestPolicyResolverAnswersTheReferenceItIsGiven(t *testing.T) {
	f := newFixture(t)

	ref := boundPolicy(t, f.db)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v, want the bound policy", found, err)
	}

	if !policy.Matches(ref) {
		t.Fatalf("policy %s/%s does not match the reference %s/%s",
			policy.Project(), policy.ID(), ref.Project(), ref.ID())
	}

	if names := policy.Parameters(); !slices.Equal(names, []authz.ParameterName{"patient"}) {
		t.Errorf("parameters = %v, want [patient]", names)
	}

	if len(policy.Rules()) != 2 {
		t.Errorf("rules = %d, want the two rows seeded for pol_chart", len(policy.Rules()))
	}
}

func TestPolicyResolverReportsAVanishedPolicyAsNotFound(t *testing.T) {
	f := newFixture(t)

	ref := boundPolicy(t, f.db)

	execAll(t, f.db, []string{
		"DELETE FROM project_membership_policies WHERE project_id = 'prj_a' AND policy_id = 'pol_chart'",
		"DELETE FROM access_policies WHERE project_id = 'prj_a' AND id = 'pol_chart'",
	})

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), ref)
	if err != nil {
		t.Fatalf("resolve a policy that no longer exists: %v", err)
	}

	if found {
		t.Fatalf("a deleted policy resolved to %s/%s", policy.Project(), policy.ID())
	}
}

// boundPolicy returns the reference the clinician's own binding names, which is
// the only way to obtain a PolicyRef: its fields are unexported and set inside
// the Project that owns the policy.
func boundPolicy(t *testing.T, db *sql.DB) project.PolicyRef {
	t.Helper()

	membership, found, err := NewMembershipResolver(db).Membership(t.Context(), homeProject, clinician)
	if err != nil || !found {
		t.Fatalf("membership found = %v, err = %v, want the clinician's membership", found, err)
	}

	bindings := membership.Policies()
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want the one seeded binding", len(bindings))
	}

	return bindings[0].Policy()
}
