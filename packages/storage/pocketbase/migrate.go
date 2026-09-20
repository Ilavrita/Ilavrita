package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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

	// ErrQueueClaimsMissing reports a notification queue with nowhere to record
	// which worker is working through it. Without that, two replicas fan out
	// the same write and post the same notification twice, so a database
	// without it is refused rather than served by one process and hoped about.
	ErrQueueClaimsMissing = errors.New(
		"pocketbase: a notification queue cannot record which worker claimed a row")

	// ErrRuleRestrictionColumnsMissing reports an access_policy_rules table with
	// nowhere to state a filter or a projection. A rule that cannot record its
	// restriction compiles to a Grant narrowed by nothing, which reaches further
	// than the policy says, so a database without the columns is refused.
	ErrRuleRestrictionColumnsMissing = errors.New(
		"pocketbase: access_policy_rules is missing its restriction columns")

	// ErrSessionLaunchMissing reports a sessions table with nowhere to record
	// what a SMART app was granted. A session that cannot say so reads as an
	// ordinary login, and an ordinary login is narrowed by nothing — so serving
	// such a database would hand every app the whole of what the person it acts
	// for may reach. It is refused.
	ErrSessionLaunchMissing = errors.New(
		"pocketbase: sessions cannot record what a SMART app was granted")

	// ErrClientKindMissing reports a client_applications table that cannot say
	// whether a registration keeps a secret. Without it the token endpoint has
	// no way to know which proof to demand, and the safe reading — demand a
	// secret from everything — would lock out every public app rather than
	// admitting one. A database that cannot answer is refused.
	ErrClientKindMissing = errors.New(
		"pocketbase: client_applications cannot say whether a registration keeps a secret")

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

	// sessionTable holds what a later request is served as, and what a SMART app
	// holding that session was granted.
	sessionTable = "sessions"

	// grantedScopesColumn is what a session records an app's grant in, and what
	// an install that predates SMART does not declare.
	grantedScopesColumn = "granted_scopes"

	// clientTable holds the registrations an authorization code is issued to.
	clientTable = "client_applications"

	// clientKindColumn is what a registration records its proof in, and what an
	// install that predates the OAuth endpoints does not declare.
	clientKindColumn = "kind"
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

	// The file is applied before anything is rebuilt, so every statement in it
	// has to be one an older database can run. An index over a column a rebuild
	// is about to add would fail here, before the rebuild that adds it — which
	// is why a new column's index keeps the name and shape the old one had, or
	// is left to the rebuild's own replay below.
	if err := ApplySchema(ctx, db); err != nil {
		return err
	}

	// Every step below is a super job: it decides for itself whether there is
	// anything to do, so running it twice does nothing the second time, and what
	// it did do is written down against the table it did it to. A start that
	// changed nothing writes nothing.
	at := time.Now().UTC()

	if !indexed {
		if _, err := Perform(ctx, db, at, SuperJob{
			Name: "backfill.fhir_search_index", Kind: JobBackfill, Subject: searchIndexTable,
		}, func(ctx context.Context) (Done, error) {
			if err := backfillSearchIndex(ctx, db); err != nil {
				return Done{}, err
			}

			return Done{
				Changed:     true,
				Fingerprint: "created",
				Detail:      "the index was built for an install that predates it",
			}, nil
		}); err != nil {
			return err
		}
	}

	rebuilt := false

	for _, migration := range schemaMigrations() {
		done, err := Perform(ctx, db, at, migration.job, migration.run(db))
		if err != nil {
			return err
		}

		rebuilt = rebuilt || done.Changed
	}

	// A rebuild drops its table, and that table's indexes and triggers go with
	// it. The file is the only definition of them, so it is replayed rather than
	// a second hand-written list kept in step with it.
	if rebuilt {
		if err := ApplySchema(ctx, db); err != nil {
			return err
		}
	}

	return assertServable(ctx, db)
}

// assertServable refuses a database missing a guarantee the declarations depend
// on. Every check here answers a shape a migration was supposed to produce, so
// reaching one of these errors means a rebuild ran and silently did nothing.
//
// They are a list rather than a run of statements in PrepareSchema so that a
// test can exercise the list itself. Inlined, each check was unreachable from
// outside — the migration that would make one fail is the same migration that
// runs immediately before it — and an assertion nothing can fail is one that can
// be deleted without any test noticing.
func assertServable(ctx context.Context, db *sql.DB) error {
	for _, assert := range []func(context.Context, *sql.DB) error{
		AssertMembershipPrincipalKeys,
		AssertRuleRestrictionColumns,
		AssertFactorReplacement,
		AssertQueueClaims,
		AssertSessionLaunch,
		AssertClientKind,
		AssertNoSystemClientApplicationDocuments,
	} {
		if err := assert(ctx, db); err != nil {
			return err
		}
	}

	return nil
}

