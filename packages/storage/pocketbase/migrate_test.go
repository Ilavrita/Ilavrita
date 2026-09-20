package pocketbase

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyDatabase is a database of the shape every install created before the
// registries existed: project_memberships carries no principal foreign key, and
// ApplySchema's IF NOT EXISTS will not replace it.
func legacyDatabase(t *testing.T) *sql.DB {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "legacy.db") + "?_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_memberships.sql"))
	if err != nil {
		t.Fatalf("read the legacy declaration: %v", err)
	}

	// The projects parent comes first, then the old membership table, so the
	// current schema's own CREATE TABLE IF NOT EXISTS finds it already there.
	applyStatements(t, db, declarationOf(t, "projects"))
	applyStatements(t, db, string(legacy))

	if err := ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("apply the current schema over the legacy table: %v", err)
	}

	return db
}

// declarationOf returns one table's CREATE statement from the embedded schema.
func declarationOf(t *testing.T, table string) string {
	t.Helper()

	statements, err := splitStatements(schema)
	if err != nil {
		t.Fatalf("split the schema: %v", err)
	}

	opening := "CREATE TABLE IF NOT EXISTS " + table + " ("
	for _, statement := range statements {
		if strings.HasPrefix(statement, opening) {
			return statement + ";"
		}
	}

	t.Fatalf("the schema declares no %s table", table)

	return ""
}

func applyStatements(t *testing.T, db *sql.DB, script string) {
	t.Helper()

	statements, err := splitStatements(script)
	if err != nil {
		t.Fatalf("split script: %v", err)
	}

	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("apply %q: %v", summarize(statement), err)
		}
	}
}

func seedLegacyRows(t *testing.T, db *sql.DB) {
	t.Helper()

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_a', 'standard', 'a', 'A', 'active', 0, 0, 0)",

		"INSERT INTO users (id, scope, home_project_id, email_normalized, email_display," +
			" password_hash, state, created_at, updated_at)" +
			" VALUES ('usr_1', 'project', 'prj_a', 'a@example.test', 'a@example.test', 'hash', 'active', 0, 0)",

		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_1', 'standard', 'usr_1', 'active', 'api', 0, 0, 0)",
	})
}

// TestALegacyDatabaseCarriesNoPrincipalKeys is the premise every test below rests
// on. If this ever stops holding, the rebuild solves a problem that is gone.
func TestALegacyDatabaseCarriesNoPrincipalKeys(t *testing.T) {
	db := legacyDatabase(t)

	err := AssertMembershipPrincipalKeys(t.Context(), db)
	if !errors.Is(err, ErrMembershipPrincipalKeysMissing) {
		t.Fatalf("a legacy database already carries the principal keys: %v", err)
	}

	// It accepts what the constraint exists to refuse, which is the whole reason
	// the rebuild cannot be skipped.
	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO project_memberships (project_id, id, project_kind, client_application_id, state,"+
			" invitation_source, created_at, updated_at, activated_at)"+
			" VALUES ('prj_a', 'pm_ghost', 'standard', 'cli_ghost', 'active', 'api', 0, 0, 0)"); err != nil {
		t.Skipf("the legacy table already refuses a phantom principal: %v", err)
	}
}

// TestAMembershipsTableBuiltBeforeTheRegistriesGainsTheirForeignKeys. SQLite has
// no ALTER TABLE ADD CONSTRAINT and the schema is applied with IF NOT EXISTS, so
// without the rebuild the constraint would reach only new databases.
func TestAMembershipsTableBuiltBeforeTheRegistriesGainsTheirForeignKeys(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	if err := AssertMembershipPrincipalKeys(t.Context(), db); err != nil {
		t.Fatalf("the principal keys are still missing after the rebuild: %v", err)
	}

	// The constraint is adopted, not merely declared: a phantom principal is now
	// refused on the very database that would have accepted one.
	_, err := db.ExecContext(t.Context(),
		"INSERT INTO project_memberships (project_id, id, project_kind, client_application_id, state,"+
			" invitation_source, created_at, updated_at, activated_at)"+
			" VALUES ('prj_a', 'pm_ghost', 'standard', 'cli_ghost', 'active', 'api', 0, 0, 0)")
	if err == nil {
		t.Error("a membership naming no registered client application was still accepted")
	}
}

