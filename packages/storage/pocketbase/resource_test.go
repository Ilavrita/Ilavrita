package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"

	_ "modernc.org/sqlite"
)

const patientBody = `{"resourceType":"Patient"}`

func fhirGrant(project storage.ProjectID, resourceType storage.ResourceType, action storage.Action) storage.Grant {
	return storage.Grant{
		Project: project, Kind: storage.KindFHIR, Type: resourceType,
		Action: action, Source: storage.SourceMembership,
	}
}

func compartmentGrant(
	project storage.ProjectID,
	resourceType storage.ResourceType,
	action storage.Action,
	compartment storage.Compartment,
) storage.Grant {
	grant := fhirGrant(project, resourceType, action)
	grant.Compartment = &compartment

	return grant
}

// fullScope holds every action on one type in one Project, which is the widest
// Scope any test here uses.
func fullScope(project storage.ProjectID, resourceType storage.ResourceType) storage.Scope {
	return storage.NewScope(
		fhirGrant(project, resourceType, storage.ActionRead),
		fhirGrant(project, resourceType, storage.ActionWrite),
		fhirGrant(project, resourceType, storage.ActionDelete),
		fhirGrant(project, resourceType, storage.ActionHistory),
	)
}

func patientKey(project storage.ProjectID, id storage.LogicalID) storage.ResourceKey {
	return storage.ResourceKey{Project: project, Type: "Patient", ID: id}
}

func patientRecord(key storage.ResourceKey, body string) storage.ResourceRecord {
	return storage.ResourceRecord{Key: key, Content: []byte(body)}
}

// newStore opens a real SQLite database in a temporary directory, applies the
// schema and asserts foreign keys are enforced on it.
func newStore(t *testing.T) (*ResourceStore, *sql.DB) {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "ilavrita.db") + "?_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	var enforced int
	if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatalf("read foreign key pragma: %v", err)
	}

	if enforced != 1 {
		t.Fatal("foreign keys are not enforced, so every composite key guarantee is a convention")
	}

	for _, id := range []string{"prj_a", "prj_b"} {
		_, err := db.ExecContext(t.Context(),
			"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)"+
				" VALUES (?, 'standard', ?, ?, 'active', 0, 0, 0)", id, id, id)
		if err != nil {
			t.Fatalf("seed project %s: %v", id, err)
		}
	}

	return NewResourceStore(db), db
}

func seed(t *testing.T, store *ResourceStore, project storage.ProjectID, id storage.LogicalID) storage.ResourceKey {
	t.Helper()

	key := patientKey(project, id)
	if err := store.Create(t.Context(), fullScope(project, "Patient"), patientRecord(key, patientBody)); err != nil {
		t.Fatalf("seed %s/%s: %v", project, id, err)
	}

	return key
}

// attachCompartment writes the projection the search-index layer owns, so a
// compartment-restricted Grant has something to match against.
func attachCompartment(t *testing.T, db *sql.DB, key storage.ResourceKey, compartment storage.Compartment) {
	t.Helper()

	_, err := db.ExecContext(t.Context(),
		"INSERT INTO fhir_resource_compartment (project_id, comp_type, comp_id, res_type, res_id)"+
			" VALUES (?, ?, ?, ?, ?)",
		string(key.Project), string(compartment.Type), string(compartment.ID),
		string(key.Type), string(key.ID))
	if err != nil {
		t.Fatalf("attach compartment: %v", err)
	}
}

func countRows(t *testing.T, db *sql.DB, text string, args []any) int {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), text, args...)
	if err != nil {
		t.Fatalf("run %q: %v", text, err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		count++
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}

	return count
}

// ---------------------------------------------------------------------------
// Cross-project isolation
// ---------------------------------------------------------------------------

func TestReadDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_b", "shared")

	if _, err := store.Read(t.Context(), fullScope("prj_a", "Patient"), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a project A Scope read a project B resource: err = %v", err)
	}
}

func TestReadIgnoresAKeyNamingAProjectTheScopeDoesNotHold(t *testing.T) {
	store, _ := newStore(t)
	seed(t, store, "prj_a", "shared")
	seed(t, store, "prj_b", "shared")

	record, err := store.Read(t.Context(), fullScope("prj_a", "Patient"), patientKey("prj_a", "shared"))
	if err != nil {
		t.Fatalf("read own resource: %v", err)
	}

	if record.Key.Project != "prj_a" {
		t.Fatalf("read returned a row from %s; naming a Project in a key is not entitlement", record.Key.Project)
	}
}

func TestUpdateDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_b", "shared")

	err := store.Update(t.Context(), fullScope("prj_a", "Patient"), patientRecord(key, `{"tampered":true}`), "1")
	if !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("a project A Scope was allowed to update a project B resource: err = %v", err)
	}

	record, err := store.Read(t.Context(), fullScope("prj_b", "Patient"), key)
	if err != nil {
		t.Fatalf("read project B resource: %v", err)
	}

	if record.Version != "1" || string(record.Content) != patientBody {
		t.Fatalf("project B's resource changed: version %q content %q", record.Version, record.Content)
	}
}

func TestDeleteDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_b", "shared")

	if err := store.Delete(t.Context(), fullScope("prj_a", "Patient"), key, "1"); !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("a project A Scope was allowed to delete a project B resource: err = %v", err)
	}

	if _, err := store.Read(t.Context(), fullScope("prj_b", "Patient"), key); err != nil {
		t.Fatalf("project B's resource was deleted through another project's Scope: %v", err)
	}
}

func TestListVersionsDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_b", "shared")

	_, err := store.ListVersions(t.Context(), fullScope("prj_a", "Patient"), key)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a project A Scope listed a project B resource's history: err = %v", err)
	}
}

func TestReadVersionDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_b", "shared")

	_, err := store.ReadVersion(t.Context(), fullScope("prj_a", "Patient"), key, "1")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a project A Scope read a project B version: err = %v", err)
	}
}

// The tests above are refused before a statement runs, because a key naming
// another Project matches no Grant. These name the caller's own Project, so only
// the bound project literal in the compiled SQL can keep the other row out.

func TestReadUnderAnOwnProjectKeyNeverReturnsAnotherProjectsRow(t *testing.T) {
	store, _ := newStore(t)
	seed(t, store, "prj_b", "shared")

	_, err := store.Read(t.Context(), fullScope("prj_a", "Patient"), patientKey("prj_a", "shared"))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a project A key reached project B's row: err = %v", err)
	}
}

func TestHistoryUnderAnOwnProjectKeyNeverReturnsAnotherProjectsRow(t *testing.T) {
	store, _ := newStore(t)
	seed(t, store, "prj_b", "shared")

	scope := fullScope("prj_a", "Patient")

	if _, err := store.ListVersions(t.Context(), scope, patientKey("prj_a", "shared")); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a project A key listed project B's history: err = %v", err)
	}

	if _, err := store.ReadVersion(t.Context(), scope, patientKey("prj_a", "shared"), "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a project A key read a project B version: err = %v", err)
	}
}

func TestWritesUnderAnOwnProjectKeyNeverTouchAnotherProjectsRow(t *testing.T) {
	store, _ := newStore(t)
	victim := seed(t, store, "prj_b", "shared")

	scope := fullScope("prj_a", "Patient")
	attacker := patientKey("prj_a", "shared")

	if err := store.Update(t.Context(), scope, patientRecord(attacker, `{"tampered":true}`), "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("update err = %v, want ErrNotFound", err)
	}

	if err := store.Delete(t.Context(), scope, attacker, "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("delete err = %v, want ErrNotFound", err)
	}

	record, err := store.Read(t.Context(), fullScope("prj_b", "Patient"), victim)
	if err != nil {
		t.Fatalf("read project B resource: %v", err)
	}

	if record.Version != "1" || string(record.Content) != patientBody {
		t.Fatalf("project B's resource changed: version %q content %q", record.Version, record.Content)
	}
}

