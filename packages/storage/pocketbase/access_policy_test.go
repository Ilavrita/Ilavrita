package pocketbase

import (
	"database/sql"
	"testing"
)

// seedPolicies stands up the two Projects' policies, one declared parameter, and
// a direct and a link-minted membership, which is everything the wiring tests
// below need to name.
func seedPolicies(t *testing.T, db *sql.DB) {
	t.Helper()

	statements := []string{
		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES ('prj_a', 'pol_chart', 'Own chart', 0, 0), ('prj_b', 'pol_wide', 'Everything', 0, 0)",
		"INSERT INTO access_policy_parameters (project_id, policy_id, name) VALUES ('prj_a', 'pol_chart', 'patient')",
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)" +
			" VALUES ('usr_1', 'server', 'a@example.test', 'a@example.test', 'active', 0, 0)," +
			" ('usr_2', 'server', 'b@example.test', 'b@example.test', 'active', 0, 0)",
		"INSERT INTO project_links (grantee_project, grantor_project, kind, status, created_at, updated_at)" +
			" VALUES ('prj_b', 'prj_a', 'administrative', 'proposed', 0, 0)",
		"INSERT INTO project_memberships" +
			" (project_id, id, project_kind, user_id, state, invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_direct', 'standard', 'usr_1', 'active', 'api', 0, 0, 0)",
		"INSERT INTO project_memberships" +
			" (project_id, id, project_kind, user_id, state, invitation_source," +
			" via_link_grantee_project, via_link_kind, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_linked', 'standard', 'usr_2', 'active', 'link', 'prj_b', 'administrative', 0, 0, 0)",
	}

	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", summarize(statement), err)
		}
	}
}

// runSchemaCases asserts each statement is accepted or refused by the schema
// itself, so an invariant that only application code enforces fails here.
func runSchemaCases(t *testing.T, db *sql.DB, cases []struct {
	name      string
	statement string
	refused   bool
},
) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), tc.statement)
			if tc.refused != (err != nil) {
				t.Fatalf("statement refused = %v, want %v: %v", err != nil, tc.refused, err)
			}
		})
	}
}

func TestAccessPolicyRuleStatesExactlyOneRestriction(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO access_policy_rules" +
		" (project_id, policy_id, ordinal, kind, res_type, action," +
		" unrestricted, compartment_type, compartment_id, compartment_param) VALUES "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name:      "a literal subject",
			statement: insert + "('prj_a', 'pol_chart', 0, 'fhir', 'Patient', 'read', 0, 'Patient', 'pat_1', NULL)",
		},
		{
			name:      "a declared parameter",
			statement: insert + "('prj_a', 'pol_chart', 1, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, 'patient')",
		},
		{
			name:      "an explicitly unrestricted rule",
			statement: insert + "('prj_a', 'pol_chart', 2, 'fhir', 'Practitioner', 'search', 1, NULL, NULL, NULL)",
		},
		{
			name:      "no restriction at all",
			statement: insert + "('prj_a', 'pol_chart', 3, 'fhir', 'Observation', 'search', 0, NULL, NULL, NULL)",
			refused:   true,
		},
		{
			name:      "a literal subject and a parameter at once",
			statement: insert + "('prj_a', 'pol_chart', 4, 'fhir', 'Observation', 'search', 0, 'Patient', 'pat_1', 'patient')",
			refused:   true,
		},
		{
			name:      "unrestricted while still naming a subject",
			statement: insert + "('prj_a', 'pol_chart', 5, 'fhir', 'Observation', 'search', 1, 'Patient', 'pat_1', NULL)",
			refused:   true,
		},
		{
			name:      "an undeclared parameter",
			statement: insert + "('prj_a', 'pol_chart', 6, 'fhir', 'Observation', 'search', 0, 'Patient', NULL, 'clinician')",
			refused:   true,
		},
		{
			name:      "an action outside the enum",
			statement: insert + "('prj_a', 'pol_chart', 7, 'fhir', 'Observation', 'vread', 1, NULL, NULL, NULL)",
			refused:   true,
		},
		{
			name:      "a kind outside the enum",
			statement: insert + "('prj_a', 'pol_chart', 8, 'clinical', 'Observation', 'read', 1, NULL, NULL, NULL)",
			refused:   true,
		},
		{
			name:      "a policy that does not exist",
			statement: insert + "('prj_a', 'pol_ghost', 0, 'fhir', 'Observation', 'read', 1, NULL, NULL, NULL)",
			refused:   true,
		},
	})
}

func TestADataLinkResolvesItsRestrictionInTheGrantor(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO project_links" +
		" (grantee_project, grantor_project, kind, status, grantor_access_policy_id, created_at, updated_at) VALUES "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name:      "the grantor's own policy",
			statement: insert + "('prj_b', 'prj_a', 'data', 'proposed', 'pol_chart', 0, 0)",
		},
		{
			name:      "a data link naming no policy",
			statement: insert + "('prj_b', 'prj_a', 'data', 'proposed', NULL, 0, 0)",
			refused:   true,
		},
		{
			name:      "the grantee's own same-named policy",
			statement: insert + "('prj_b', 'prj_a', 'data', 'proposed', 'pol_wide', 0, 0)",
			refused:   true,
		},
		{
			name:      "an administrative link carrying a policy",
			statement: insert + "('prj_b', 'prj_a', 'administrative', 'proposed', 'pol_chart', 0, 0)",
			refused:   true,
		},
	})
}

func TestAPolicyBindsOnlyToADirectMembershipInTheOwningProject(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO project_membership_policies" +
		" (project_id, membership_id, policy_id, ordinal, policy_params) VALUES "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name:      "a direct membership binding its Project's policy",
			statement: insert + `('prj_a', 'pm_direct', 'pol_chart', 0, '{"patient":"pat_1"}')`,
		},
		{
			name:      "a link-minted membership binding anything",
			statement: insert + "('prj_a', 'pm_linked', 'pol_chart', 0, '{}')",
			refused:   true,
		},
		{
			name: "a binding claiming the membership is direct",
			statement: "INSERT INTO project_membership_policies" +
				" (project_id, membership_id, policy_id, membership_link_sourced)" +
				" VALUES ('prj_a', 'pm_linked', 'pol_chart', 1)",
			refused: true,
		},
		{
			name:      "a binding naming another Project's policy",
			statement: insert + "('prj_a', 'pm_direct', 'pol_wide', 1, '{}')",
			refused:   true,
		},
		{
			name:      "a binding on a membership that does not exist",
			statement: insert + "('prj_a', 'pm_ghost', 'pol_chart', 0, '{}')",
			refused:   true,
		},
	})
}

func TestABoundAccessPolicySurvivesItsBindings(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	_, err := db.ExecContext(t.Context(),
		"INSERT INTO project_membership_policies (project_id, membership_id, policy_id)"+
			" VALUES ('prj_a', 'pm_direct', 'pol_chart')")
	if err != nil {
		t.Fatalf("bind policy: %v", err)
	}

	_, err = db.ExecContext(t.Context(),
		"DELETE FROM access_policies WHERE project_id = 'prj_a' AND id = 'pol_chart'")
	if err == nil {
		t.Error("deleting a bound policy left a membership resolving against nothing")
	}
}
