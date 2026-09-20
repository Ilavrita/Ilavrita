package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// storeKind is the Kind this store authorizes against. FHIR and platform rows
// live in physically separate tables, so a store is bound to one family and a
// Grant for the other can never satisfy a check here.
const storeKind = storage.KindFHIR

// ErrScopeEscape reports that a returned row fell outside the Scope it was read
// under. It means a predicate was dropped, so it fails the request rather than
// filtering the row away.
var ErrScopeEscape = errors.New("pocketbase: row escaped the scope it was read under")

// Columns every compiled arm selects, in the order scanRecord reads them. The
// history arms carry version_seq so the wrapper can order by it.
const (
	currentColumns = "r.project_id, r.res_type, r.res_id, r.version_id, r.last_updated, r.deleted, r.content"
	historyColumns = "h.project_id AS project_id, h.res_type AS res_type, h.res_id AS res_id," +
		" h.version_id AS version_id, h.last_updated AS last_updated, h.deleted AS deleted," +
		" h.content AS content, h.version_seq AS version_seq"
	recordColumns = "project_id, res_type, res_id, version_id, last_updated, deleted, content"
)

// The relations a compiled arm may read. Each names its own tenant column, which
// is what lets every arm bind its Project as a literal.
const (
	currentRelation = "fhir_resource r"
	historyRelation = "fhir_resource_history h"

	currentKeyPredicate = "r.project_id = ? AND r.res_type = ? AND r.res_id = ?"
	historyKeyPredicate = "h.project_id = ? AND h.res_type = ? AND h.res_id = ?"

	// Binds the history row to the logical id's current epoch, so a recreated id
	// never serves the previous owner's versions.
	currentEpochPredicate = "h.identity_epoch = (SELECT e.identity_epoch FROM fhir_resource e" +
		" WHERE e.project_id = ? AND e.res_type = ? AND e.res_id = ?)"

	compartmentPredicate = "EXISTS (SELECT 1 FROM fhir_resource_compartment c" +
		" WHERE c.project_id = ? AND c.comp_type = ? AND c.comp_id = ?" +
		" AND c.res_type = ? AND c.res_id = ?)"

	// Version-dimensioned, so a Compartment is checked against the version being
	// returned rather than against whatever the current row now says.
	historyCompartmentPredicate = "EXISTS (SELECT 1 FROM fhir_resource_history_compartment hc" +
		" WHERE hc.project_id = ? AND hc.comp_type = ? AND hc.comp_id = ?" +
		" AND hc.res_type = ? AND hc.res_id = ? AND hc.version_seq = h.version_seq)"
)

// The pieces a scoped write is assembled from. Each fragment ends at AND, so
// the Scope's own predicate is the last thing the WHERE clause carries.
const (
	writtenKey = " WHERE project_id = ? AND res_type = ? AND res_id = ? AND "

	expectedVersion = "version_id = ? AND "
	liveRow         = "deleted = 0 AND "
	buriedRow       = "deleted = 1 AND "

	updateSet = "UPDATE fhir_resource SET" +
		" version_seq = version_seq + 1, version_id = CAST(version_seq + 1 AS TEXT)," +
		" last_updated = ?, content = ?"

	deleteSet = "UPDATE fhir_resource SET" +
		" version_seq = version_seq + 1, version_id = CAST(version_seq + 1 AS TEXT)," +
		" last_updated = ?, deleted = 1, content = NULL"

	recreateSet = "UPDATE fhir_resource SET" +
		" version_seq = version_seq + 1, version_id = CAST(version_seq + 1 AS TEXT)," +
		" identity_epoch = identity_epoch + 1, last_updated = ?, deleted = 0, content = ?"
)

// setClauses are the two forms one scoped write takes: expecting the version
// the caller named, or expecting none.
type setClauses struct {
	expecting     string
	unconditional string
}

// forExpectation picks the clause for what the caller claimed. Naming no
// version is last-write-wins, never a race the caller has to resolve itself.
func (c setClauses) forExpectation(expect storage.VersionID) string {
	if expect == "" {
		return c.unconditional
	}

	return c.expecting
}

var (
	recreatePrefix = recreateSet + writtenKey + buriedRow

	updateClauses = setClauses{
		expecting:     updateSet + writtenKey + expectedVersion + liveRow,
		unconditional: updateSet + writtenKey + liveRow,
	}

	deleteClauses = setClauses{
		expecting:     deleteSet + writtenKey + expectedVersion + liveRow,
		unconditional: deleteSet + writtenKey + liveRow,
	}
)

// ResourceStore implements the storage interfaces for FHIR resources on SQLite.
// Version ids and last-updated instants are assigned by the store; the matching
// fields on a record handed to a write are ignored.
type ResourceStore struct {
	// savepoints names each one uniquely within this process, so two entries
	// running at once cannot unwind each other's.
	savepoints atomic.Int64

	db *sql.DB
}