func TestCreateDeniesAnotherProjectsResource(t *testing.T) {
	store, _ := newStore(t)

	key := patientKey("prj_b", "planted")
	err := store.Create(t.Context(), fullScope("prj_a", "Patient"), patientRecord(key, patientBody))

	if !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("a project A Scope wrote into project B: err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// The zero Scope
// ---------------------------------------------------------------------------

func TestZeroScopeReadsNothing(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	var empty storage.Scope

	if _, err := store.Read(t.Context(), empty, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the zero Scope read a resource: err = %v", err)
	}
}

func TestZeroScopeReadsNoHistory(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	var empty storage.Scope

	if _, err := store.ListVersions(t.Context(), empty, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the zero Scope listed history: err = %v", err)
	}

	if _, err := store.ReadVersion(t.Context(), empty, key, "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the zero Scope read a version: err = %v", err)
	}
}

func TestZeroScopeWritesNothing(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	var empty storage.Scope

	if err := store.Create(t.Context(), empty, patientRecord(patientKey("prj_a", "new"), patientBody)); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("the zero Scope created a resource: err = %v", err)
	}

	if err := store.Update(t.Context(), empty, patientRecord(key, patientBody), "1"); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("the zero Scope updated a resource: err = %v", err)
	}

	if err := store.Delete(t.Context(), empty, key, "1"); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("the zero Scope deleted a resource: err = %v", err)
	}
}

func TestZeroScopeCompilesToAQueryMatchingNoRows(t *testing.T) {
	store, db := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	var empty storage.Scope

	compiled := map[string]func() (string, []any, error){
		"read":    func() (string, []any, error) { return currentStatement(empty, key, storage.ActionRead) },
		"vread":   func() (string, []any, error) { return versionStatement(empty, key, "1") },
		"history": func() (string, []any, error) { return versionsStatement(empty, key) },
		"write arm": func() (string, []any, error) {
			return writeStatement(updateClauses.expecting, empty, key, storage.ActionWrite)
		},
	}

	for name, compile := range compiled {
		text, args, err := compile()
		if err != nil {
			t.Fatalf("%s did not compile: %v", name, err)
		}

		if !strings.Contains(text, "1 = 0") {
			t.Errorf("%s compiled without a constantly false predicate: %s", name, text)
		}

		if name == "write arm" {
			continue
		}

		if got := countRows(t, db, text, args); got != 0 {
			t.Errorf("%s matched %d rows under the zero Scope, want 0", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// One Grant does not stand in for another
// ---------------------------------------------------------------------------

func TestReadGrantDoesNotAuthorizeAWrite(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	readOnly := storage.NewScope(fhirGrant("prj_a", "Patient", storage.ActionRead))

	if err := store.Create(t.Context(), readOnly, patientRecord(patientKey("prj_a", "new"), patientBody)); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a read Grant created a resource: err = %v", err)
	}

	if err := store.Update(t.Context(), readOnly, patientRecord(key, `{"tampered":true}`), "1"); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a read Grant updated a resource: err = %v", err)
	}

	if err := store.Delete(t.Context(), readOnly, key, "1"); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a read Grant deleted a resource: err = %v", err)
	}

	record, err := store.Read(t.Context(), readOnly, key)
	if err != nil {
		t.Fatalf("read under a read Grant: %v", err)
	}

	if record.Version != "1" || string(record.Content) != patientBody {
		t.Fatalf("the resource changed under a read-only Scope: version %q content %q", record.Version, record.Content)
	}
}

func TestReadGrantDoesNotAuthorizeHistory(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	readOnly := storage.NewScope(fhirGrant("prj_a", "Patient", storage.ActionRead))

	if _, err := store.ListVersions(t.Context(), readOnly, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a Scope covering only the current row's type listed its history: err = %v", err)
	}

	if _, err := store.ReadVersion(t.Context(), readOnly, key, "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a Scope covering only the current row's type read a version: err = %v", err)
	}
}

func TestHistoryGrantForAnotherTypeReadsNothing(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	elsewhere := storage.NewScope(fhirGrant("prj_a", "Observation", storage.ActionHistory))

	if _, err := store.ListVersions(t.Context(), elsewhere, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("an Observation history Grant listed a Patient's history: err = %v", err)
	}
}

