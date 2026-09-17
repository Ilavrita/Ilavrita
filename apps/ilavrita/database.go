package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// databaseFile holds every Ilavrita table, in the runtime's own data directory.
// It is a second file because PocketBase's default users collection already owns
// the table name `users`, so one file cannot carry both schemas.
const databaseFile = "ilavrita.db"

// openDatabase opens it exactly the way the runtime opens its own, so the
// pragmas foreign keys and WAL depend on are the runtime's rather than a second
// copy of them that can drift.
func openDatabase(dataDir string) (*dbx.DB, error) {
	db, err := core.DefaultDBConnect(filepath.Join(dataDir, databaseFile))
	if err != nil {
		return nil, fmt.Errorf("ilavrita: open %s: %w", databaseFile, err)
	}

	// One connection: a write reads before it writes within one transaction, and
	// SQLite answers a second writer arriving mid-transaction with SQLITE_BUSY
	// instead of waiting for it.
	db.DB().SetMaxOpenConns(1)
	db.DB().SetMaxIdleConns(1)

	return db, nil
}

// prepareDatabase refuses a database the server may not trust. Foreign keys are
// asserted on the one pooled connection every later query runs on, so the
// constraints guarding Project isolation are enforced rather than declared.
func prepareDatabase(ctx context.Context, db *sql.DB) error {
	if err := sqlite.AssertForeignKeysEnforced(ctx, db); err != nil {
		return fmt.Errorf("ilavrita: %w", err)
	}

	// PrepareSchema rather than ApplySchema: a database created before the client
	// application and bot registries carries no foreign key to them, and SQLite
	// cannot add one to a table that already exists. It adopts them or refuses.
	if err := sqlite.PrepareSchema(ctx, db); err != nil {
		return fmt.Errorf("ilavrita: apply schema to %s: %w", databaseFile, err)
	}

	return nil
}