// TestARebuildKeepsEveryMembershipAndRestoresItsIndexes. The rebuild drops the
// table, so its indexes go with it and the schema file is replayed to put them
// back rather than a second hand-written list being kept in step with it.
func TestARebuildKeepsEveryMembershipAndRestoresItsIndexes(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	var surviving int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM project_memberships WHERE id = 'pm_1'").Scan(&surviving); err != nil {
		t.Fatalf("count memberships: %v", err)
	}

	if surviving != 1 {
		t.Errorf("the rebuild kept %d of 1 membership", surviving)
	}

	// The generated columns are recomputed rather than copied, so they must still
	// answer after the rebuild.
	var kind, principal string
	if err := db.QueryRowContext(t.Context(),
		"SELECT principal_kind, principal_id FROM project_memberships WHERE id = 'pm_1'").Scan(
		&kind, &principal); err != nil {
		t.Fatalf("read the generated columns: %v", err)
	}

	if kind != "user" || principal != "usr_1" {
		t.Errorf("the generated columns read %q/%q", kind, principal)
	}

	wanted := []string{
		"ux_pm_active_principal", "ux_pm_profile", "ix_pm_project_state",
		"ix_pm_super", "ix_pm_principal", "ux_pm_link_sourced", "ix_pm_project_principal",
	}

	present := indexNames(t, db)
	for _, index := range wanted {
		if !slices.Contains(present, index) {
			t.Errorf("%s did not survive the rebuild; present: %v", index, present)
		}
	}
}

// indexNames lists the indexes standing on the membership table.
func indexNames(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(),
		"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'project_memberships'")
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string

	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}

		if name.Valid {
			names = append(names, name.String)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read indexes: %v", err)
	}

	return names
}

// TestARebuildLeavesTheChildTablesPointingAtTheRebuiltTable. The new table is
// renamed into place rather than the old one being renamed away, which is what
// keeps the children's own foreign key text intact.
func TestARebuildLeavesTheChildTablesPointingAtTheRebuiltTable(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	for _, child := range []string{"project_membership_policies", "project_link_capabilities"} {
		var declaration string
		if err := db.QueryRowContext(t.Context(),
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", child).Scan(
			&declaration); err != nil {
			t.Fatalf("read %s: %v", child, err)
		}

		if !strings.Contains(declaration, "REFERENCES project_memberships") {
			t.Errorf("%s no longer references project_memberships:\n%s", child, declaration)
		}

		if strings.Contains(declaration, membershipRebuildTable) {
			t.Errorf("%s points at the rebuild's scratch name:\n%s", child, declaration)
		}
	}
}

// TestTheSelfReferencingInviterKeySurvivesTheRebuild. Only the first occurrence
// of the table name is substituted, so the inviter key keeps naming the final
// table rather than the scratch one.
func TestTheSelfReferencingInviterKeySurvivesTheRebuild(t *testing.T) {
	declaration, err := tableDeclaration(membershipTable, membershipRebuildTable)
	if err != nil {
		t.Fatalf("tableDeclaration: %v", err)
	}

	if !strings.Contains(declaration, "CREATE TABLE IF NOT EXISTS "+membershipRebuildTable) {
		t.Error("the declaration does not create the rebuild table")
	}

	if !strings.Contains(declaration, "REFERENCES project_memberships (project_id, id)") {
		t.Error("the inviter key no longer names the final table")
	}

	if strings.Contains(declaration, "REFERENCES "+membershipRebuildTable) {
		t.Error("the inviter key was rewritten to the scratch name")
	}
}

// TestARebuildRefusesADatabaseHoldingADanglingPrincipal. A membership is never
// deleted to make a constraint pass, so the rebuild reports it and stops.
func TestARebuildRefusesADatabaseHoldingADanglingPrincipal(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	execAll(t, db, []string{
		"INSERT INTO project_memberships (project_id, id, project_kind, client_application_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_ghost', 'standard', 'cli_ghost', 'active', 'api', 0, 0, 0)",
	})

	if err := PrepareSchema(t.Context(), db); !errors.Is(err, ErrDanglingPrincipal) {
		t.Fatalf("got %v, want ErrDanglingPrincipal", err)
	}

	// The refusal changed nothing: the membership it named is still there.
	var surviving int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM project_memberships").Scan(&surviving); err != nil {
		t.Fatalf("count memberships: %v", err)
	}

	if surviving != 2 {
		t.Errorf("a refused rebuild left %d of 2 memberships", surviving)
	}
}

// TestARebuildNamesTheRowsANewConstraintWouldReject, rather than letting the copy
// abort with a bare constraint error naming no row at all.
func TestARebuildNamesTheRowsANewConstraintWouldReject(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	execAll(t, db, []string{
		"INSERT INTO bots (project_id, id, name, state, created_at, updated_at)" +
			" VALUES ('prj_a', 'bot_x', 'Worker', 'active', 0, 0)",
		"INSERT INTO project_memberships (project_id, id, project_kind, bot_id, state, admin," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_2', 'standard', 'bot_x', 'active', 1, 'api', 0, 0, 0)",
	})

	err := PrepareSchema(t.Context(), db)
	if !errors.Is(err, ErrRebuildWouldRejectRow) {
		t.Fatalf("got %v, want ErrRebuildWouldRejectRow", err)
	}

	if !strings.Contains(err.Error(), "administrative standing on a bot") {
		t.Errorf("the refusal does not name what it found: %v", err)
	}
}