func TestWriteGrantForAnotherTypeWritesNothing(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	elsewhere := storage.NewScope(fhirGrant("prj_a", "Observation", storage.ActionWrite))

	if err := store.Update(t.Context(), elsewhere, patientRecord(key, `{"tampered":true}`), "1"); !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("an Observation write Grant updated a Patient: err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Compartments
// ---------------------------------------------------------------------------

func TestCompartmentGrantReadsNothingOutsideItsCompartment(t *testing.T) {
	store, db := newStore(t)
	mine := seed(t, store, "prj_a", "mine")
	theirs := seed(t, store, "prj_a", "theirs")

	attachCompartment(t, db, mine, storage.Compartment{Type: "Patient", ID: "pat-1"})
	attachCompartment(t, db, theirs, storage.Compartment{Type: "Patient", ID: "pat-2"})

	scope := storage.NewScope(compartmentGrant("prj_a", "Patient", storage.ActionRead,
		storage.Compartment{Type: "Patient", ID: "pat-1"}))

	if _, err := store.Read(t.Context(), scope, mine); err != nil {
		t.Fatalf("read inside the granted compartment: %v", err)
	}

	if _, err := store.Read(t.Context(), scope, theirs); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a compartment Grant read outside its compartment: err = %v", err)
	}
}

func TestHistoryChecksTheCompartmentOfEachVersion(t *testing.T) {
	store, _ := newStore(t)
	key := seed(t, store, "prj_a", "chart")

	// Version 1 landed in no compartment; version 2 states one, so the two
	// versions differ in exactly the fact a history read is checked against.
	amended := patientRecord(key, `{"v":2}`)
	amended.Compartments = []storage.Compartment{{Type: "Patient", ID: "pat-1"}}

	if err := store.Update(t.Context(), fullScope("prj_a", "Patient"), amended, "1"); err != nil {
		t.Fatalf("update: %v", err)
	}

	scope := storage.NewScope(compartmentGrant("prj_a", "Patient", storage.ActionHistory,
		storage.Compartment{Type: "Patient", ID: "pat-1"}))

	versions, err := store.ListVersions(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}

	if len(versions) != 1 || versions[0].Version != "2" {
		t.Fatalf("history served %d versions %v; a version outside the Grant's compartment must not be returned",
			len(versions), versions)
	}
}

func TestCompartmentRestrictedGrantCannotCreate(t *testing.T) {
	store, _ := newStore(t)

	scope := storage.NewScope(
		fhirGrant("prj_a", "Patient", storage.ActionRead),
		compartmentGrant("prj_a", "Patient", storage.ActionWrite,
			storage.Compartment{Type: "Patient", ID: "pat-1"}),
	)

	err := store.Create(t.Context(), scope, patientRecord(patientKey("prj_a", "new"), patientBody))
	if !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("a compartment-restricted Grant created a row storage cannot place in that compartment: err = %v", err)
	}
}

// And the other half of the same rule. A new row carries no compartment
// projection, so a read confined to one can never see what the create wrote:
// the caller would be answered as if the row it committed did not exist.
func TestACompartmentRestrictedReadCannotCreate(t *testing.T) {
	store, db := newStore(t)

	scope := storage.NewScope(
		compartmentGrant("prj_a", "Patient", storage.ActionRead,
			storage.Compartment{Type: "Patient", ID: "pat-1"}),
		fhirGrant("prj_a", "Patient", storage.ActionWrite),
	)

	err := store.Create(t.Context(), scope, patientRecord(patientKey("prj_a", "new"), patientBody))
	if !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("a create was allowed that its own caller could never read back: err = %v", err)
	}

	if stored := countRows(t, db, "SELECT 1 FROM fhir_resource WHERE res_id = ?", []any{"new"}); stored != 0 {
		t.Fatalf("%d row(s) were written by a refused create", stored)
	}
}

// ---------------------------------------------------------------------------
// Identity reuse
// ---------------------------------------------------------------------------

func TestRecreatedIdHidesTheVersionsWrittenBeforeTheDelete(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := seed(t, store, "prj_a", "reused")

	if err := store.Update(t.Context(), scope, patientRecord(key, `{"v":2}`), "1"); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := store.Delete(t.Context(), scope, key, "2"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := store.Create(t.Context(), scope, patientRecord(key, `{"v":4}`)); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	versions, err := store.ListVersions(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}

	if len(versions) != 1 || versions[0].Version != "4" {
		t.Fatalf("the new owner inherited %d versions %v from before the delete", len(versions), versions)
	}

	if _, err := store.ReadVersion(t.Context(), scope, key, "1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a version written before the delete was still readable: err = %v", err)
	}
}

