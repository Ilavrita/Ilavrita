package pocketbase

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// TestTheSchemaRefusesAHalfStatedFilter. A rule holding a path with no
// comparator is a restriction nothing can apply, and a restriction nothing
// applies is a rule that reaches further than the row says it does. The schema
// refuses it, so no code path has to decide what half a filter means.
func TestTheSchemaRefusesAHalfStatedFilter(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type," +
		" action, unrestricted, compartment_type, compartment_id, filter_path, filter_comparator," +
		" filter_values) VALUES ('prj_a', 'pol_chart', "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name: "a filter stated in full",
			statement: insert + "10, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'eq', '[\"final\"]')",
		},
		{
			name: "no filter at all",
			statement: insert + "11, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" NULL, NULL, NULL)",
		},
		{
			name: "a path with no comparator",
			statement: insert + "12, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', NULL, '[\"final\"]')",
			refused: true,
		},
		{
			name: "values with no path",
			statement: insert + "13, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" NULL, 'eq', '[\"final\"]')",
			refused: true,
		},
		{
			name: "a comparator outside the enum",
			statement: insert + "14, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'like', '[\"fin%\"]')",
			refused: true,
		},
		{
			name: "an empty path",
			statement: insert + "15, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" '', 'eq', '[\"final\"]')",
			refused: true,
		},
		{
			name: "values that are not a list",
			statement: insert + "16, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'eq', '\"final\"')",
			refused: true,
		},
		{
			name: "an empty list of values",
			statement: insert + "17, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'in', '[]')",
			refused: true,
		},
		{
			name: "equality over two values",
			statement: insert + "18, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'eq', '[\"final\",\"amended\"]')",
			refused: true,
		},
		{
			name: "membership over two values",
			statement: insert + "19, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1'," +
				" 'status', 'in', '[\"final\",\"amended\"]')",
		},
	})
}

// TestARuleRowStatingAFilterCompilesItOntoTheGrant is the whole path: a stored
// policy row, rebuilt into a rule, compiled into the Grant storage enforces.
func TestARuleRowStatingAFilterCompilesItOntoTheGrant(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE access_policy_rules SET filter_path = 'category.coding.code'," +
			" filter_comparator = 'in', filter_values = '[\"vital-signs\",\"laboratory\"]'" +
			" WHERE project_id = 'prj_a' AND policy_id = 'pol_chart' AND ordinal = 0",
	})

	ref := boundPolicy(t, f.db)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v", found, err)
	}

	grants, err := policy.Compile(authz.GrantRequest{
		Project: ref.Project(), Kind: storage.KindFHIR, Type: "Observation",
		Action: storage.ActionRead, Origin: authz.OriginMembership,
		Parameters: authz.Parameters{"patient": "pat-1"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if len(grants) != 1 {
		t.Fatalf("compiled %d grants, want the one rule for this triple", len(grants))
	}

	if grants[0].Filter == nil {
		t.Fatal("the stored filter did not reach the grant, so nothing would apply it")
	}

	want, err := storage.NewFilter("category.coding.code", storage.ComparatorIn, "vital-signs", "laboratory")
	if err != nil {
		t.Fatalf("build the expected filter: %v", err)
	}

	if !grants[0].Filter.Equal(want) {
		t.Errorf("the grant carries %q, want %q", grants[0].Filter, want)
	}
}

// TestARuleRowStatingNoFilterCompilesNone, so the test above is reading the
// stored columns rather than a filter something else supplies.
func TestARuleRowStatingNoFilterCompilesNone(t *testing.T) {
	f := newFixture(t)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), boundPolicy(t, f.db))
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v", found, err)
	}

	for _, rule := range policy.Rules() {
		if _, carried := rule.Filter(); carried {
			t.Errorf("a rule stating no filter rebuilt one for %s %s", rule.Type(), rule.Action())
		}
	}
}