var (
	_ storage.ResourceRepository = (*ResourceStore)(nil)
	_ storage.VersionStore       = (*ResourceStore)(nil)
	_ storage.Transactor         = (*ResourceStore)(nil)
)

// NewResourceStore binds a store to an open database. The caller owns the pool
// and is responsible for opening it with foreign keys enforced.
func NewResourceStore(db *sql.DB) *ResourceStore {
	return &ResourceStore{db: db}
}

// arm is one Grant compiled to SQL. Every relation it reads binds the Grant's
// Project as a literal, so a compiled arm holds one bound Project per table and
// no predicate can be relative to another row's Project.
type arm struct {
	conditions []string
	args       []any
}

// relation adds a table the arm reads. The Project is always the first bound
// argument, which is why a relation cannot be added without one.
func (a *arm) relation(project storage.ProjectID, predicate string, rest ...any) {
	a.conditions = append(a.conditions, predicate)
	a.args = append(a.args, string(project))
	a.args = append(a.args, rest...)
}

// filter adds a predicate over a relation the arm already reads.
func (a *arm) filter(predicate string, args ...any) {
	a.conditions = append(a.conditions, predicate)
	a.args = append(a.args, args...)
}

func (a *arm) query(columns, relation string) (string, []any) {
	return "SELECT " + columns + " FROM " + relation + " WHERE " + strings.Join(a.conditions, " AND "), a.args
}

// union compiles the arms into one statement. No arm compiles to a constantly
// false query, which is what an empty Scope must produce: a query matching no
// rows rather than an unpredicated one.
func union(columns, relation string, arms []arm) (string, []any) {
	if len(arms) == 0 {
		return "SELECT " + columns + " FROM " + relation + " WHERE 1 = 0", nil
	}

	texts := make([]string, 0, len(arms))

	var args []any

	for index := range arms {
		text, armArgs := arms[index].query(columns, relation)
		texts = append(texts, text)
		args = append(args, armArgs...)
	}

	return strings.Join(texts, " UNION "), args
}

// authorizedGrants keeps the Grants that allow this exact operation on this key.
// The candidate Projects come from Scope.Projects(), so a Scope that reaches no
// Project yields no Grant and therefore no arm.
func authorizedGrants(scope storage.Scope, key storage.ResourceKey, action storage.Action) []storage.Grant {
	projects := scope.Projects()
	kept := make([]storage.Grant, 0, len(projects))

	for _, grant := range scope.Grants() {
		if !slices.Contains(projects, grant.Project) || grant.Project != key.Project {
			continue
		}

		if !scope.Allows(grant.Project, storeKind, key.Type, action) {
			continue
		}

		if grant.Kind != storeKind || grant.Type != key.Type || grant.Action != action {
			continue
		}

		kept = append(kept, grant)
	}

	return kept
}

// covers reports whether these Grants authorize a resource landing in exactly
// these compartments. An unconfined Grant covers anything.
//
// A confined one is read one dimension at a time. A Grant confined to Patient/x
// speaks to the Patient dimension and to nothing else: every Patient compartment
// the resource lands in must be x, and a Practitioner or Encounter it also names
// is incidental. Without that, a clinician holding one patient could not record
// an Observation that named the encounter it happened in — which is nearly all
// clinical data.
//
// The resource must still land somewhere the Grants name, or it is not theirs
// to write: a subject-less resource reaches no compartment its author could read
// back, and a confined caller must not be able to write one.
func covers(grants []storage.Grant, key storage.ResourceKey, compartments []storage.Compartment) bool {
	if reachesEveryCompartment(grants) {
		return true
	}

	self := storage.Compartment{Type: key.Type, ID: key.ID}

	for _, compartment := range compartments {
		// The compartment a resource is itself is one it creates rather than
		// reaches into, so a Grant is not required to already name it.
		if compartment == self {
			continue
		}

		if speaksTo(grants, compartment.Type) && !named(grants, compartment) {
			return false
		}
	}

	return slices.ContainsFunc(compartments, func(compartment storage.Compartment) bool {
		return named(grants, compartment)
	})
}

// speaksTo reports whether any Grant is confined to a compartment of this type.
// A type no Grant names is one the caller's confinement says nothing about.
func speaksTo(grants []storage.Grant, subject storage.ResourceType) bool {
	return slices.ContainsFunc(grants, func(grant storage.Grant) bool {
		return grant.Compartment != nil && grant.Compartment.Type == subject
	})
}

// named reports whether any Grant is confined to this exact compartment.
func named(grants []storage.Grant, compartment storage.Compartment) bool {
	return slices.ContainsFunc(grants, func(grant storage.Grant) bool {
		return grant.Compartment != nil && *grant.Compartment == compartment
	})
}

