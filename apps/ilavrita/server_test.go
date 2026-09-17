package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func preparedDatabase(t *testing.T) *sql.DB {
	t.Helper()

	opened, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	if err := prepareDatabase(context.Background(), opened.DB()); err != nil {
		t.Fatalf("prepare the database: %v", err)
	}

	return opened.DB()
}

// Applying the schema is a startup precondition, so the tables the FHIR routes
// read must exist the moment the server begins serving.
func TestPrepareDatabaseAppliesTheSchema(t *testing.T) {
	db := preparedDatabase(t)

	for _, table := range []string{"projects", "users", "fhir_resource", "fhir_resource_history"} {
		var name string

		query := "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?"
		if err := db.QueryRowContext(context.Background(), query, table).Scan(&name); err != nil {
			t.Fatalf("table %q is missing after startup: %v", table, err)
		}
	}
}

// The composite keys guarding Project isolation are constraints only while
// foreign keys are enforced, so a connection without them must not be served on.
func TestPrepareDatabaseRefusesWithoutForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatalf("open a database with foreign keys off: %v", err)
	}

	defer func() { _ = db.Close() }()

	if err := prepareDatabase(context.Background(), db); err == nil {
		t.Fatal("a connection that does not enforce foreign keys was accepted")
	}
}

// A nil port is a wiring mistake authz reports as a failed decision, which would
// turn every FHIR request into a 500 rather than an authorization answer.
func TestBackendWiresEveryAuthorizationPort(t *testing.T) {
	wired := newBackend(preparedDatabase(t), nil)

	switch {
	case wired.resolvers.Memberships == nil:
		t.Fatal("membership resolver is not wired")
	case wired.resolvers.Projects == nil:
		t.Fatal("project resolver is not wired")
	case wired.resolvers.Policies == nil:
		t.Fatal("policy resolver is not wired")
	case wired.resolvers.Links == nil:
		t.Fatal("link resolver is not wired")
	case wired.resources == nil || wired.users == nil:
		t.Fatal("the stores are not wired")
	}
}

func TestByKeyRoutesReachNoLinkedProject(t *testing.T) {
	links, err := noProjectLinks{}.Inbound(context.Background(), "clinic-a")
	if err != nil {
		t.Fatalf("resolve inbound links: %v", err)
	}

	if len(links) != 0 {
		t.Fatalf("resolved %d links, want none reachable by key", len(links))
	}
}
