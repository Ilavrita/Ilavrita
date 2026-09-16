package pocketbase

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openWith(t *testing.T, pragma string) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "guard.db")
	db, err := sql.Open("sqlite", path+pragma)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestApplySchemaRefusesAConnectionWithoutForeignKeys(t *testing.T) {
	db := openWith(t, "")

	if err := ApplySchema(context.Background(), db); err == nil {
		t.Error("schema applied without foreign key enforcement; every composite-FK guard would be decorative")
	}
}

func TestApplySchemaAcceptsAConnectionWithForeignKeys(t *testing.T) {
	db := openWith(t, "?_pragma=foreign_keys(ON)")

	if err := ApplySchema(context.Background(), db); err != nil {
		t.Fatalf("schema refused a correctly configured connection: %v", err)
	}
}