// TestARebuildRestoresTheTriggersThatNameTheMembershipTable. A trigger standing
// on another table but naming this one is re-validated by the rename, and the
// table it names is gone by then, so the rename fails unless it is carried.
func TestARebuildRestoresTheTriggersThatNameTheMembershipTable(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	for _, trigger := range []string{"client_application_state_bumps_authz", "bot_state_bumps_authz"} {
		var name string

		err := db.QueryRowContext(t.Context(),
			"SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?", trigger).Scan(&name)
		if err != nil {
			t.Fatalf("%s did not survive the rebuild: %v", trigger, err)
		}
	}

	// Restored in name is not restored in effect: the trigger has to still fire.
	execAll(t, db, []string{
		"INSERT INTO client_applications (project_id, id, name, state, created_at, updated_at)" +
			" VALUES ('prj_a', 'cli_x', 'Loader', 'active', 0, 0)",
		"UPDATE project_memberships SET user_id = NULL, client_application_id = 'cli_x' WHERE id = 'pm_1'",
		"UPDATE client_applications SET state = 'suspended' WHERE project_id = 'prj_a' AND id = 'cli_x'",
	})

	var version int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT authz_version FROM project_memberships WHERE id = 'pm_1'").Scan(&version); err != nil {
		t.Fatalf("read authz_version: %v", err)
	}

	if version < 2 {
		t.Errorf("a registration state change left authz_version at %d, so no cache would notice it", version)
	}
}

// TestTheRebuildIsANoOpOnACurrentDatabase, so a server that restarts does not
// rewrite its membership table every time it starts.
func TestTheRebuildIsANoOpOnACurrentDatabase(t *testing.T) {
	_, db := newStore(t)

	rebuilt, err := rebuildMembershipPrincipalKeys(t.Context(), db)
	if err != nil {
		t.Fatalf("rebuildMembershipPrincipalKeys: %v", err)
	}

	if rebuilt {
		t.Error("a database created from the current schema was rebuilt anyway")
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Errorf("PrepareSchema on a current database: %v", err)
	}
}

// TestApplySchemaStaysIdempotentOnACurrentDatabase. The assertions live in
// PrepareSchema and not in ApplySchema, which keeps ApplySchema's own promise
// that applying a current schema changes nothing and reports no error — and is
// why the assertion cannot be moved into it.
func TestApplySchemaStaysIdempotentOnACurrentDatabase(t *testing.T) {
	_, db := newStore(t)

	for attempt := range 3 {
		if err := ApplySchema(t.Context(), db); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}

	// ApplySchema must not refuse a legacy database, or the rebuild that needs it
	// to have run first could never run at all.
	legacy := legacyDatabase(t)
	if err := ApplySchema(t.Context(), legacy); err != nil {
		t.Errorf("ApplySchema refused a legacy database: %v", err)
	}
}

// TestTheRebuildRestoresForeignKeyEnforcementBeforeItReturns. The pragma is
// per-connection, so a rebuild that left it off would hand back a pool whose
// composite-key guarantees were decorative.
func TestTheRebuildRestoresForeignKeyEnforcementBeforeItReturns(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	if err := AssertForeignKeysEnforced(t.Context(), db); err != nil {
		t.Errorf("foreign keys are no longer enforced after the rebuild: %v", err)
	}
}

// TestTheCopyCarriesNoGeneratedColumn. Inserting into one is a hard error, and
// they are recomputed from the columns beside them in any case.
func TestTheCopyCarriesNoGeneratedColumn(t *testing.T) {
	db := legacyDatabase(t)

	declaration, err := tableDeclaration(membershipTable, membershipRebuildTable)
	if err != nil {
		t.Fatalf("tableDeclaration: %v", err)
	}

	columns, err := copyableColumns(t.Context(), db, membershipTable, declaration)
	if err != nil {
		t.Fatalf("copyableColumns: %v", err)
	}

	for _, generated := range []string{"principal_kind", "principal_id", "link_sourced"} {
		if slices.Contains(columns, generated) {
			t.Errorf("the copy would write the generated column %s", generated)
		}
	}

	for _, stored := range []string{"project_id", "id", "user_id", "client_application_id", "bot_id"} {
		if !slices.Contains(columns, stored) {
			t.Errorf("the copy would drop %s", stored)
		}
	}
}