// A write that names no version replaces whatever the row holds. Without that,
// a caller who asked for no concurrency control is handed one it cannot retry.
func TestAWriteExpectingNoVersionReplacesWhateverIsThere(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := seed(t, store, "prj_a", "shared")

	if err := store.Update(t.Context(), scope, patientRecord(key, `{"v":2}`), "1"); err != nil {
		t.Fatalf("take the resource to version 2: %v", err)
	}

	if err := store.Update(t.Context(), scope, patientRecord(key, `{"v":3}`), ""); err != nil {
		t.Fatalf("update expecting no version: %v", err)
	}

	record, err := store.Read(t.Context(), scope, key)
	if err != nil || record.Version != "3" {
		t.Fatalf("read back version %q: %v, want 3", record.Version, err)
	}

	// The named version is still enforced by the same statement that writes.
	if err := store.Update(t.Context(), scope, patientRecord(key, `{"v":4}`), "2"); !errors.Is(
		err, storage.ErrVersionConflict) {
		t.Fatalf("a stale claim answered %v, want %v", err, storage.ErrVersionConflict)
	}

	if err := store.Delete(t.Context(), scope, key, ""); err != nil {
		t.Fatalf("delete expecting no version: %v", err)
	}

	if _, err := store.Read(t.Context(), scope, key); !errors.Is(err, storage.ErrDeleted) {
		t.Fatalf("read after an unconditional delete: %v, want %v", err, storage.ErrDeleted)
	}
}

func TestCreateDoesNotOverwriteALiveRow(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := seed(t, store, "prj_a", "taken")

	if err := store.Create(t.Context(), scope, patientRecord(key, `{"tampered":true}`)); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("a create overwrote an existing row: err = %v", err)
	}

	record, err := store.Read(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(record.Content) != patientBody {
		t.Fatalf("content = %q, want the original body", record.Content)
	}
}

// ---------------------------------------------------------------------------
// The contract's own behaviour
// ---------------------------------------------------------------------------