// reachesEveryCompartment reports whether any Grant here is unconfined. No Grant
// at all is confinement to nothing, which is why the empty set answers false.
func reachesEveryCompartment(grants []storage.Grant) bool {
	return slices.ContainsFunc(grants, func(grant storage.Grant) bool { return grant.Compartment == nil })
}

// reachesEveryResource reports whether any Grant here is narrowed by nothing at
// all. A Grant narrowed in either dimension leaves resources of its own type
// unreadable, and a caller who cannot read a row must not be able to learn from
// a create that its id is taken.
func reachesEveryResource(grants []storage.Grant) bool {
	return slices.ContainsFunc(grants, func(grant storage.Grant) bool {
		return grant.Compartment == nil && grant.Filter == nil
	})
}

// currentArm compiles one Grant against the current-state table.
func currentArm(grant storage.Grant, key storage.ResourceKey) (arm, error) {
	var compiled arm

	compiled.relation(grant.Project, currentKeyPredicate, string(key.Type), string(key.ID))

	if grant.Compartment != nil {
		compiled.relation(grant.Project, compartmentPredicate,
			string(grant.Compartment.Type), string(grant.Compartment.ID),
			string(key.Type), string(key.ID))
	}

	predicate, args, err := filterPredicate("r.content", grant.Filter)
	if err != nil {
		return arm{}, err
	}

	if predicate != "" {
		compiled.filter(predicate, args...)
	}

	return compiled, nil
}

// historyArm compiles one Grant against the history table. The Compartment and
// the Filter are both checked against the version being returned, never against
// whatever the current row now says: a resource that has since been amended out
// of a Grant's reach must not hand over the versions it was once inside.
func historyArm(grant storage.Grant, key storage.ResourceKey) (arm, error) {
	var compiled arm

	compiled.relation(grant.Project, historyKeyPredicate, string(key.Type), string(key.ID))
	compiled.relation(grant.Project, currentEpochPredicate, string(key.Type), string(key.ID))

	if grant.Compartment != nil {
		compiled.relation(grant.Project, historyCompartmentPredicate,
			string(grant.Compartment.Type), string(grant.Compartment.ID),
			string(key.Type), string(key.ID))
	}

	predicate, args, err := filterPredicate("h.content", grant.Filter)
	if err != nil {
		return arm{}, err
	}

	if predicate != "" {
		compiled.filter(predicate, args...)
	}

	return compiled, nil
}

func currentArms(scope storage.Scope, key storage.ResourceKey, action storage.Action) ([]arm, error) {
	return compileArms(authorizedGrants(scope, key, action), key, currentArm)
}

func historyArms(scope storage.Scope, key storage.ResourceKey, action storage.Action) ([]arm, error) {
	return compileArms(authorizedGrants(scope, key, action), key, historyArm)
}

// compileArms compiles every Grant or none. One Grant this backend cannot
// compile fails the request: keeping the arms it could compile would answer
// under a Scope narrower than the caller holds, and dropping the failing arm's
// own restriction would answer under a wider one.
func compileArms(
	grants []storage.Grant,
	key storage.ResourceKey,
	compile func(storage.Grant, storage.ResourceKey) (arm, error),
) ([]arm, error) {
	arms := make([]arm, 0, len(grants))

	for _, grant := range grants {
		compiled, err := compile(grant, key)
		if err != nil {
			return nil, err
		}

		arms = append(arms, compiled)
	}

	return arms, nil
}

// currentStatement compiles a by-key read of the current row.
func currentStatement(
	scope storage.Scope, key storage.ResourceKey, action storage.Action,
) (string, []any, error) {
	arms, err := currentArms(scope, key, action)
	if err != nil {
		return "", nil, err
	}

	text, args := union(currentColumns, currentRelation, arms)

	return text + " LIMIT 1", args, nil
}

// versionStatement compiles a read of one named version.
func versionStatement(
	scope storage.Scope, key storage.ResourceKey, version storage.VersionID,
) (string, []any, error) {
	arms, err := historyArms(scope, key, storage.ActionHistory)
	if err != nil {
		return "", nil, err
	}

	for index := range arms {
		arms[index].filter("h.version_id = ?", string(version))
	}

	text, args := union(historyColumns, historyRelation, arms)

	return "SELECT " + recordColumns + " FROM (" + text + ") LIMIT 1", args, nil
}