// TestASystemScopedClientApplicationDocumentRefusesToServe. A client application
// is registered in one Project, so a document outside every Project would be a
// second model of it and the two cannot both be authoritative.
func TestASystemScopedClientApplicationDocumentRefusesToServe(t *testing.T) {
	_, db := newStore(t)

	if err := AssertNoSystemClientApplicationDocuments(t.Context(), db); err != nil {
		t.Fatalf("a clean database was refused: %v", err)
	}

	// The current schema's CHECK refuses the row outright, which is the mechanism
	// on a new database; the guard is what reaches one created before it.
	_, err := db.ExecContext(t.Context(),
		"INSERT INTO platform_resource (project_id, res_type, res_id, version_id, version_seq,"+
			" content, created_at, updated_at) VALUES ('system', 'ClientApplication', 'x', 'v1', 1, '{}', 0, 0)")
	if err == nil {
		t.Error("the current schema accepted a system-scoped ClientApplication document")
	}
}

// oldStringIndex is the search index as a database written before :exact
// declares it: a string row carried the folded value and no other.
const oldStringIndex = `CREATE TABLE fhir_search_index (
  project_id TEXT NOT NULL,
  res_type   TEXT NOT NULL,
  res_id     TEXT NOT NULL,
  param      TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('token', 'string', 'reference', 'date')),
  code       TEXT,
  system     TEXT,
  folded     TEXT,
  lower      BIGINT,
  upper      BIGINT,
  PRIMARY KEY (project_id, res_type, res_id, param, kind, code, system, folded, lower, upper),
  CHECK (
    (kind = 'token' AND code IS NOT NULL AND folded IS NULL AND lower IS NULL AND upper IS NULL)
    OR (kind = 'reference'
      AND code IS NOT NULL AND system IS NULL AND folded IS NULL
      AND lower IS NULL AND upper IS NULL)
    OR (kind = 'string'
      AND folded IS NOT NULL AND folded <> ''
      AND code IS NULL AND system IS NULL AND lower IS NULL AND upper IS NULL)
    OR (kind = 'date'
      AND lower IS NOT NULL AND upper IS NOT NULL
      AND code IS NULL AND system IS NULL AND folded IS NULL)
  )
)`

// TestAnIndexBuiltBeforeExactIsRebuiltRatherThanCarried.
//
// A string row used to carry the folded value and no other, and the constraint
// said so. Carrying those rows across would mean carrying rows the new
// constraint refuses — which is the state this migration is for — so they are
// dropped and derived again from the content the resources already hold.
func TestAnIndexBuiltBeforeExactIsRebuiltRatherThanCarried(t *testing.T) {
	db := legacyDatabase(t)
	seedLegacyRows(t, db)

	// The legacy fixture already declares one, in whatever shape it holds.
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS fhir_search_index"); err != nil {
		t.Fatalf("drop the declared index: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), oldStringIndex); err != nil {
		t.Fatalf("declare the old index: %v", err)
	}

	// A row in the old shape, which the new constraint would refuse.
	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO fhir_search_index (project_id, res_type, res_id, param, kind,"+
			" code, system, folded, lower, upper)"+
			" VALUES ('prj_a', 'Organization', 'org-1', 'name', 'string',"+
			" NULL, NULL, 'a folded name', NULL, NULL)"); err != nil {
		t.Fatalf("plant an old row: %v", err)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("PrepareSchema: %v", err)
	}

	// The constraint is adopted rather than merely declared, which is the whole
	// of what a rebuild is for: SQLite has no ALTER TABLE ADD CONSTRAINT, so
	// without one the new rule would reach only databases created after it.
	var declaration string

	if err := db.QueryRowContext(t.Context(),
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'fhir_search_index'",
	).Scan(&declaration); err != nil {
		t.Fatalf("read the declaration: %v", err)
	}

	if !strings.Contains(declaration, newStringArm) {
		t.Errorf("the index does not require the value as written: %s", declaration)
	}

	// The rows that could not satisfy it are gone, rather than carried across
	// into a table that refuses them.
	var carried int

	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM fhir_search_index").Scan(&carried); err != nil {
		t.Fatalf("count what survived: %v", err)
	}

	if carried != 0 {
		t.Errorf("%d row(s) were carried across", carried)
	}

	// Running it again changes nothing, which is what every migration here has
	// to be able to do — and matters more for this one than for most, because
	// what it does when it fires is empty the search index and derive the whole
	// of it again. A detection that answered yes on an already-migrated
	// database would do that on every start.
	for range 3 {
		if err := PrepareSchema(t.Context(), db); err != nil {
			t.Fatalf("PrepareSchema again: %v", err)
		}
	}

	var recorded int

	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM super_jobs WHERE name = 'migrate.fhir_search_index.string_value'",
	).Scan(&recorded); err != nil {
		t.Fatalf("count the runs: %v", err)
	}

	if recorded != 1 {
		t.Errorf("the migration ran %d times, want once", recorded)
	}
}
