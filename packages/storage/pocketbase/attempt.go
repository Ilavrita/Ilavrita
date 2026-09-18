package pocketbase

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// AttemptStore counts failed logins across the install.
//
// Nothing here runs inside another operation's transaction: a failure recorded
// in the transaction that refused would be rolled back with it, and the guess
// would cost the attacker nothing.
type AttemptStore struct {
	db *sql.DB
}

// NewAttemptStore binds the counter to an open database.
func NewAttemptStore(db *sql.DB) *AttemptStore {
	return &AttemptStore{db: db}
}

// Failures counts what this key has failed since the given instant.
func (s *AttemptStore) Failures(
	ctx context.Context, key project.AttemptKey, since time.Time,
) (int, error) {
	const query = "SELECT COUNT(*) FROM login_attempts WHERE key = ? AND at > ?"

	var count int

	if err := s.db.QueryRowContext(ctx, query, string(key), since.UnixMilli()).Scan(&count); err != nil {
		return 0, fmt.Errorf("pocketbase: count login attempts: %w", err)
	}

	return count, nil
}

// RecordFailure counts one refused attempt against this key.
func (s *AttemptStore) RecordFailure(
	ctx context.Context, key project.AttemptKey, at time.Time,
) error {
	const insert = "INSERT INTO login_attempts (key, at) VALUES (?, ?)"

	if _, err := s.db.ExecContext(ctx, insert, string(key), at.UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: record a login attempt: %w", err)
	}

	return nil
}

// ClearFailures forgets what this key failed, which is what a success does: a
// person who mistyped their password four times is not locked out by finally
// getting it right.
func (s *AttemptStore) ClearFailures(ctx context.Context, key project.AttemptKey) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM login_attempts WHERE key = ?", string(key)); err != nil {
		return fmt.Errorf("pocketbase: clear login attempts: %w", err)
	}

	return nil
}

// Sweep drops what aged out of the window. The table would otherwise grow with
// every address anyone ever guessed at.
func (s *AttemptStore) Sweep(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM login_attempts WHERE at <= ?", before.UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: sweep login attempts: %w", err)
	}

	return nil
}