// versionsStatement compiles a read of every visible version, newest first.
func versionsStatement(
	scope storage.Scope, key storage.ResourceKey, window storage.VersionWindow,
) (string, []any, error) {
	arms, err := historyArms(scope, key, storage.ActionHistory)
	if err != nil {
		return "", nil, err
	}

	text, args := union(historyColumns, historyRelation, arms)

	statement := "SELECT " + recordColumns + " FROM (" + text + ")"

	// History reads newest first, so resuming from a version means reading below
	// its sequence. Every version this server mints is the decimal of that
	// sequence, which is what lets a client resume from one it was handed.
	if window.Before != "" {
		before, err := window.Before.Sequence()
		if err != nil {
			return "", nil, err
		}

		statement += " WHERE version_seq < ?"
		args = append(args, before)
	}

	// _since narrows to what changed after a moment. It is applied here rather
	// than after reading, so a caller asking for changes since yesterday does
	// not page through years of history to find them.
	if !window.Since.IsZero() {
		if window.Before == "" {
			statement += " WHERE"
		} else {
			statement += " AND"
		}

		statement += " last_updated > ?"
		args = append(args, window.Since.UTC().UnixMilli())
	}

	// One more than asked for, so a further page is known to exist without
	// counting the rest of a history that may be long.
	statement += " ORDER BY version_seq DESC LIMIT ?"
	args = append(args, window.Count+1)

	return statement, args, nil
}

// authorizedExists compiles the arm set into an EXISTS a write can require, so
// a write and a read of the same row share one authorization predicate.
func authorizedExists(
	scope storage.Scope, key storage.ResourceKey, action storage.Action,
) (string, []any, error) {
	arms, err := currentArms(scope, key, action)
	if err != nil {
		return "", nil, err
	}

	text, args := union("1", currentRelation, arms)

	return "EXISTS (" + text + ")", args, nil
}

// writeStatement assembles a scoped write: a SET clause, the key, and the same
// authorization predicate a read of that row would compile to.
func writeStatement(
	prefix string,
	scope storage.Scope,
	key storage.ResourceKey,
	action storage.Action,
) (string, []any, error) {
	exists, args, err := authorizedExists(scope, key, action)
	if err != nil {
		return "", nil, err
	}

	return prefix + exists + " RETURNING version_seq, identity_epoch", args, nil
}

// Read returns the current version of a resource. A row outside the Scope is
// reported as missing, so a caller cannot probe for ids it may not read.
func (s *ResourceStore) Read(ctx context.Context, scope storage.Scope, key storage.ResourceKey) (storage.ResourceRecord, error) {
	if err := validateKey(key); err != nil {
		return storage.ResourceRecord{}, err
	}

	record, err := s.readCurrent(ctx, scope, key, storage.ActionRead)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	return s.narrowed(ctx, scope, record, storage.ActionRead, s.currentPlacement)
}

func (s *ResourceStore) readCurrent(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	action storage.Action,
) (storage.ResourceRecord, error) {
	text, args, err := currentStatement(scope, key, action)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	record, deleted, err := scanRecord(s.conn(ctx).QueryRowContext(ctx, text, args...))
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := assertInScope(scope, record, action); err != nil {
		return storage.ResourceRecord{}, err
	}

	if deleted {
		return storage.ResourceRecord{}, storage.ErrDeleted
	}

	return record, nil
}

// ReadVersion returns one immutable version. It authorizes under ActionHistory,
// never ActionRead: a read Grant may have been minted against the current
// compartment, which an older version need not share.
func (s *ResourceStore) ReadVersion(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	version storage.VersionID,
) (storage.ResourceRecord, error) {
	if err := validateKey(key); err != nil {
		return storage.ResourceRecord{}, err
	}

	if version == "" {
		return storage.ResourceRecord{}, fmt.Errorf("pocketbase: read version needs a version id")
	}

	text, args, err := versionStatement(scope, key, version)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	record, deleted, err := scanRecord(s.conn(ctx).QueryRowContext(ctx, text, args...))
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := assertInScope(scope, record, storage.ActionHistory); err != nil {
		return storage.ResourceRecord{}, err
	}

	if deleted {
		return storage.ResourceRecord{}, storage.ErrDeleted
	}

	return s.narrowed(ctx, scope, record, storage.ActionHistory, s.versionPlacement)
}