// TestADriftedFilterRowDenies. The schema refuses a half-stated filter, so this
// drives the rebuild directly: if the columns ever arrive incomplete, the rule
// must fail rather than compile without the restriction it was meant to carry.
func TestADriftedFilterRowDenies(t *testing.T) {
	drifted := map[string]ruleRow{
		"a path with no comparator": {
			filterPath: sql.NullString{String: "status", Valid: true},
		},
		"a comparator with no values": {
			filterPath:       sql.NullString{String: "status", Valid: true},
			filterComparator: sql.NullString{String: "eq", Valid: true},
		},
		"values that are not a list": {
			filterPath:       sql.NullString{String: "status", Valid: true},
			filterComparator: sql.NullString{String: "eq", Valid: true},
			filterValues:     sql.NullString{String: `"final"`, Valid: true},
		},
		"a comparator this server does not implement": {
			filterPath:       sql.NullString{String: "status", Valid: true},
			filterComparator: sql.NullString{String: "like", Valid: true},
			filterValues:     sql.NullString{String: `["fin%"]`, Valid: true},
		},
		"a path this server cannot read": {
			filterPath:       sql.NullString{String: "category[0].code", Valid: true},
			filterComparator: sql.NullString{String: "eq", Valid: true},
			filterValues:     sql.NullString{String: `["vital-signs"]`, Valid: true},
		},
	}

	for name, row := range drifted {
		row.kind, row.resourceType, row.action = "fhir", "Observation", "read"
		row.compartmentType = sql.NullString{String: "Patient", Valid: true}
		row.compartmentID = sql.NullString{String: "pat-1", Valid: true}

		if _, err := buildRule(row); !errors.Is(err, ErrUnreadableFilter) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrUnreadableFilter)
		}
	}
}

// TestAnOlderDatabaseGainsTheFilterColumns. The schema is applied with
// IF NOT EXISTS, so a table that already exists never gains a column or a check
// from it; without the rebuild every rule in an existing install would compile
// to a grant narrowed by nothing.
func TestAnOlderDatabaseGainsTheFilterColumns(t *testing.T) {
	db := legacyPolicyRulesDatabase(t)

	if err := AssertRuleRestrictionColumns(t.Context(), db); !errors.Is(err, ErrRuleRestrictionColumnsMissing) {
		t.Fatalf("the legacy table was accepted: err = %v, want %v", err, ErrRuleRestrictionColumnsMissing)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare the legacy database: %v", err)
	}

	if err := AssertRuleRestrictionColumns(t.Context(), db); err != nil {
		t.Fatalf("the prepared database still cannot state a filter: %v", err)
	}

	// The rows it held are still there and still readable as rules.
	var ordinals []int

	rows, err := db.QueryContext(t.Context(),
		"SELECT ordinal FROM access_policy_rules WHERE project_id = 'prj_a' ORDER BY ordinal")
	if err != nil {
		t.Fatalf("read the carried rules: %v", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			t.Fatalf("scan: %v", err)
		}

		ordinals = append(ordinals, ordinal)
	}

	if !slices.Equal(ordinals, []int{0, 1}) {
		t.Errorf("the rebuild carried ordinals %v, want both rows it held", ordinals)
	}

	// The check came with the columns, so the prepared table refuses what the
	// declaration refuses rather than only holding the columns it names.
	_, err = db.ExecContext(t.Context(),
		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action,"+
			" unrestricted, compartment_type, compartment_id, filter_path)"+
			" VALUES ('prj_a', 'pol_chart', 9, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', 'status')")
	if err == nil {
		t.Error("the rebuilt table accepted a half-stated filter, so it gained columns without their check")
	}
}

// legacyPolicyRulesDatabase builds a database whose access_policy_rules table
// predates the filter columns, with rules already in it. The parents are
// declared first so the current schema's own CREATE TABLE IF NOT EXISTS finds
// the old table already there, which is the situation an upgrade is.
func legacyPolicyRulesDatabase(t *testing.T) *sql.DB {
	t.Helper()

	dsn := "file:" + t.TempDir() + "/legacy.db?_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	for _, table := range []string{"projects", "access_policies", "access_policy_parameters"} {
		applyStatements(t, db, declarationOf(t, table))
	}

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_policy_rules.sql"))
	if err != nil {
		t.Fatalf("read the legacy declaration: %v", err)
	}

	applyStatements(t, db, string(legacy))

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_a', 'standard', 'prj_a', 'prj_a', 'active', 0, 0, 0)",
		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES ('prj_a', 'pol_chart', 'Own chart', 0, 0)",
		"INSERT INTO access_policy_parameters (project_id, policy_id, name)" +
			" VALUES ('prj_a', 'pol_chart', 'patient')",
		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param) VALUES" +
			" ('prj_a', 'pol_chart', 0, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, 'patient')," +
			" ('prj_a', 'pol_chart', 1, 'fhir', 'Practitioner', 'search', 1, NULL, NULL, NULL)",
	})

	return db
}

