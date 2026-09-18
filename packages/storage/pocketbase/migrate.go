package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrMembershipPrincipalKeysMissing reports a project_memberships table naming
	// no client application or bot parent. Every containment guarantee for a
	// machine principal rests on those keys, so a database without them is refused
	// rather than served with the guarantee quietly downgraded.
	ErrMembershipPrincipalKeysMissing = errors.New(
		"pocketbase: project_memberships is missing its principal foreign keys")

	// ErrFactorReplacementMissing reports a user_second_factors table with
	// nowhere to hold a replacement awaiting proof. Without that column a
	// factor can only be replaced by being switched off first, which is what a
	// stolen session would want, so a database without it is refused.
	ErrFactorReplacementMissing = errors.New(
		"pocketbase: user_second_factors cannot hold a replacement awaiting proof")

	// ErrRuleRestrictionColumnsMissing reports an access_policy_rules table with
	// nowhere to state a filter or a projection. A rule that cannot record its
	// restriction compiles to a Grant narrowed by nothing, which reaches further
	// than the policy says, so a database without the columns is refused.
	ErrRuleRestrictionColumnsMissing = errors.New(
		"pocketbase: access_policy_rules is missing its restriction columns")

	// ErrRebuildWouldDropColumn reports an old table holding a column the current
	// declaration does not. The rebuild copies rows, so a dropped column is lost
	// data and the rebuild refuses rather than performing it.
	ErrRebuildWouldDropColumn = errors.New("pocketbase: the rebuilt table would drop a column the old one holds")

	// ErrDanglingPrincipal reports a membership naming a client application or bot
	// with no registry row. The constraint cannot be adopted over it, and the
	// membership is never deleted to make the constraint pass.
	ErrDanglingPrincipal = errors.New("pocketbase: a membership names a principal no registry row describes")

	// ErrRebuildWouldRejectRow reports a row the current declaration refuses, found
	// before the rebuild touches anything. The copy would abort naming no row, so
	// the offending ones are reported instead.
	ErrRebuildWouldRejectRow = errors.New("pocketbase: an existing row violates a constraint the rebuild adds")

	// ErrSystemClientApplicationDocument reports a platform_resource row claiming a
	// system-scoped ClientApplication. A client application is registered in one
	// Project, so a document outside every Project is a second model of it, and the
	// two cannot both be authoritative.
	ErrSystemClientApplicationDocument = errors.New(
		"pocketbase: a system-scoped ClientApplication document contradicts the client application registry")
)

// membershipTable is the table the principal keys are adopted onto, and
// rebuildTable is the name its replacement is built under before the rename.
const (
	membershipTable = "project_memberships"

	// membershipRebuildTable is the scratch name its replacement is built under
	// before the rename.
	membershipRebuildTable = membershipTable + "_new"

	// ruleTable holds the access policy rules a restriction is stated on.
	ruleTable = "access_policy_rules"

	// searchIndexTable holds what each resource is searchable by.
	searchIndexTable = "fhir_search_index"

	// factorTable holds the second factor an identity proved.
	factorTable = "user_second_factors"
)

// principalParents are the registries project_memberships must name, each with
// the column its key binds. Both are checked, because a key attached to one of
// them would satisfy a check that counted parents rather than naming them.
var principalParents = map[string]string{
	"client_applications": "client_application_id",
	"bots":                "bot_id",
}

// PrepareSchema brings a database to the current schema and refuses to hand back
// one missing a constraint the declarations depend on. It is what a server calls;
// ApplySchema stays the plain, idempotent application of the file.
func PrepareSchema(ctx context.Context, db *sql.DB) error {
	// Asked before the schema is applied, because applying it is what creates
	// the table: afterwards there is no way to tell an install that predates the
	// search index from one that has nothing to put in it.
	indexed, err := hasTable(ctx, db, searchIndexTable)
	if err != nil {
		return err
	}

	if err := ApplySchema(ctx, db); err != nil {
		return err
	}

	if !indexed {
		if err := backfillSearchIndex(ctx, db); err != nil {
			return err
		}
	}

	memberships, err := rebuildMembershipPrincipalKeys(ctx, db)
	if err != nil {
		return err
	}

	restrictions, err := rebuildRuleRestrictions(ctx, db)
	if err != nil {
		return err
	}

	factors, err := rebuildFactorReplacement(ctx, db)
	if err != nil {
		return err
	}

	// A rebuild drops its table, and that table's indexes and triggers go with
	// it. The file is the only definition of them, so it is replayed rather than
	// a second hand-written list kept in step with it.
	if memberships || restrictions || factors {
		if err := ApplySchema(ctx, db); err != nil {
			return err
		}
	}

	if err := AssertMembershipPrincipalKeys(ctx, db); err != nil {
		return err
	}

	if err := AssertRuleRestrictionColumns(ctx, db); err != nil {
		return err
	}

	if err := AssertFactorReplacement(ctx, db); err != nil {
		return err
	}

	return AssertNoSystemClientApplicationDocuments(ctx, db)
}