// schemaMigration is one migration and the job it is recorded as.
type schemaMigration struct {
	job SuperJob

	// rebuild adopts a constraint onto a table that already exists, and reports
	// whether it had to. What makes it idempotent is its own inspection of the
	// database: a table that already carries the column is left alone.
	rebuild func(context.Context, *sql.DB) (bool, error)

	// applied is what the migration puts in place, recorded so the row says
	// which version of the work ran rather than only that something did.
	applied string
}

// run adapts a migration to the job the ledger performs.
func (m schemaMigration) run(db *sql.DB) func(context.Context) (Done, error) {
	return func(ctx context.Context) (Done, error) {
		changed, err := m.rebuild(ctx, db)
		if err != nil {
			return Done{}, err
		}

		return Done{
			Changed:     changed,
			Fingerprint: m.applied,
			Detail:      "the table was rebuilt to adopt " + m.applied,
		}, nil
	}
}

// schemaMigrations is every migration this build carries, in the order it runs
// them. Adding one here is what makes it run and what makes it recorded; there
// is no second list.
func schemaMigrations() []schemaMigration {
	return []schemaMigration{
		{
			job: SuperJob{
				Name: "migrate.project_memberships.principal_keys",
				Kind: JobMigration, Subject: membershipTable,
			},
			rebuild: rebuildMembershipPrincipalKeys,
			applied: "the client application and bot foreign keys",
		},
		{
			job: SuperJob{
				Name: "migrate.access_policy_rules.restrictions",
				Kind: JobMigration, Subject: "access_policy_rules",
			},
			rebuild: rebuildRuleRestrictions,
			applied: "the filter and projection columns",
		},
		{
			job: SuperJob{
				Name: "migrate.user_second_factors.replacement",
				Kind: JobMigration, Subject: factorTable,
			},
			rebuild: rebuildFactorReplacement,
			applied: "pending_secret, so a factor can be replaced without being switched off",
		},
		{
			job: SuperJob{
				Name: "migrate.fhir_search_index.string_value",
				Kind: JobMigration, Subject: stringValueTable,
			},
			rebuild: rebuildStringValues,
			applied: "the value as written beside the value folded, so :exact can read one",
		},
		{
			job: SuperJob{
				Name: "migrate.subscription_queues.claims",
				Kind: JobMigration, Subject: "subscription_backlog",
			},
			rebuild: rebuildQueueClaims,
			applied: "claimed_by and claimed_until on both queues",
		},
		{
			job: SuperJob{
				Name: "migrate.sessions.launch_context",
				Kind: JobMigration, Subject: sessionTable,
			},
			rebuild: rebuildSessionLaunch,
			applied: "launch_patient and granted_scopes, so a SMART app's session says what it may reach",
		},
		{
			job: SuperJob{
				Name: "migrate.client_applications.kind",
				Kind: JobMigration, Subject: clientTable,
			},
			rebuild: rebuildClientKind,
			applied: "kind, so a registration says which proof the token endpoint demands of it",
		},
	}
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

// rebuildSessionLaunch adopts the launch context columns onto a sessions table
// that predates SMART.
//
// Every row it carries across is an ordinary login, and takes NULL for both: a
// session issued before this server could record a grant was issued to somebody
// signing in, never to an app. So the copy has nothing the new checks reject,
// and no existing session is narrowed by the migration.
func rebuildSessionLaunch(ctx context.Context, db *sql.DB) (bool, error) {
	present, err := hasColumn(ctx, db, sessionTable, grantedScopesColumn)
	if err != nil || present {
		return false, err
	}

	// A table that is not there yet is created by the schema with the columns
	// already in it, and has nothing to carry across.
	declared, err := hasTable(ctx, db, sessionTable)
	if err != nil || !declared {
		return false, err
	}

	plan, err := planRebuild(ctx, db, sessionTable)
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	return true, nil
}

// rebuildClientKind adopts the kind column onto a client_applications table
// that predates the OAuth endpoints.
//
// Every registration it carries across was issued a client secret when it was
// created — that is the only way this server has ever registered one — so the
// column's default states what each of them already was, and the copy has
// nothing the new check rejects.
func rebuildClientKind(ctx context.Context, db *sql.DB) (bool, error) {
	present, err := hasColumn(ctx, db, clientTable, clientKindColumn)
	if err != nil || present {
		return false, err
	}

	// A table that is not there yet is created by the schema with the column
	// already in it, and has nothing to carry across.
	declared, err := hasTable(ctx, db, clientTable)
	if err != nil || !declared {
		return false, err
	}

	plan, err := planRebuild(ctx, db, clientTable)
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	return true, nil
}

// AssertClientKind refuses a database whose registrations cannot say whether
// they keep a secret. The token endpoint would have to guess which proof to
// demand, and every reading of that guess is wrong for half the clients.
func AssertClientKind(ctx context.Context, db *sql.DB) error {
	present, err := hasColumn(ctx, db, clientTable, clientKindColumn)
	if err != nil {
		return err
	}

	if !present {
		return fmt.Errorf("%w: %s.%s", ErrClientKindMissing, clientTable, clientKindColumn)
	}

	return nil
}

// AssertSessionLaunch refuses a database whose sessions cannot say what a SMART
// app was granted. Such a session reads as an ordinary login, and an ordinary
// login is narrowed by nothing, so every app would hold the whole of what the
// person it acts for may reach.
func AssertSessionLaunch(ctx context.Context, db *sql.DB) error {
	present, err := hasColumn(ctx, db, sessionTable, grantedScopesColumn)
	if err != nil {
		return err
	}

	if !present {
		return fmt.Errorf("%w: %s.%s", ErrSessionLaunchMissing, sessionTable, grantedScopesColumn)
	}

	return nil
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

// queueTables are the two a worker claims rows from, each carrying the same pair
// of claim columns.
var queueTables = []string{"subscription_backlog", "subscription_deliveries"}

// rebuildQueueClaims adopts the claim columns onto queues that predate them.
//
// A row that predates them is unclaimed, which is what a NULL claim means, so
// the copy has nothing the new checks reject.
func rebuildQueueClaims(ctx context.Context, db *sql.DB) (bool, error) {
	rebuilt := false

	for _, table := range queueTables {
		present, err := hasColumn(ctx, db, table, "claimed_by")
		if err != nil {
			return rebuilt, err
		}

		// A table that is not there yet is created by the schema with the
		// columns already in it, and has nothing to carry across.
		declared, err := hasTable(ctx, db, table)
		if err != nil {
			return rebuilt, err
		}

		if present || !declared {
			continue
		}

		plan, err := planRebuild(ctx, db, table)
		if err != nil {
			return rebuilt, err
		}

		if err := performRebuild(ctx, db, plan); err != nil {
			return rebuilt, err
		}

		rebuilt = true
	}

	return rebuilt, nil
}

// AssertQueueClaims refuses a database whose queues cannot record who is working
// through them. Without a claim, two replicas fan out the same write and post
// the same notification twice.
func AssertQueueClaims(ctx context.Context, db *sql.DB) error {
	for _, table := range queueTables {
		for _, column := range []string{"claimed_by", "claimed_until"} {
			present, err := hasColumn(ctx, db, table, column)
			if err != nil {
				return err
			}

			if !present {
				return fmt.Errorf("%w: %s.%s", ErrQueueClaimsMissing, table, column)
			}
		}
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

// stringValueTable is the index, whose string rows gained the value as written
// beside the value folded.
const stringValueTable = "fhir_search_index"

// newStringArm is what the constraint says once a string row carries the value
// as written. It is matched on the declaration itself because a CHECK is not a
// column: nothing in the catalogue reports one, and the text is the only thing
// that says.
//
// The new requirement is matched rather than the old one it replaced. The old
// text — "code IS NULL AND system IS NULL" — also appears in the arm for dates,
// which is true of every database including the ones already migrated: matching
// it would rebuild the index on every start, emptying and re-deriving the whole
// of it each time.
const newStringArm = "AND code IS NOT NULL AND code <> ''"

// rebuildStringValues widens the index so a string row keeps the value as it
// was written.
//
// A prefix match reads the folded value, because a search should not have to
// know how a name was capitalised. `:exact` is about exactly that, so it reads
// the value as written — and the old constraint said a string row had none.
//
// The rows are dropped rather than carried across. The index is derived from
// content the rows already hold, so it can be rebuilt; carrying it would mean
// carrying rows the new constraint refuses, which is the state this is for.
func rebuildStringValues(ctx context.Context, db *sql.DB) (bool, error) {
	declared, err := hasTable(ctx, db, stringValueTable)
	if err != nil || !declared {
		return false, err
	}

	current, err := declarationContains(ctx, db, stringValueTable, newStringArm)
	if err != nil || current {
		return false, err
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM "+stringValueTable); err != nil {
		return false, fmt.Errorf("pocketbase: empty the search index: %w", err)
	}

	plan, err := planRebuild(ctx, db, stringValueTable)
	if err != nil {
		return false, err
	}

	if err := performRebuild(ctx, db, plan); err != nil {
		return false, err
	}

	// Emptied above, so every searchable value has to be derived again.
	if err := backfillSearchIndex(ctx, db); err != nil {
		return false, err
	}

	return true, nil
}

// declarationContains reports whether a table's own declaration holds a
// fragment, which is how a constraint is recognised: the catalogue lists
// columns and indexes and says nothing about a CHECK.
func declarationContains(
	ctx context.Context, db *sql.DB, table, fragment string,
) (bool, error) {
	var declaration string

	switch err := db.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?",
		table).Scan(&declaration); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocketbase: read the declaration of %s: %w", table, err)
	}

	return strings.Contains(declaration, fragment), nil
}