// ListVersions returns every version the Scope authorizes, newest first. A
// resource with no visible version is reported as missing.
func (s *ResourceStore) ListVersions(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	window storage.VersionWindow,
) (storage.VersionPage, error) {
	if err := validateKey(key); err != nil {
		return storage.VersionPage{}, err
	}

	text, args, err := versionsStatement(scope, key, window)
	if err != nil {
		return storage.VersionPage{}, err
	}

	rows, err := s.conn(ctx).QueryContext(ctx, text, args...)
	if err != nil {
		return storage.VersionPage{}, fmt.Errorf("pocketbase: list versions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var records []storage.ResourceRecord

	for rows.Next() {
		record, _, err := scanRecord(rows)
		if err != nil {
			return storage.VersionPage{}, err
		}

		if err := assertInScope(scope, record, storage.ActionHistory); err != nil {
			return storage.VersionPage{}, err
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return storage.VersionPage{}, fmt.Errorf("pocketbase: list versions: %w", err)
	}

	// A resource always has at least one version, so an unnarrowed history with
	// no rows is a resource that is not there. A narrowed one with no rows is a
	// resource nothing happened to in the window asked about, which is an empty
	// history and not a missing resource.
	if len(records) == 0 && window.Since.IsZero() {
		return storage.VersionPage{}, storage.ErrNotFound
	}

	page := storage.VersionPage{First: window.Before == ""}

	// One more than the page was asked for is how a further page is known to
	// exist without counting the rest. The extra row is read and dropped.
	if len(records) > window.Count {
		records, page.More = records[:window.Count], true
	}

	// Narrowing reads each version's placement, which is a query of its own. The
	// cursor above is closed first: this pool holds one connection, and a query
	// issued while it is still open waits for a connection that only closing the
	// cursor can release.
	if err := rows.Close(); err != nil {
		return storage.VersionPage{}, fmt.Errorf("pocketbase: close the version cursor: %w", err)
	}

	for index, record := range records {
		narrowed, err := s.narrowed(ctx, scope, record, storage.ActionHistory, s.versionPlacement)
		if err != nil {
			return storage.VersionPage{}, err
		}

		records[index] = narrowed
	}

	page.Records = records

	return page, nil
}

// Create writes a resource that does not exist yet. It never falls through to an
// update: a taken id is a conflict, so a create can not overwrite a row the
// caller was never able to read.
func (s *ResourceStore) Create(ctx context.Context, scope storage.Scope, record storage.ResourceRecord) error {
	if err := validateWrite(record); err != nil {
		return err
	}

	// The insert carries no Scope predicate of its own, so a Grant confined to a
	// compartment could otherwise write outside it.
	if err := s.authorizePlacement(ctx, scope, record); err != nil {
		return err
	}

	// A caller must be able to read what it just created, or a create would be a
	// write into somewhere it cannot see. An update needs no equivalent: the row
	// is already there, so the read-back that renders it answers exactly rather
	// than predicting.
	reads, err := s.admittingGrants(ctx, authorizedGrants(scope, record.Key, storage.ActionRead), record.Content)
	if err != nil {
		return err
	}

	if !covers(reads, record.Key, record.Compartments) {
		return storage.ErrDenied
	}

	narrowed := !reachesEveryResource(reads)

	return s.WithinTransaction(ctx, func(ctx context.Context) error {
		err := s.create(ctx, scope, record)

		// An id taken by a resource this caller cannot reach is one a create
		// must not report: a narrowed caller cannot tell a taken id from one it
		// was never allowed to name.
		if narrowed && errors.Is(err, storage.ErrAlreadyExists) {
			return storage.ErrDenied
		}

		return err
	})
}

// authorizePlacement refuses a write whose resource would land outside the
// compartments the caller may write into. A create and an update ask it alike:
// a confined caller may no more move a resource to another patient than create
// it there.
//
// The record states where the resource lands, so this reads the content the
// caller submitted and never the projection a previous version left behind.
// A Grant whose filter that content fails takes no part in the decision, which
// is what stops a filtered caller writing a resource its own filter would then
// hide from it.
func (s *ResourceStore) authorizePlacement(
	ctx context.Context, scope storage.Scope, record storage.ResourceRecord,
) error {
	writes, err := s.admittingGrants(ctx, authorizedGrants(scope, record.Key, storage.ActionWrite), record.Content)
	if err != nil {
		return err
	}

	if !covers(writes, record.Key, record.Compartments) {
		return storage.ErrDenied
	}

	return nil
}

func (s *ResourceStore) create(ctx context.Context, scope storage.Scope, record storage.ResourceRecord) error {
	const insert = "INSERT INTO fhir_resource" +
		" (project_id, res_type, res_id, version_id, version_seq, identity_epoch, last_updated, deleted, content)" +
		" VALUES (?, ?, ?, '1', 1, 0, ?, 0, ?)" +
		" ON CONFLICT (project_id, res_type, res_id) DO NOTHING" +
		" RETURNING version_seq, identity_epoch"

	stamp := time.Now().UTC()
	key := record.Key

	var seq, epoch int64

	err := s.conn(ctx).QueryRowContext(ctx, insert,
		string(key.Project), string(key.Type), string(key.ID),
		stamp.UnixMilli(), string(record.Content),
	).Scan(&seq, &epoch)

	switch {
	case err == nil:
		if err := s.writeProjections(ctx, key, record, stamp); err != nil {
			return err
		}

		return s.writeVersion(ctx, key, seq, epoch, stamp, record.Content)
	case errors.Is(err, sql.ErrNoRows):
		return s.recreate(ctx, scope, record, stamp)
	default:
		return fmt.Errorf("pocketbase: create resource: %w", err)
	}
}

// writeProjections replaces everything derived from a resource's content: the
// compartments it lands in and the values it is searchable by. They are
// replaced together because they are derived from the same content at the same
// moment, and one maintained without the other is the decorative predicate this
// store has already been bitten by once.
func (s *ResourceStore) writeProjections(
	ctx context.Context, key storage.ResourceKey, record storage.ResourceRecord, at time.Time,
) error {
	if err := s.writeCompartments(ctx, key, record.Compartments); err != nil {
		return err
	}

	// What a SearchParameter defines is derived from the same content at the
	// same moment as everything else derived here, and has to be written before
	// the index is: this resource can define a parameter that indexes itself.
	if key.Type == searchParameterType {
		if err := s.writeSearchParameters(ctx, key, record.Content, at); err != nil {
			return err
		}
	}

	return s.writeSearchIndex(ctx, key, record.Content)
}

// writeCompartments replaces the row's compartment projection with the one its
// current content states. It runs before the version is written, so the history
// projection copies the compartments that version actually landed in.
func (s *ResourceStore) writeCompartments(
	ctx context.Context, key storage.ResourceKey, compartments []storage.Compartment,
) error {
	const clear = "DELETE FROM fhir_resource_compartment" +
		" WHERE project_id = ? AND res_type = ? AND res_id = ?"

	if _, err := s.conn(ctx).ExecContext(ctx, clear,
		string(key.Project), string(key.Type), string(key.ID)); err != nil {
		return fmt.Errorf("pocketbase: clear compartments: %w", err)
	}

	const insert = "INSERT INTO fhir_resource_compartment" +
		" (project_id, comp_type, comp_id, res_type, res_id) VALUES (?, ?, ?, ?, ?)" +
		" ON CONFLICT DO NOTHING"

	for _, compartment := range compartments {
		if _, err := s.conn(ctx).ExecContext(ctx, insert,
			string(key.Project), string(compartment.Type), string(compartment.ID),
			string(key.Type), string(key.ID)); err != nil {
			return fmt.Errorf("pocketbase: project compartment %s/%s: %w",
				compartment.Type, compartment.ID, err)
		}
	}

	return nil
}

// recreate takes over a logical id whose current row is a tombstone the caller
// is authorized to write. The epoch bump is what stops the new owner inheriting
// the previous owner's versions.
func (s *ResourceStore) recreate(
	ctx context.Context,
	scope storage.Scope,
	record storage.ResourceRecord,
	stamp time.Time,
) error {
	update, existsArgs, err := writeStatement(recreatePrefix, scope, record.Key, storage.ActionWrite)
	if err != nil {
		return err
	}

	key := record.Key
	args := append([]any{stamp.UnixMilli(), string(record.Content),
		string(key.Project), string(key.Type), string(key.ID)}, existsArgs...)

	var seq, epoch int64

	switch err := s.conn(ctx).QueryRowContext(ctx, update, args...).Scan(&seq, &epoch); {
	case errors.Is(err, sql.ErrNoRows):
		return storage.ErrAlreadyExists
	case err != nil:
		return fmt.Errorf("pocketbase: recreate resource: %w", err)
	}

	// The previous owner's projections must not describe the new resource; the
	// history rows they produced stay behind the old epoch.
	if err := s.writeProjections(ctx, key, record, stamp); err != nil {
		return err
	}
	return s.writeVersion(ctx, key, seq, epoch, stamp, record.Content)
}

// Update replaces the current version. One WHERE clause carries the expectation
// and the Scope, so a stale caller and an unauthorized one are refused by the
// same statement. An empty expectation replaces whatever the row holds.
func (s *ResourceStore) Update(
	ctx context.Context,
	scope storage.Scope,
	record storage.ResourceRecord,
	expect storage.VersionID,
) error {
	if err := validateWrite(record); err != nil {
		return err
	}

	if err := refuseBlindReplace(scope, record.Key); err != nil {
		return err
	}

	if err := s.authorizePlacement(ctx, scope, record); err != nil {
		return err
	}

	stamp := time.Now().UTC()

	return s.WithinTransaction(ctx, func(ctx context.Context) error {
		return s.mutate(ctx, scope, record.Key, mutation{
			action:       storage.ActionWrite,
			prefix:       updateClauses.forExpectation(expect),
			stamp:        stamp,
			leading:      []any{stamp.UnixMilli(), string(record.Content)},
			expect:       expect,
			content:      record.Content,
			compartments: record.Compartments,
		})
	})
}

// Delete soft-deletes a resource, leaving the row and its compartment
// projection in place so the tombstone stays attributable. An empty expectation
// buries whatever the row currently holds.
func (s *ResourceStore) Delete(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	expect storage.VersionID,
) error {
	if err := validateKey(key); err != nil {
		return err
	}

	stamp := time.Now().UTC()

	return s.WithinTransaction(ctx, func(ctx context.Context) error {
		return s.mutate(ctx, scope, key, mutation{
			action:  storage.ActionDelete,
			prefix:  deleteClauses.forExpectation(expect),
			stamp:   stamp,
			leading: []any{stamp.UnixMilli()},
			expect:  expect,
		})
	})
}

// mutation is what Update and Delete differ by: the action they authorize under,
// their SET clause and the body the new version carries. One stamp serves the
// row and its version, so the two can never disagree about when it was written.
type mutation struct {
	action       storage.Action
	prefix       string
	stamp        time.Time
	leading      []any
	expect       storage.VersionID
	content      []byte
	compartments []storage.Compartment
}

func (s *ResourceStore) mutate(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	change mutation,
) error {
	if len(authorizedGrants(scope, key, change.action)) == 0 {
		return storage.ErrDenied
	}

	statement, existsArgs, err := writeStatement(change.prefix, scope, key, change.action)
	if err != nil {
		return err
	}

	args := append([]any{}, change.leading...)
	args = append(args, string(key.Project), string(key.Type), string(key.ID))

	if change.expect != "" {
		args = append(args, string(change.expect))
	}

	args = append(args, existsArgs...)

	var seq, epoch int64

	switch err := s.conn(ctx).QueryRowContext(ctx, statement, args...).Scan(&seq, &epoch); {
	case errors.Is(err, sql.ErrNoRows):
		return s.explainMiss(ctx, scope, key, change.action)
	case err != nil:
		return fmt.Errorf("pocketbase: mutate resource: %w", err)
	}

	// A write states where the resource now lands, so the projection is replaced
	// before the version is appended and the history copy records where this
	// version landed. A delete states nothing: its tombstone keeps the placement
	// it had, so the row stays attributable to whoever could reach it.
	switch {
	case change.action == storage.ActionWrite:
		if err := s.writeProjections(ctx, key, storage.ResourceRecord{
			Key: key, Content: change.content, Compartments: change.compartments,
		}, change.stamp); err != nil {
			return err
		}

	case key.Type == searchParameterType:
		// A tombstone keeps its placement, which is what the branch above is
		// about. A definition is not a placement: it is a claim about what
		// searches answer, and a deleted resource claims nothing. Left behind,
		// it would keep a code accepted and keep matching whatever it indexed.
		if err := s.writeSearchParameters(ctx, key, nil, change.stamp); err != nil {
			return err
		}
	}

	return s.writeVersion(ctx, key, seq, epoch, change.stamp, change.content)
}

// explainMiss says why a scoped write matched nothing, re-reading inside the
// same transaction under the same Scope. An unauthorized row reads as missing,
// which is the same answer an absent one gives.
func (s *ResourceStore) explainMiss(
	ctx context.Context,
	scope storage.Scope,
	key storage.ResourceKey,
	action storage.Action,
) error {
	if _, err := s.readCurrent(ctx, scope, key, action); err != nil {
		return err
	}

	return storage.ErrVersionConflict
}

// writeVersion appends the immutable version row and projects the resource's
// compartments onto it, so a later history read has a per-version fact to check.
func (s *ResourceStore) writeVersion(
	ctx context.Context,
	key storage.ResourceKey,
	seq, epoch int64,
	stamp time.Time,
	content []byte,
) error {
	const insert = "INSERT INTO fhir_resource_history" +
		" (project_id, res_type, res_id, version_seq, version_id, identity_epoch," +
		" last_updated, deleted, content) VALUES (?, ?, ?, ?, CAST(? AS TEXT), ?, ?, ?, ?)"

	deleted := 0
	body := any(string(content))

	if content == nil {
		deleted = 1
		body = nil
	}

	if _, err := s.conn(ctx).ExecContext(ctx, insert,
		string(key.Project), string(key.Type), string(key.ID), seq, seq, epoch,
		stamp.UnixMilli(), deleted, body,
	); err != nil {
		return fmt.Errorf("pocketbase: append version: %w", err)
	}

	return s.projectCompartments(ctx, key, seq)
}

func (s *ResourceStore) projectCompartments(ctx context.Context, key storage.ResourceKey, seq int64) error {
	const project = "INSERT INTO fhir_resource_history_compartment" +
		" (project_id, comp_type, comp_id, res_type, res_id, version_seq)" +
		" SELECT project_id, comp_type, comp_id, res_type, res_id, ?" +
		" FROM fhir_resource_compartment" +
		" WHERE project_id = ? AND res_type = ? AND res_id = ?"

	if _, err := s.conn(ctx).ExecContext(ctx, project,
		seq, string(key.Project), string(key.Type), string(key.ID)); err != nil {
		return fmt.Errorf("pocketbase: project compartments: %w", err)
	}

	return nil
}

// WithinTransaction runs work inside one commit boundary. A nested call joins
// the boundary already open rather than starting a second one.
func (s *ResourceStore) WithinTransaction(ctx context.Context, work func(ctx context.Context) error) error {
	if _, open := ctx.Value(transactionKey{}).(*sql.Tx); open {
		return work(ctx)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pocketbase: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := work(context.WithValue(ctx, transactionKey{}, tx)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pocketbase: commit transaction: %w", err)
	}

	return nil
}

// transactionKey carries the open transaction down to the statements that must
// commit with it.
type transactionKey struct{}

// executor is the part of *sql.DB and *sql.Tx the statements here need.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *ResourceStore) conn(ctx context.Context) executor {
	if tx, open := ctx.Value(transactionKey{}).(*sql.Tx); open {
		return tx
	}

	return s.db
}

// row is what a compiled statement returns, satisfied by *sql.Row and *sql.Rows.
type row interface {
	Scan(dest ...any) error
}

func scanRecord(src row) (storage.ResourceRecord, bool, error) {
	var (
		project, resType, resID, version string
		lastUpdated                      int64
		deleted                          int
		content                          []byte
	)

	switch err := src.Scan(&project, &resType, &resID, &version, &lastUpdated, &deleted, &content); {
	case errors.Is(err, sql.ErrNoRows):
		return storage.ResourceRecord{}, false, storage.ErrNotFound
	case err != nil:
		return storage.ResourceRecord{}, false, fmt.Errorf("pocketbase: scan resource: %w", err)
	}

	return storage.ResourceRecord{
		Key: storage.ResourceKey{
			Project: storage.ProjectID(project),
			Type:    storage.ResourceType(resType),
			ID:      storage.LogicalID(resID),
		},
		Version:     storage.VersionID(version),
		LastUpdated: time.UnixMilli(lastUpdated).UTC(),
		Deleted:     deleted == 1,
		Content:     content,
	}, deleted == 1, nil
}

// assertInScope re-checks a returned row's Project, type and action against the
// Scope it was read under. It does not re-check the Grant's Compartment, so a
// dropped compartment predicate reaches the caller uncaught (AUTH-3).
func assertInScope(scope storage.Scope, record storage.ResourceRecord, action storage.Action) error {
	if scope.Allows(record.Key.Project, storeKind, record.Key.Type, action) {
		return nil
	}

	return fmt.Errorf("%w: %s/%s in %s", ErrScopeEscape, record.Key.Type, record.Key.ID, record.Key.Project)
}

func validateKey(key storage.ResourceKey) error {
	if _, err := storage.NewResourceKey(key.Project, key.Type, key.ID); err != nil {
		return fmt.Errorf("pocketbase: %w", err)
	}

	return nil
}

func validateWrite(record storage.ResourceRecord) error {
	if err := validateKey(record.Key); err != nil {
		return err
	}

	if len(record.Content) == 0 {
		return fmt.Errorf("pocketbase: a written resource needs content")
	}

	return nil
}

// WithinSavepoint runs work so that its failure undoes only what it wrote.
//
// A batch is several interactions that succeed or fail on their own, inside one
// request that already holds a transaction. Rolling the whole of it back for
// one bad entry would be a transaction, which is the other thing entirely; not
// rolling anything back would leave half an entry behind.
//
// Outside a transaction it is one, because a savepoint with nothing to nest in
// is a transaction by another name.
func (s *ResourceStore) WithinSavepoint(
	ctx context.Context, work func(ctx context.Context) error,
) error {
	tx, open := ctx.Value(transactionKey{}).(*sql.Tx)
	if !open {
		return s.WithinTransaction(ctx, work)
	}

	// Named by this store rather than by a caller: a savepoint name goes into
	// the statement verbatim, and one a request could choose is one a request
	// could write.
	name := fmt.Sprintf("entry_%d", s.savepoints.Add(1))

	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("pocketbase: open a savepoint: %w", err)
	}

	if err := work(ctx); err != nil {
		if _, undone := tx.ExecContext(ctx, "ROLLBACK TO "+name); undone != nil {
			// The savepoint could not be unwound, so what this entry wrote is
			// still there and nothing below can be trusted to be what it says.
			return errors.Join(err, fmt.Errorf("pocketbase: undo an entry: %w", undone))
		}

		if _, err := tx.ExecContext(ctx, "RELEASE "+name); err != nil {
			return fmt.Errorf("pocketbase: release an undone savepoint: %w", err)
		}

		return err
	}

	if _, err := tx.ExecContext(ctx, "RELEASE "+name); err != nil {
		return fmt.Errorf("pocketbase: release a savepoint: %w", err)
	}

	return nil
}
