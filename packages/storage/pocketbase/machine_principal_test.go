package pocketbase

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

var (
	loader = project.PrincipalRef{Kind: project.PrincipalClientApplication, ID: "cli_loader"}
	worker = project.PrincipalRef{Kind: project.PrincipalBot, ID: "bot_worker"}
)

// seedMachinePrincipals registers one client application and one bot in prj_a,
// each holding an active membership. A second registration carries the same id
// in prj_b, which is what the containment test needs to be about the Project and
// not about the id.
func seedMachinePrincipals(t *testing.T, db *sql.DB) {
	t.Helper()

	execAll(t, db, []string{
		"INSERT INTO client_applications (project_id, id, name, state, created_at, updated_at) VALUES" +
			" ('prj_a', 'cli_loader', 'Nightly loader', 'active', 0, 0)," +
			" ('prj_b', 'cli_loader', 'Nightly loader', 'active', 0, 0)",

		"INSERT INTO bots (project_id, id, name, state, created_at, updated_at)" +
			" VALUES ('prj_a', 'bot_worker', 'Nightly worker', 'active', 0, 0)",

		"INSERT INTO project_memberships (project_id, id, project_kind, client_application_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_loader', 'standard', 'cli_loader', 'active', 'api', 0, 0, 0)",

		"INSERT INTO project_memberships (project_id, id, project_kind, bot_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_worker', 'standard', 'bot_worker', 'active', 'api', 0, 0, 0)",
	})
}

func machineResolver(t *testing.T) (*MembershipResolver, *sql.DB) {
	t.Helper()

	_, db := newStore(t)
	seedMachinePrincipals(t, db)

	return NewMembershipResolver(db), db
}

// TestAMachinePrincipalResolvesWhileItsRegistrationIsActive is the baseline the
// withdrawal tests below are measured against.
func TestAMachinePrincipalResolvesWhileItsRegistrationIsActive(t *testing.T) {
	resolver, _ := machineResolver(t)

	for _, principal := range []project.PrincipalRef{loader, worker} {
		membership, found, err := resolver.Membership(t.Context(), homeProject, principal)
		if err != nil {
			t.Fatalf("%s: %v", principal.Kind, err)
		}

		if !found {
			t.Fatalf("%s holds no membership though its registration is active", principal.Kind)
		}

		if !membership.HoldsStanding() {
			t.Errorf("%s holds no standing", principal.Kind)
		}
	}
}

// TestARevokedClientApplicationHoldsNoMembership. Standing dies with the
// registration behind it, exactly as a user's dies with its identity: there is
// no MembershipState that could carry the fact, so the row reports nothing.
func TestARevokedClientApplicationHoldsNoMembership(t *testing.T) {
	withdrawals := map[string]string{
		"suspended": "UPDATE client_applications SET state = 'suspended'" +
			" WHERE project_id = 'prj_a' AND id = 'cli_loader'",
		"revoked": "UPDATE client_applications SET state = 'revoked', revoked_at = 1" +
			" WHERE project_id = 'prj_a' AND id = 'cli_loader'",
	}

	for name, withdraw := range withdrawals {
		resolver, db := machineResolver(t)
		execAll(t, db, []string{withdraw})

		_, found, err := resolver.Membership(t.Context(), homeProject, loader)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if found {
			t.Errorf("a %s client application still holds a membership", name)
		}
	}
}

// TestASuspendedBotHoldsNoMembership. A bot presents no credential, so its
// registration row is the only thing that can withdraw it.
func TestASuspendedBotHoldsNoMembership(t *testing.T) {
	resolver, db := machineResolver(t)
	execAll(t, db, []string{
		"UPDATE bots SET state = 'suspended' WHERE project_id = 'prj_a' AND id = 'bot_worker'",
	})

	if _, found, err := resolver.Membership(t.Context(), homeProject, worker); err != nil || found {
		t.Errorf("a suspended bot holds a membership: found %t, err %v", found, err)
	}
}

