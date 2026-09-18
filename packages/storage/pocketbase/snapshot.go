package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// ErrSnapshotOccupied reports a snapshot asked for over a file that is already
// there. SQLite refuses it too; this says so before anything is attempted.
var ErrSnapshotOccupied = errors.New("pocketbase: a snapshot will not be written over a file")

// Snapshot writes a consistent copy of a database to a path.
//
// VACUUM INTO rather than a file copy. A database file copied while something
// is writing to it is a file with a torn page in it, and one copied without its
// write-ahead log is a database missing whatever had not been checkpointed.
// VACUUM INTO reads one transaction's worth of the database and writes a
// complete one, without stopping the writers.
type Snapshot struct {
	db *sql.DB
}

// NewSnapshot binds a snapshotter to one database.
func NewSnapshot(db *sql.DB) *Snapshot { return &Snapshot{db: db} }

// SnapshotInto writes the copy.
func (s *Snapshot) SnapshotInto(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrSnapshotOccupied, path)
	}

	// Bound rather than spelled into the statement. SQLite takes the filename
	// as an expression, so nothing here has to be quoted correctly — and a path
	// an operator typed is still a value this server should not be building SQL
	// out of.
	if _, err := s.db.ExecContext(context.Background(), "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("pocketbase: snapshot the database: %w", err)
	}

	return nil
}
