package pocketbase

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// sessionAt is the instant the sessions in this file are issued at.
var sessionAt = time.UnixMilli(1_700_000_000_000).UTC()

// legacySessionsDatabase builds a database whose sessions table predates SMART,
// with a session already open in it. Every other table is current, which is what
// an install upgrading from an earlier release actually is.
func legacySessionsDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/legacy.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("apply the schema: %v", err)
	}

	execAll(t, db, []string{"DROP TABLE sessions"})

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_sessions.sql"))
	if err != nil {
		t.Fatalf("read the legacy declaration: %v", err)
	}

	applyStatements(t, db, string(legacy))

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_a', 'standard', 'prj_a', 'prj_a', 'active', 0, 0, 0)",
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)" +
			" VALUES ('usr_1', 'server', 'one@example.test', 'one@example.test', 'active', 0, 0)",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_1', 'standard', 'usr_1', 'active', 'api', 0, 0, 0)",
		"INSERT INTO sessions (project_id, id, token_hash, user_id, membership_id, state," +
			" created_at, expires_at, revoked_at)" +
			" VALUES ('prj_a', 'ses_old', 'a digest', 'usr_1', 'pm_1', 'active', 0, 3600000, NULL)",
	})

	return db
}

// TestAnInstallThatPredatesSmartGainsSomewhereToRecordAGrant, without which
// every app's session would read as an ordinary login — and an ordinary login is
// narrowed by nothing.
func TestAnInstallThatPredatesSmartGainsSomewhereToRecordAGrant(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := AssertSessionLaunch(t.Context(), db); !errors.Is(err, ErrSessionLaunchMissing) {
		t.Fatalf("before the migration: got %v, want ErrSessionLaunchMissing", err)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := AssertSessionLaunch(t.Context(), db); err != nil {
		t.Fatalf("after the migration: %v", err)
	}
}

// TestTheMigrationCarriesEveryOpenSessionAcrossAsAnOrdinaryLogin.
//
// A session issued before this server could record a grant was issued to
// somebody signing in, never to an app. So it keeps reaching what it reached,
// and the migration narrows nobody.
func TestTheMigrationCarriesEveryOpenSessionAcrossAsAnOrdinaryLogin(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	var patient, scopes sql.NullString

	err := db.QueryRowContext(t.Context(),
		"SELECT launch_patient, granted_scopes FROM sessions WHERE id = 'ses_old'",
	).Scan(&patient, &scopes)
	if err != nil {
		t.Fatalf("read the carried session: %v", err)
	}

	if patient.Valid || scopes.Valid {
		t.Errorf("a session that predates SMART came across as %v/%v, want an ordinary login",
			patient, scopes)
	}
}

// TestASmartSessionRoundTripsThroughTheDatabase, because a grant written and not
// read back is a grant nothing narrows by.
func TestASmartSessionRoundTripsThroughTheDatabase(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	granted, err := project.NewLaunchContext("pat_7", "patient/Observation.read patient/Condition.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	issued, token, err := project.IssueAppSession(
		"prj_a", "ses_app", "usr_1", "pm_1", granted, sessionAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueAppSession: %v", err)
	}

	store := NewSessionStore(db)
	if err := store.Issue(t.Context(), issued); err != nil {
		t.Fatalf("issue: %v", err)
	}

	resolved, found, err := store.Resolve(t.Context(), token, sessionAt.Add(time.Minute))
	if err != nil || !found {
		t.Fatalf("resolve: found %v, err %v", found, err)
	}

	if resolved.Launch() != granted {
		t.Errorf("resolved %v, want %v", resolved.Launch(), granted)
	}
}

// TestAnOrdinaryLoginRoundTripsAsOne, so the absence is NULL in the row and the
// zero value in the session rather than an empty string in either.
func TestAnOrdinaryLoginRoundTripsAsOne(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	issued, token, err := project.IssueSession(
		"prj_a", "ses_login", "usr_1", "pm_1", sessionAt, time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	store := NewSessionStore(db)
	if err := store.Issue(t.Context(), issued); err != nil {
		t.Fatalf("issue: %v", err)
	}

	var scopes sql.NullString

	if err := db.QueryRowContext(t.Context(),
		"SELECT granted_scopes FROM sessions WHERE id = 'ses_login'").Scan(&scopes); err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if scopes.Valid {
		t.Errorf("an ordinary login wrote %q, want NULL", scopes.String)
	}

	resolved, found, err := store.Resolve(t.Context(), token, sessionAt.Add(time.Minute))
	if err != nil || !found {
		t.Fatalf("resolve: found %v, err %v", found, err)
	}

	if !resolved.Launch().IsZero() {
		t.Errorf("an ordinary login resolved as %v", resolved.Launch())
	}
}

// TestTheTableRefusesAPatientWithNoGrant, which is the shape that would read as
// an ordinary login while looking like an app's session to anyone reading the
// table. The domain refuses it too; this is the schema saying so on its own, so
// a writer that bypassed the domain still cannot store one.
func TestTheTableRefusesAPatientWithNoGrant(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	_, err := db.ExecContext(t.Context(),
		"INSERT INTO sessions (project_id, id, token_hash, user_id, membership_id, state,"+
			" launch_patient, granted_scopes, created_at, expires_at, revoked_at)"+
			" VALUES ('prj_a', 'ses_bad', 'another digest', 'usr_1', 'pm_1', 'active',"+
			" 'pat_7', NULL, 0, 3600000, NULL)")

	if err == nil {
		t.Fatal("the table accepted a launch patient with no granted scopes")
	}
}

// TestTheTableRefusesAnEmptyGrant, because absence is NULL: the empty string
// would be a second way to say it, and the two would not narrow alike.
func TestTheTableRefusesAnEmptyGrant(t *testing.T) {
	db := legacySessionsDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	_, err := db.ExecContext(t.Context(),
		"INSERT INTO sessions (project_id, id, token_hash, user_id, membership_id, state,"+
			" launch_patient, granted_scopes, created_at, expires_at, revoked_at)"+
			" VALUES ('prj_a', 'ses_bad', 'another digest', 'usr_1', 'pm_1', 'active',"+
			" NULL, '', 0, 3600000, NULL)")

	if err == nil {
		t.Fatal("the table accepted an empty grant")
	}
}