func TestWriteReadDeleteRoundTrip(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := seed(t, store, "prj_a", "round-trip")

	if err := store.Update(t.Context(), scope, patientRecord(key, `{"v":2}`), "1"); err != nil {
		t.Fatalf("update: %v", err)
	}

	record, err := store.Read(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if record.Version != "2" || string(record.Content) != `{"v":2}` {
		t.Fatalf("read returned version %q content %q", record.Version, record.Content)
	}

	if err := store.Delete(t.Context(), scope, key, "2"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := store.Read(t.Context(), scope, key); !errors.Is(err, storage.ErrDeleted) {
		t.Fatalf("a deleted resource read as %v, want ErrDeleted", err)
	}

	versions, err := store.ListVersions(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}

	if len(versions) != 3 {
		t.Fatalf("history holds %d versions, want 3", len(versions))
	}
}

func TestUpdateRequiresTheExpectedVersion(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := seed(t, store, "prj_a", "concurrent")

	err := store.Update(t.Context(), scope, patientRecord(key, `{"v":2}`), "9")
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("a stale expected version was accepted: err = %v", err)
	}
}

func TestUpdateOfAnUnknownResourceIsNotFound(t *testing.T) {
	store, _ := newStore(t)

	err := store.Update(t.Context(), fullScope("prj_a", "Patient"), patientRecord(patientKey("prj_a", "ghost"), patientBody), "1")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWithinTransactionRollsBackEveryStatement(t *testing.T) {
	store, _ := newStore(t)
	scope := fullScope("prj_a", "Patient")
	key := patientKey("prj_a", "rolled-back")

	stop := errors.New("stop")

	err := store.WithinTransaction(t.Context(), func(ctx context.Context) error {
		if err := store.Create(ctx, scope, patientRecord(key, patientBody)); err != nil {
			return err
		}

		return stop
	})

	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the work's own error", err)
	}

	if _, err := store.Read(t.Context(), scope, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the rolled back create survived: err = %v", err)
	}

	if _, err := store.ListVersions(t.Context(), scope, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the rolled back version survived: err = %v", err)
	}
}

func TestAssertInScopeRejectsARowFromAnotherProject(t *testing.T) {
	record := storage.ResourceRecord{Key: patientKey("prj_b", "shared")}

	err := assertInScope(fullScope("prj_a", "Patient"), record, storage.ActionRead)
	if !errors.Is(err, ErrScopeEscape) {
		t.Fatalf("err = %v, want ErrScopeEscape", err)
	}
}

// ---------------------------------------------------------------------------
// The compiler itself
// ---------------------------------------------------------------------------

// compiledStatements is every statement shape the store emits under a Scope
// that authorizes it, so the structural guards below cover all of them.
func compiledStatements(t *testing.T, key storage.ResourceKey) map[string]struct {
	text string
	args []any
} {
	t.Helper()

	scope := fullScope(key.Project, key.Type)
	restricted := storage.NewScope(
		compartmentGrant(key.Project, key.Type, storage.ActionRead,
			storage.Compartment{Type: "Patient", ID: "pat-1"}),
		compartmentGrant(key.Project, key.Type, storage.ActionHistory,
			storage.Compartment{Type: "Patient", ID: "pat-1"}),
	)

	// A filtered Grant compiles relations of its own, so the structural guards
	// below have to see one or the newest predicate is the one nothing checks.
	deep, err := storage.NewFilter("category.coding.code", storage.ComparatorIn, "vital-signs", "laboratory")
	if err != nil {
		t.Fatalf("build filter: %v", err)
	}

	narrowed := storage.NewScope(
		filteredGrant(key.Project, key.Type, storage.ActionRead, deep),
		filteredGrant(key.Project, key.Type, storage.ActionHistory, deep),
	)

	statements := map[string]struct {
		text string
		args []any
	}{}

	add := func(name, text string, args []any, err error) {
		if err != nil {
			t.Fatalf("%s did not compile: %v", name, err)
		}

		statements[name] = struct {
			text string
			args []any
		}{text: text, args: args}
	}

	text, args, err := currentStatement(scope, key, storage.ActionRead)
	add("read", text, args, err)

	text, args, err = currentStatement(restricted, key, storage.ActionRead)
	add("read in compartment", text, args, err)

	text, args, err = versionStatement(scope, key, "1")
	add("vread", text, args, err)

	text, args, err = versionStatement(restricted, key, "1")
	add("vread in compartment", text, args, err)

	text, args, err = versionsStatement(scope, key)
	add("history", text, args, err)

	text, args, err = versionsStatement(restricted, key)
	add("history in compartment", text, args, err)

	text, args, err = currentStatement(narrowed, key, storage.ActionRead)
	add("read under a filter", text, args, err)

	text, args, err = versionStatement(narrowed, key, "1")
	add("vread under a filter", text, args, err)

	text, args, err = versionsStatement(narrowed, key)
	add("history under a filter", text, args, err)

	return statements
}

// filteredGrant is one Grant narrowed by an element filter.
func filteredGrant(
	project storage.ProjectID,
	resourceType storage.ResourceType,
	action storage.Action,
	filter storage.Filter,
) storage.Grant {
	grant := fhirGrant(project, resourceType, action)
	grant.Filter = &filter

	return grant
}

// TestEveryArmBindsOneProjectPerRelation counts the tables an arm reads against
// the Project literals it binds, both read off the compiled statement. A
// relation that inherits its Project from another relation drops the count.
func TestEveryArmBindsOneProjectPerRelation(t *testing.T) {
	key := patientKey("prj_a", "shared")
	compartment := storage.Compartment{Type: "Patient", ID: "pat-1"}

	mustArm := func(compiled arm, err error) arm {
		t.Helper()

		if err != nil {
			t.Fatalf("compile arm: %v", err)
		}

		return compiled
	}

	arms := map[string]struct {
		compiled arm
		relation string
	}{
		"current": {
			mustArm(currentArm(fhirGrant("prj_a", "Patient", storage.ActionRead), key)), currentRelation,
		},
		"current in compartment": {
			mustArm(currentArm(compartmentGrant("prj_a", "Patient", storage.ActionRead, compartment), key)),
			currentRelation,
		},
		"history": {
			mustArm(historyArm(fhirGrant("prj_a", "Patient", storage.ActionHistory), key)), historyRelation,
		},
		"history in compartment": {
			mustArm(historyArm(compartmentGrant("prj_a", "Patient", storage.ActionHistory, compartment), key)),
			historyRelation,
		},
	}

	for name, shape := range arms {
		text, args := shape.compiled.query("1", shape.relation)

		relations := strings.Count(text, "FROM ")
		if relations == 0 {
			t.Errorf("%s reads no relation", name)
		}

		bound := 0

		for _, arg := range args {
			if arg == "prj_a" {
				bound++
			}
		}

		if bound != relations {
			t.Errorf("%s binds %d projects across %d relations; every relation must bind its own literal: %s",
				name, bound, relations, text)
		}

		if placeholders := strings.Count(text, "?"); placeholders != len(args) {
			t.Errorf("%s has %d placeholders and %d arguments", name, placeholders, len(args))
		}
	}
}

func TestNoCompiledStatementComparesTwoProjectColumns(t *testing.T) {
	relative := regexp.MustCompile(`project_id\s*=\s*[^?\s]`)

	for name, statement := range compiledStatements(t, patientKey("prj_a", "shared")) {
		if match := relative.FindString(statement.text); match != "" {
			t.Errorf("%s compares project_id to something other than a bound literal (%q): %s",
				name, match, statement.text)
		}
	}

	for name, prefix := range map[string]string{
		"recreate":             recreatePrefix,
		"update":               updateClauses.expecting,
		"unconditional update": updateClauses.unconditional,
		"delete":               deleteClauses.expecting,
		"unconditional delete": deleteClauses.unconditional,
	} {
		if match := relative.FindString(prefix); match != "" {
			t.Errorf("%s compares project_id to something other than a bound literal (%q)", name, match)
		}
	}
}

// TestSourceNeverComparesTwoProjectColumns guards the statements built inline in
// the write paths too, so a relative tenant comparison cannot be introduced
// anywhere in this file.
func TestSourceNeverComparesTwoProjectColumns(t *testing.T) {
	source, err := os.ReadFile("resource.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	relative := regexp.MustCompile(`project_id\s*=\s*[^?\s]`)

	for _, match := range relative.FindAllString(string(source), -1) {
		t.Errorf("resource.go compares project_id to something other than a bound literal: %q", match)
	}
}

func TestCompiledStatementsNeitherScanNorSkipScan(t *testing.T) {
	store, db := newStore(t)
	key := seed(t, store, "prj_a", "shared")

	aliases := map[string]bool{"r": true, "h": true, "c": true, "hc": true, "e": true}

	for name, statement := range compiledStatements(t, key) {
		rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement.text, statement.args...)
		if err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}

		for rows.Next() {
			var (
				id, parent, unused int
				detail             string
			)

			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("scan plan for %s: %v", name, err)
			}

			if strings.Contains(detail, "ANY(") {
				t.Errorf("%s skip-scans, so a leading index column is unbound: %s", name, detail)
			}

			if after, found := strings.CutPrefix(detail, "SCAN "); found {
				if alias, _, _ := strings.Cut(after, " "); aliases[alias] {
					t.Errorf("%s scans the compiler's own relation %q: %s", name, alias, detail)
				}
			}
		}

		if err := rows.Err(); err != nil {
			t.Fatalf("read plan for %s: %v", name, err)
		}

		_ = rows.Close()
	}
}