// TestAHalfMigratedTableIsBroughtForward. A table carrying some restriction
// columns but not all is what a hand-run ALTER leaves behind, and what one
// release of this server's own schema looks like to the next. Every column is
// checked rather than the first, so such a table is rebuilt rather than served
// with rules whose restriction has nowhere to live.
func TestAHalfMigratedTableIsBroughtForward(t *testing.T) {
	db := legacyPolicyRulesDatabase(t)

	execAll(t, db, []string{"ALTER TABLE access_policy_rules ADD COLUMN filter_path TEXT"})

	if err := AssertRuleRestrictionColumns(t.Context(), db); !errors.Is(err, ErrRuleRestrictionColumnsMissing) {
		t.Fatalf("a half-migrated table was accepted: err = %v, want %v", err, ErrRuleRestrictionColumnsMissing)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare a half-migrated database: %v", err)
	}

	if err := AssertRuleRestrictionColumns(t.Context(), db); err != nil {
		t.Fatalf("the prepared database still cannot state a restriction: %v", err)
	}

	// The rules it held came through the repair.
	var carried int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM access_policy_rules WHERE project_id = 'prj_a'").Scan(&carried); err != nil {
		t.Fatalf("count the carried rules: %v", err)
	}

	if carried != 2 {
		t.Errorf("the repair carried %d rules, want both", carried)
	}
}

// TestTheSchemaRefusesAProjectionNobodyMeantToWrite. NULL returns the whole
// resource; a list returns those members. An empty list returns nothing, which
// is indistinguishable from a list nothing ever bound.
func TestTheSchemaRefusesAProjectionNobodyMeantToWrite(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type," +
		" action, unrestricted, compartment_type, compartment_id, returns)" +
		" VALUES ('prj_a', 'pol_chart', "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name:      "a list of elements",
			statement: insert + "20, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', '[\"status\"]')",
		},
		{
			name:      "no projection at all",
			statement: insert + "21, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', NULL)",
		},
		{
			name:      "an empty list",
			statement: insert + "22, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', '[]')",
			refused:   true,
		},
		{
			name:      "something that is not a list",
			statement: insert + "23, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', '\"status\"')",
			refused:   true,
		},
		{
			name:      "text that is not json",
			statement: insert + "24, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', 'status')",
			refused:   true,
		},
	})
}

// TestARuleRowNamingElementsCompilesThemOntoTheGrant is the projection's whole
// path: a stored policy row, rebuilt into a rule, compiled into the Grant
// storage narrows with.
func TestARuleRowNamingElementsCompilesThemOntoTheGrant(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE access_policy_rules SET returns = '[\"status\",\"valueQuantity\"]'" +
			" WHERE project_id = 'prj_a' AND policy_id = 'pol_chart' AND ordinal = 0",
	})

	ref := boundPolicy(t, f.db)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v", found, err)
	}

	grants, err := policy.Compile(authz.GrantRequest{
		Project: ref.Project(), Kind: storage.KindFHIR, Type: "Observation",
		Action: storage.ActionRead, Origin: authz.OriginMembership,
		Parameters: authz.Parameters{"patient": "pat-1"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if len(grants) != 1 || grants[0].Projection == nil {
		t.Fatalf("the stored projection did not reach the grant: %v", grants)
	}

	want, err := storage.NewProjection("status", "valueQuantity")
	if err != nil {
		t.Fatalf("build the expected projection: %v", err)
	}

	if !grants[0].Projection.Equal(want) {
		t.Errorf("the grant returns %q, want %q", grants[0].Projection, want)
	}
}

// TestARuleRowNamingNoElementsReturnsEverything, so the test above reads the
// stored column rather than a projection something else supplies.
func TestARuleRowNamingNoElementsReturnsEverything(t *testing.T) {
	f := newFixture(t)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), boundPolicy(t, f.db))
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v", found, err)
	}

	for _, rule := range policy.Rules() {
		if _, carried := rule.Projection(); carried {
			t.Errorf("a rule naming no elements rebuilt a projection for %s %s", rule.Type(), rule.Action())
		}
	}
}