// AssertMembershipPrincipalKeys refuses a database whose membership principal
// columns are unconstrained. It checks the column each key binds and not only the
// parent it names, because a key attached to the wrong column would satisfy a
// check that read the parent table alone.
func AssertMembershipPrincipalKeys(ctx context.Context, db *sql.DB) error {
	bound, err := principalKeyColumns(ctx, conn(ctx, db))
	if err != nil {
		return err
	}

	for parent, column := range principalParents {
		if bound[parent] != column {
			return fmt.Errorf("%w: %s names %q, not %q",
				ErrMembershipPrincipalKeysMissing, parent, bound[parent], column)
		}
	}

	return nil
}

// principalKeyColumns reads which column each registry key binds. The tenant
// column is skipped, so what is reported is the column that decides whether the
// constraint is about the principal or only about the Project.
func principalKeyColumns(ctx context.Context, db executor) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_list("+membershipTable+")")
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read %s foreign keys: %w", membershipTable, err)
	}
	defer func() { _ = rows.Close() }()

	bound := map[string]string{}

	for rows.Next() {
		var (
			id, seq                   int
			table, from               string
			onUpdate, onDelete, match string
			to                        sql.NullString
		)

		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, fmt.Errorf("pocketbase: scan %s foreign keys: %w", membershipTable, err)
		}

		if _, wanted := principalParents[table]; wanted && from != "project_id" {
			bound[table] = from
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read %s foreign keys: %w", membershipTable, err)
	}

	return bound, nil
}

// AssertNoSystemClientApplicationDocuments refuses a database holding one. The
// CHECK that forbids it reaches only databases created from the current schema,
// so an older one is proven empty rather than assumed to be.
func AssertNoSystemClientApplicationDocuments(ctx context.Context, db *sql.DB) error {
	const query = "SELECT COUNT(*) FROM platform_resource" +
		" WHERE project_id = 'system' AND res_type = 'ClientApplication'"

	var held int
	if err := conn(ctx, db).QueryRowContext(ctx, query).Scan(&held); err != nil {
		return fmt.Errorf("pocketbase: read system client application documents: %w", err)
	}

	if held > 0 {
		return fmt.Errorf("%w: %d rows", ErrSystemClientApplicationDocument, held)
	}

	return nil
}

// rebuild describes one table replaced by its current declaration. SQLite has no
// ALTER TABLE ADD CONSTRAINT and the schema is applied with IF NOT EXISTS, so a
// constraint added to a table that already exists reaches only databases created
// afterwards unless the table is rebuilt.
type rebuild struct {
	table       string
	replacement string
	declaration string
	columns     []string

	// finishing are statements the rename leaves to be re-made inside the same
	// transaction, before the foreign key check reads the result.
	finishing []string
}

// planRebuild reads what the copy must carry across. A column the current
// declaration does not name is refused rather than dropped.
func planRebuild(ctx context.Context, db *sql.DB, table string, finishing ...string) (rebuild, error) {
	replacement := table + "_new"

	declaration, err := tableDeclaration(table, replacement)
	if err != nil {
		return rebuild{}, err
	}

	columns, err := copyableColumns(ctx, db, table, declaration)
	if err != nil {
		return rebuild{}, err
	}

	return rebuild{
		table: table, replacement: replacement,
		declaration: declaration, columns: columns, finishing: finishing,
	}, nil
}