// TestDroppingTheProjectPredicateCrossesProjects is the loud failure the rest of
// the suite depends on: it proves the bound project literal, and nothing else,
// is what keeps the read inside one Project.
func TestDroppingTheProjectPredicateCrossesProjects(t *testing.T) {
	store, db := newStore(t)
	seed(t, store, "prj_a", "shared")
	seed(t, store, "prj_b", "shared")

	text, args, err := currentStatement(fullScope("prj_a", "Patient"), patientKey("prj_a", "shared"), storage.ActionRead)
	if err != nil {
		t.Fatalf("the read did not compile: %v", err)
	}

	unlimited := strings.Replace(text, " LIMIT 1", "", 1)

	if got := countRows(t, db, unlimited, args); got != 1 {
		t.Fatalf("the compiled read matched %d rows, want exactly the caller's own", got)
	}

	const anchor = "r.project_id = ? AND "

	if !strings.Contains(unlimited, anchor) {
		t.Fatal("the compiled read no longer binds its project as a literal")
	}

	leaky := strings.Replace(unlimited, anchor, "", 1)

	if got := countRows(t, db, leaky, args[1:]); got != 2 {
		t.Fatalf("dropping the project predicate matched %d rows, want 2; the predicate is not what isolates projects", got)
	}
}

// ---------------------------------------------------------------------------
// A resource's placement follows its content
// ---------------------------------------------------------------------------