// TestAClientApplicationFromAnotherProjectNeverResolves. Both Projects register
// the same id, so this fails only if the registry predicate binds the request's
// own Project rather than matching on the id alone.
func TestAClientApplicationFromAnotherProjectNeverResolves(t *testing.T) {
	resolver, db := machineResolver(t)

	// prj_a's registration is withdrawn and prj_b's namesake stays active, so a
	// predicate that forgot its Project would find the wrong one and grant.
	execAll(t, db, []string{
		"UPDATE client_applications SET state = 'suspended' WHERE project_id = 'prj_a' AND id = 'cli_loader'",
	})

	if _, found, err := resolver.Membership(t.Context(), homeProject, loader); err != nil || found {
		t.Errorf("a registration in another Project answered for this one: found %t, err %v", found, err)
	}
}

// TestAKindWithNoRegistryCompilesNoStatement is about the day a fourth principal
// kind is added to PrincipalKind.Valid: the lookup must refuse to compile rather
// than fall through to standing gated by project_memberships.state alone. It
// drives membershipStatement directly, because Membership's own principal check
// would answer first and prove nothing about this arm.
func TestAKindWithNoRegistryCompilesNoStatement(t *testing.T) {
	unknown := project.PrincipalRef{Kind: project.PrincipalKind("agent"), ID: "agt_1"}

	text, args, err := membershipStatement(homeProject, unknown)
	if !errors.Is(err, project.ErrInvalidPrincipal) {
		t.Errorf("got %v, want ErrInvalidPrincipal", err)
	}

	if text != "" || args != nil {
		t.Errorf("a kind with no registry compiled %q with %v", text, args)
	}
}

// TestAnUnrecognisedPrincipalKindResolvesNothing covers the whole path rather
// than one guard on it: whichever check answers first, nothing resolves.
func TestAnUnrecognisedPrincipalKindResolvesNothing(t *testing.T) {
	resolver, _ := machineResolver(t)

	unknown := project.PrincipalRef{Kind: project.PrincipalKind("agent"), ID: "agt_1"}

	_, found, err := resolver.Membership(t.Context(), homeProject, unknown)
	if found {
		t.Fatal("an unrecognised principal kind resolved to a membership")
	}

	if !errors.Is(err, project.ErrInvalidPrincipal) {
		t.Errorf("got %v, want ErrInvalidPrincipal", err)
	}
}

// TestEveryPrincipalKindBindsOneProjectPerRelation. A predicate comparing one
// relation's Project to another's would make the isolation depend on the join
// rather than on the value the request carried.
func TestEveryPrincipalKindBindsOneProjectPerRelation(t *testing.T) {
	kinds := []project.PrincipalRef{
		{Kind: project.PrincipalUser, ID: "usr_clinician"}, loader, worker,
	}

	for _, principal := range kinds {
		text, args, err := membershipStatement(homeProject, principal)
		if err != nil {
			t.Fatalf("%s: %v", principal.Kind, err)
		}

		if got := strings.Count(text, "?"); got != len(args) {
			t.Errorf("%s binds %d placeholders against %d arguments", principal.Kind, got, len(args))
		}

		for _, compared := range []string{"m.project_id = c.project_id", "m.project_id = b.project_id"} {
			if strings.Contains(text, compared) {
				t.Errorf("%s compares one relation's Project to another's", principal.Kind)
			}
		}
	}
}

// TestTheRegistryPredicateSeeksByItsPrimaryKey. The subquery runs on every FHIR
// request a machine principal makes, so a scan of the registry would be a cost
// paid per request rather than a lookup.
func TestTheRegistryPredicateSeeksByItsPrimaryKey(t *testing.T) {
	_, db := machineResolver(t)

	for _, principal := range []project.PrincipalRef{loader, worker} {
		text, args, err := membershipStatement(homeProject, principal)
		if err != nil {
			t.Fatalf("%s: %v", principal.Kind, err)
		}

		plan := queryPlan(t, db, text, args)
		for _, scan := range []string{"SCAN client_applications", "SCAN bots"} {
			if strings.Contains(plan, scan) {
				t.Errorf("%s scans its registry rather than seeking it:\n%s", principal.Kind, plan)
			}
		}
	}
}

// queryPlan returns how SQLite says it will run a statement, which is the only
// way to assert an index is reached rather than merely declared.
func queryPlan(t *testing.T, db *sql.DB, text string, args []any) string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+text, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder

	for rows.Next() {
		var id, parent, notUsed int

		var detail string

		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}

		plan.WriteString(detail + "\n")
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}

	return plan.String()
}