// performRebuild runs one plan with foreign keys suspended for its duration.
func performRebuild(ctx context.Context, db *sql.DB, plan rebuild) error {
	// The pragma is per-connection and a no-op inside a transaction, so the whole
	// rebuild runs on one connection with the toggle outside the transaction.
	pinned, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pocketbase: reserve a connection for the rebuild: %w", err)
	}
	defer func() { _ = pinned.Close() }()

	if _, err := pinned.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("pocketbase: suspend foreign keys for the rebuild: %w", err)
	}

	// Enforcement is restored on every path, including the one where the rebuild
	// failed, so no later work on this connection runs with it off.
	defer func() { _, _ = pinned.ExecContext(ctx, "PRAGMA foreign_keys = ON") }()

	return runRebuild(ctx, pinned, plan)
}

// rebuildMembershipPrincipalKeys adopts the principal foreign keys onto a table
// that already exists.
func rebuildMembershipPrincipalKeys(ctx context.Context, db *sql.DB) (bool, error) {
	needed, err := rebuildNeeded(ctx, db)
	if err != nil || !needed {
		return false, err
	}

	if err := refuseRowsTheRebuildWouldReject(ctx, db); err != nil {
		return false, err
	}

	// ux_pm_link_sourced is the parent key project_membership_policies' own
	// three-column foreign key resolves against, not merely an index. Without it
	// the check at the end of the rebuild reports a schema mismatch rather than
	// a row.
	plan, err := planRebuild(ctx, db, membershipTable,
		"CREATE UNIQUE INDEX IF NOT EXISTS ux_pm_link_sourced"+
			" ON "+membershipTable+" (project_id, id, link_sourced)")
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	return true, nil
}

// restrictionColumns are the columns a rule states its narrowing on. Every one
// is checked, so a table carrying some of them — a half-run migration, or one
// release of this server's own schema — is brought forward rather than served.
var restrictionColumns = []string{
	"filter_path", "filter_comparator", "filter_values", "returns", "compartment_ids",
}

// rebuildRuleRestrictions adopts the restriction columns and their checks onto
// an access_policy_rules table that predates any of them. A rule that predates
// them carries no restriction, so the copy has nothing the new checks reject.
func rebuildRuleRestrictions(ctx context.Context, db *sql.DB) (bool, error) {
	missing, err := missingRestrictionColumn(ctx, db)
	if err != nil || missing == "" {
		return false, err
	}

	plan, err := planRebuild(ctx, db, ruleTable)
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	return true, nil
}

// rebuildFactorReplacement adopts the replacement column onto a
// user_second_factors table that predates it.
//
// The table is created by the schema like any other, so an install that
// predates the column keeps its shape: every read of a factor would then name a
// column that is not there, and every login by somebody holding one would fail.
func rebuildFactorReplacement(ctx context.Context, db *sql.DB) (bool, error) {
	present, err := hasColumn(ctx, db, factorTable, "pending_secret")
	if err != nil || present {
		return false, err
	}

	// A table that is not there yet is created by the schema with the column
	// already in it, and has nothing to carry across.
	declared, err := hasTable(ctx, db, factorTable)
	if err != nil || !declared {
		return false, err
	}

	plan, err := planRebuild(ctx, db, factorTable)
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	return true, nil
}

// AssertFactorReplacement refuses a database that cannot hold a replacement
// awaiting proof. Without it a factor can only be replaced by being switched
// off first, which is what somebody holding a stolen session would want.
func AssertFactorReplacement(ctx context.Context, db *sql.DB) error {
	present, err := hasColumn(ctx, db, factorTable, "pending_secret")
	if err != nil {
		return err
	}

	if !present {
		return fmt.Errorf("%w: %s.pending_secret", ErrFactorReplacementMissing, factorTable)
	}

	return nil
}

// AssertRuleRestrictionColumns refuses a database whose policy rules cannot
// state their narrowing. A rule row with nowhere to record one compiles to a
// Grant narrowed by nothing, which is wider than the policy says.
func AssertRuleRestrictionColumns(ctx context.Context, db *sql.DB) error {
	missing, err := missingRestrictionColumn(ctx, db)
	if err != nil {
		return err
	}

	if missing != "" {
		return fmt.Errorf("%w: %s.%s", ErrRuleRestrictionColumnsMissing, ruleTable, missing)
	}

	return nil
}

// missingRestrictionColumn names the first restriction column the table does
// not declare, or the empty string when it declares them all.
func missingRestrictionColumn(ctx context.Context, db *sql.DB) (string, error) {
	for _, column := range restrictionColumns {
		present, err := hasColumn(ctx, db, ruleTable, column)
		if err != nil {
			return "", err
		}

		if !present {
			return column, nil
		}
	}

	return "", nil
}