// placementOf reads the projection the compartment predicate matches against.
func placementOf(t *testing.T, db *sql.DB, key storage.ResourceKey) []storage.Compartment {
	t.Helper()

	rows, err := db.QueryContext(t.Context(),
		"SELECT comp_type, comp_id FROM fhir_resource_compartment"+
			" WHERE project_id = ? AND res_type = ? AND res_id = ? ORDER BY comp_type, comp_id",
		string(key.Project), string(key.Type), string(key.ID))
	if err != nil {
		t.Fatalf("read placement: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var placed []storage.Compartment

	for rows.Next() {
		var compartment storage.Compartment
		if err := rows.Scan(&compartment.Type, &compartment.ID); err != nil {
			t.Fatalf("scan placement: %v", err)
		}

		placed = append(placed, compartment)
	}

	return placed
}

// TestAnUpdateReplacesTheResourcesPlacement. A projection left behind by the
// previous version answers for content that no longer says it: the patient a
// resource has moved away from would go on reading it, and the patient it moved
// to could not. The content the caller submitted is the only thing that decides
// where a resource is.
func TestAnUpdateReplacesTheResourcesPlacement(t *testing.T) {
	store, db := newStore(t)
	key := seed(t, store, "prj_a", "chart")

	scope := fullScope("prj_a", "Patient")

	moved := patientRecord(key, `{"resourceType":"Patient","v":2}`)
	moved.Compartments = []storage.Compartment{{Type: "Patient", ID: "pat-2"}}

	if err := store.Update(t.Context(), scope, moved, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	placed := placementOf(t, db, key)
	if len(placed) != 1 || placed[0] != (storage.Compartment{Type: "Patient", ID: "pat-2"}) {
		t.Fatalf("after the update the resource is placed at %v, want only the compartment it now states", placed)
	}

	// The same fact read the way an authorization check reads it.
	previous := storage.NewScope(compartmentGrant("prj_a", "Patient", storage.ActionRead,
		storage.Compartment{Type: "Patient", ID: "pat-1"}))
	if _, err := store.Read(t.Context(), previous, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a compartment the resource has left still reads it: err = %v", err)
	}

	current := storage.NewScope(compartmentGrant("prj_a", "Patient", storage.ActionRead,
		storage.Compartment{Type: "Patient", ID: "pat-2"}))
	if _, err := store.Read(t.Context(), current, key); err != nil {
		t.Errorf("the compartment the resource now states cannot read it: %v", err)
	}
}

// TestAConfinedCallerCannotMoveAResourceOutOfItsCompartment. Writing a resource
// into a compartment nobody granted is the same act whether the row is new or
// already there, so an update is refused for the reason a create is.
func TestAConfinedCallerCannotMoveAResourceOutOfItsCompartment(t *testing.T) {
	store, db := newStore(t)
	key := seed(t, store, "prj_a", "chart")

	mine := storage.Compartment{Type: "Patient", ID: "pat-1"}
	attachCompartment(t, db, key, mine)

	scope := storage.NewScope(
		compartmentGrant("prj_a", "Patient", storage.ActionRead, mine),
		compartmentGrant("prj_a", "Patient", storage.ActionWrite, mine),
	)

	moved := patientRecord(key, `{"resourceType":"Patient","v":2}`)
	moved.Compartments = []storage.Compartment{{Type: "Patient", ID: "pat-2"}}

	if err := store.Update(t.Context(), scope, moved, ""); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a confined caller moved a resource to another patient: err = %v", err)
	}

	// The refusal is the move, not the update: the same write staying put works.
	stays := patientRecord(key, `{"resourceType":"Patient","v":2}`)
	stays.Compartments = []storage.Compartment{mine}

	if err := store.Update(t.Context(), scope, stays, ""); err != nil {
		t.Errorf("a confined caller could not update inside its own compartment: %v", err)
	}
}

// TestADeleteKeepsThePlacementItHad, so a tombstone stays attributable to
// whoever could reach the resource. A delete states no new content and so
// states no new placement.
func TestADeleteKeepsThePlacementItHad(t *testing.T) {
	store, db := newStore(t)
	key := seed(t, store, "prj_a", "chart")

	mine := storage.Compartment{Type: "Patient", ID: "pat-1"}
	attachCompartment(t, db, key, mine)

	if err := store.Delete(t.Context(), fullScope("prj_a", "Patient"), key, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	placed := placementOf(t, db, key)
	if len(placed) != 1 || placed[0] != mine {
		t.Errorf("the tombstone is placed at %v, want the placement it had", placed)
	}
}