// TestADriftedProjectionRowDenies rather than returning the whole resource,
// which is what dropping an unreadable restriction would do.
func TestADriftedProjectionRowDenies(t *testing.T) {
	drifted := map[string]string{
		"a name that is not an element": `["code.coding"]`,
		"an empty name":                 `[""]`,
		"a list of something else":      `[{"path":"status"}]`,
		"not a list at all":             `"status"`,
	}

	for name, returns := range drifted {
		row := ruleRow{
			kind: "fhir", resourceType: "Observation", action: "read",
			compartmentType: sql.NullString{String: "Patient", Valid: true},
			compartmentID:   sql.NullString{String: "pat-1", Valid: true},
			returns:         sql.NullString{String: returns, Valid: true},
		}

		if _, err := buildRule(row); !errors.Is(err, ErrUnreadableProjection) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrUnreadableProjection)
		}
	}
}

// TestTheSchemaRefusesTwoWaysOfNamingASubject. A rule states its restriction
// one way: a literal id, a set of them, or a parameter. Two at once is a row
// whose meaning depends on which one the reader looks at first.
func TestTheSchemaRefusesTwoWaysOfNamingASubject(t *testing.T) {
	_, db := newStore(t)
	seedPolicies(t, db)

	const insert = "INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type," +
		" action, unrestricted, compartment_type, compartment_id, compartment_ids, compartment_param)" +
		" VALUES ('prj_a', 'pol_chart', "

	runSchemaCases(t, db, []struct {
		name      string
		statement string
		refused   bool
	}{
		{
			name:      "a set of subjects",
			statement: insert + "30, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, '[\"pat-1\",\"pat-2\"]', NULL)",
		},
		{
			name:      "one literal subject",
			statement: insert + "31, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', NULL, NULL)",
		},
		{
			name:      "a parameter",
			statement: insert + "32, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, NULL, 'patient')",
		},
		{
			name:      "a literal and a set",
			statement: insert + "33, 'fhir', 'Observation', 'read', 0, 'Patient', 'pat-1', '[\"pat-2\"]', NULL)",
			refused:   true,
		},
		{
			name:      "a set and a parameter",
			statement: insert + "34, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, '[\"pat-1\"]', 'patient')",
			refused:   true,
		},
		{
			name:      "a set with no subject type",
			statement: insert + "35, 'fhir', 'Observation', 'read', 0, NULL, NULL, '[\"pat-1\"]', NULL)",
			refused:   true,
		},
		{
			name:      "an empty set",
			statement: insert + "36, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, '[]', NULL)",
			refused:   true,
		},
		{
			name:      "a set that is not a list",
			statement: insert + "37, 'fhir', 'Observation', 'read', 0, 'Patient', NULL, '\"pat-1\"', NULL)",
			refused:   true,
		},
		{
			name:      "an unrestricted rule naming a set",
			statement: insert + "38, 'fhir', 'Organization', 'read', 1, NULL, NULL, '[\"pat-1\"]', NULL)",
			refused:   true,
		},
	})
}

// TestARuleRowNamingASetMintsOneGrantEach, which is what makes the stored set
// the same restriction as the rows it replaces.
func TestARuleRowNamingASetMintsOneGrantEach(t *testing.T) {
	f := newFixture(t)

	execAll(t, f.db, []string{
		"UPDATE access_policy_rules SET compartment_param = NULL," +
			" compartment_ids = '[\"pat-1\",\"pat-2\"]'" +
			" WHERE project_id = 'prj_a' AND policy_id = 'pol_chart' AND ordinal = 0",
		"DELETE FROM access_policy_parameters WHERE project_id = 'prj_a' AND policy_id = 'pol_chart'",
	})

	ref := boundPolicy(t, f.db)

	policy, found, err := NewPolicyResolver(f.db).Policy(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("policy found = %v, err = %v", found, err)
	}

	grants, err := policy.Compile(authz.GrantRequest{
		Project: ref.Project(), Kind: storage.KindFHIR, Type: "Observation",
		Action: storage.ActionRead, Origin: authz.OriginMembership,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if len(grants) != 2 {
		t.Fatalf("a stored set of two compiled %d grants, want one each", len(grants))
	}

	reached := map[storage.Compartment]bool{}
	for _, grant := range grants {
		if grant.Compartment == nil {
			t.Fatal("a stored set compiled an unconfined grant")
		}

		reached[*grant.Compartment] = true
	}

	for _, id := range []storage.LogicalID{"pat-1", "pat-2"} {
		if !reached[storage.Compartment{Type: "Patient", ID: id}] {
			t.Errorf("no grant reaches %s", id)
		}
	}
}

// TestADriftedSubjectSetDenies rather than falling back to another of the
// rule's shapes, each of which restricts differently.
func TestADriftedSubjectSetDenies(t *testing.T) {
	drifted := map[string]string{
		"not a list":      `"pat-1"`,
		"an empty list":   `[]`,
		"not json at all": `pat-1`,
	}

	for name, ids := range drifted {
		row := ruleRow{
			kind: "fhir", resourceType: "Observation", action: "read",
			compartmentType: sql.NullString{String: "Patient", Valid: true},
			compartmentIDs:  sql.NullString{String: ids, Valid: true},
		}

		if _, err := buildRule(row); !errors.Is(err, ErrUnreadableRule) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrUnreadableRule)
		}
	}
}