// hasTable reports whether a database already declares one table.
func hasTable(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var name string

	err := conn(ctx, db).QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocketbase: look for the %s table: %w", table, err)
	}

	return true, nil
}

// hasColumn reports whether a table declares one column.
func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := conn(ctx, db).QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("pocketbase: read %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid, notNull, primaryKey int
			name, kind               string
			fallback                 sql.NullString
		)

		if err := rows.Scan(&cid, &name, &kind, &notNull, &fallback, &primaryKey); err != nil {
			return false, fmt.Errorf("pocketbase: scan %s columns: %w", table, err)
		}

		if name == column {
			return true, nil
		}
	}

	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("pocketbase: read %s columns: %w", table, err)
	}

	return false, nil
}

// rebuildNeeded reports whether the table is missing a principal key. A database
// the schema just created already carries them and is left alone.
func rebuildNeeded(ctx context.Context, db *sql.DB) (bool, error) {
	bound, err := principalKeyColumns(ctx, conn(ctx, db))
	if err != nil {
		return false, err
	}

	for parent, column := range principalParents {
		if bound[parent] != column {
			return true, nil
		}
	}

	return false, nil
}

// tableDeclaration returns the current CREATE TABLE text under the rebuild name.
// Only the first occurrence is substituted: a self-referencing key, such as
// project_memberships' inviter, must keep naming the final table, which this one
// becomes after the rename.
func tableDeclaration(table, replacement string) (string, error) {
	statements, err := splitStatements(schema)
	if err != nil {
		return "", fmt.Errorf("pocketbase: read schema: %w", err)
	}

	opening := "CREATE TABLE IF NOT EXISTS " + table

	for _, statement := range statements {
		if strings.HasPrefix(statement, opening) {
			return strings.Replace(statement, table, replacement, 1), nil
		}
	}

	return "", fmt.Errorf("pocketbase: the schema declares no %s table", table)
}