// legacySecondFactorDatabase builds a database whose user_second_factors table
// predates the replacement column, with a factor already in force.
func legacySecondFactorDatabase(t *testing.T) *sql.DB {
	t.Helper()

	dsn := "file:" + t.TempDir() + "/legacy.db?_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// users names projects, so the parent is declared first.
	applyStatements(t, db, declarationOf(t, "projects"))
	applyStatements(t, db, declarationOf(t, "users"))

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_second_factors.sql"))
	if err != nil {
		t.Fatalf("read the legacy declaration: %v", err)
	}

	applyStatements(t, db, string(legacy))

	execAll(t, db, []string{
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)" +
			" VALUES ('usr_1', 'server', 'a@example.test', 'a@example.test', 'active', 0, 0)",
		"INSERT INTO user_second_factors (user_id, state, sealed_secret, last_step, created_at, activated_at)" +
			" VALUES ('usr_1', 'active', 'sealed-material', 7, 0, 0)",
	})

	return db
}

// TestAnInstallThatPredatesTheReplacementColumnIsBroughtForward.
//
// The table is created by the schema like any other, so without this an install
// that already holds second factors would come up naming a column that is not
// there — and every login by somebody holding one would fail. The factor itself
// has to survive, or bringing the database forward would be the same as taking
// everyone's second factor away.
func TestAnInstallThatPredatesTheReplacementColumnIsBroughtForward(t *testing.T) {
	db := legacySecondFactorDatabase(t)

	if err := AssertFactorReplacement(t.Context(), db); !errors.Is(err, ErrFactorReplacementMissing) {
		t.Fatalf("the legacy table was accepted: err = %v, want %v", err, ErrFactorReplacementMissing)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare the legacy database: %v", err)
	}

	if err := AssertFactorReplacement(t.Context(), db); err != nil {
		t.Fatalf("the prepared database still cannot hold a replacement: %v", err)
	}

	var (
		state, sealed string
		step          int64
		pending       sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		"SELECT state, sealed_secret, pending_secret, last_step FROM user_second_factors"+
			" WHERE user_id = 'usr_1'").Scan(&state, &sealed, &pending, &step)
	if err != nil {
		t.Fatalf("the factor did not survive: %v", err)
	}

	if state != "active" || sealed != "sealed-material" || step != 7 {
		t.Errorf("the factor came through as %s/%q/%d", state, sealed, step)
	}

	if pending.Valid {
		t.Error("a factor that was in force came through as one being replaced")
	}

	// The check came with the column, so the table refuses what the declaration
	// refuses rather than only holding the column it names.
	if _, err := db.ExecContext(t.Context(),
		"UPDATE user_second_factors SET state = 'pending', activated_at = NULL,"+
			" pending_secret = 'x' WHERE user_id = 'usr_1'"); err == nil {
		t.Error("a replacement beside a pending factor was accepted")
	}
}

// TestPreparingTwiceDoesNotRebuildTheFactorTable.
//
// A rebuild copies every row, so one that happened needlessly would leave no
// trace in the data. What it cannot survive is a column the current declaration
// does not name: the rebuild refuses rather than dropping it. So a column added
// by hand is what tells a second prepare that rebuilt from one that did not.
func TestPreparingTwiceDoesNotRebuildTheFactorTable(t *testing.T) {
	db := legacySecondFactorDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	execAll(t, db, []string{
		"ALTER TABLE user_second_factors ADD COLUMN operator_note TEXT",
	})

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("preparing a database that already has the column rebuilt it: %v", err)
	}

	// And the factor is still there, with the column beside it.
	var note sql.NullString
	if err := db.QueryRowContext(t.Context(),
		"SELECT operator_note FROM user_second_factors WHERE user_id = 'usr_1'").Scan(&note); err != nil {
		t.Errorf("the second prepare disturbed the table: %v", err)
	}
}