// copyableColumns lists the columns the copy carries: the stored ones, excluding
// every generated column, which cannot be inserted into and is recomputed anyway.
func copyableColumns(ctx context.Context, db *sql.DB, table, declaration string) ([]string, error) {
	rows, err := conn(ctx, db).QueryContext(ctx, "PRAGMA table_xinfo("+table+")")
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string

	for rows.Next() {
		var (
			cid, notNull, primaryKey, hidden int
			name, kind                       string
			fallback                         sql.NullString
		)

		if err := rows.Scan(&cid, &name, &kind, &notNull, &fallback, &primaryKey, &hidden); err != nil {
			return nil, fmt.Errorf("pocketbase: scan %s columns: %w", table, err)
		}

		// hidden reports 3 for a stored generated column and 2 for a virtual one.
		if hidden != 0 {
			continue
		}

		// A column the current declaration does not name would be dropped by the
		// copy, which is data loss rather than a migration.
		if !strings.Contains(declaration, name) {
			return nil, fmt.Errorf("%w: %s.%s", ErrRebuildWouldDropColumn, table, name)
		}

		columns = append(columns, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read %s columns: %w", table, err)
	}

	if len(columns) == 0 {
		return nil, fmt.Errorf("pocketbase: %s declares no copyable column", table)
	}

	return columns, nil
}

// refuseRowsTheRebuildWouldReject names the rows a newly adopted constraint would
// reject, before anything is touched. The copy would otherwise abort naming no
// row at all, leaving an operator to find them by hand.
func refuseRowsTheRebuildWouldReject(ctx context.Context, db *sql.DB) error {
	dangling := map[string]string{
		"a client application no registry row describes": "SELECT COUNT(*) FROM project_memberships m" +
			" WHERE m.client_application_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM client_applications c" +
			" WHERE c.project_id = m.project_id AND c.id = m.client_application_id)",
		"a bot no registry row describes": "SELECT COUNT(*) FROM project_memberships m" +
			" WHERE m.bot_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM bots b" +
			" WHERE b.project_id = m.project_id AND b.id = m.bot_id)",
	}

	for subject, query := range dangling {
		if err := refuseNonEmpty(ctx, db, query, ErrDanglingPrincipal, subject); err != nil {
			return err
		}
	}

	rejected := map[string]string{
		"a client application id outside its namespace": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE client_application_id IS NOT NULL AND substr(client_application_id, 1, 4) <> 'cli_'",
		"a bot id outside its namespace": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE bot_id IS NOT NULL AND substr(bot_id, 1, 4) <> 'bot_'",
		"a user id inside a machine namespace": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE user_id IS NOT NULL AND substr(user_id, 1, 4) IN ('cli_', 'bot_')",
		"super admin on a machine principal": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE super_admin = 1 AND user_id IS NULL",
		"administrative standing on a bot": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE bot_id IS NOT NULL AND (admin = 1 OR super_admin = 1)",
		"a profile on a machine principal": "SELECT COUNT(*) FROM project_memberships" +
			" WHERE profile_id IS NOT NULL AND user_id IS NULL",
	}

	for subject, query := range rejected {
		if err := refuseNonEmpty(ctx, db, query, ErrRebuildWouldRejectRow, subject); err != nil {
			return err
		}
	}

	return nil
}

// refuseNonEmpty reports the named failure when the count query finds anything.
func refuseNonEmpty(ctx context.Context, db *sql.DB, query string, sentinel error, subject string) error {
	var found int
	if err := conn(ctx, db).QueryRowContext(ctx, query).Scan(&found); err != nil {
		return fmt.Errorf("pocketbase: check for %s before the rebuild: %w", subject, err)
	}

	if found > 0 {
		return fmt.Errorf("%w: %d rows carry %s", sentinel, found, subject)
	}

	return nil
}

// runRebuild performs the copy inside one transaction. Nothing is committed until
// the foreign key check has run over the result, so a database that cannot carry
// the constraint keeps the table it had.
func runRebuild(ctx context.Context, pinned *sql.Conn, plan rebuild) error {
	tx, err := pinned.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pocketbase: begin the rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	list := strings.Join(plan.columns, ", ")

	// A trigger on another table whose body names this one is re-validated by the
	// rename, and the table it names is gone by then. They are dropped here and
	// restored by the schema replay, which is their only definition.
	dependent, err := triggersNaming(ctx, tx, plan.table)
	if err != nil {
		return err
	}

	steps := append([]string{
		plan.declaration,
		"INSERT INTO " + plan.replacement + " (" + list + ") SELECT " + list + " FROM " + plan.table,
		"DROP TABLE " + plan.table,
		"ALTER TABLE " + plan.replacement + " RENAME TO " + plan.table,
	}, plan.finishing...)

	for _, trigger := range dependent {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
			return fmt.Errorf("pocketbase: drop trigger %s for the rebuild: %w", trigger, err)
		}
	}

	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step); err != nil {
			return fmt.Errorf("pocketbase: rebuild %q: %w", summarize(step), err)
		}
	}

	if err := assertNoViolations(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pocketbase: commit the rebuild: %w", err)
	}

	return nil
}

// triggersNaming lists the triggers that stand on another table but whose body
// names this one. They are found by what they say rather than by name, so a
// trigger added later is carried through the rebuild without editing it.
func triggersNaming(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	const query = "SELECT name FROM sqlite_master WHERE type = 'trigger'" +
		" AND tbl_name <> ? AND sql LIKE '%' || ? || '%'"

	rows, err := tx.QueryContext(ctx, query, table, table)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read triggers naming %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pocketbase: scan trigger naming %s: %w", table, err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read triggers naming %s: %w", table, err)
	}

	return names, nil
}

// assertNoViolations runs the foreign key check over the rebuilt table inside the
// transaction that can still undo it. The query itself errors on a schema
// mismatch, which is the most severe outcome and never a pass.
func assertNoViolations(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("pocketbase: the rebuilt %s does not satisfy its own keys: %w", membershipTable, err)
	}
	defer func() { _ = rows.Close() }()

	var violations []string

	for rows.Next() {
		var (
			table, parent   string
			rowID, keyIndex sql.NullInt64
		)

		if err := rows.Scan(&table, &rowID, &parent, &keyIndex); err != nil {
			return fmt.Errorf("pocketbase: scan foreign key violations: %w", err)
		}

		violations = append(violations, table+" -> "+parent)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("pocketbase: read foreign key violations: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf("%w: %s", ErrDanglingPrincipal, strings.Join(violations, ", "))
	}

	return nil
}
