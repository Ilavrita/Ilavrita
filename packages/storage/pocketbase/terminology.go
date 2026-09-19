package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/terminology"
)

// importBatch is how many concepts are written per statement group. A release
// holds hundreds of thousands, and one transaction per concept would take hours
// while one transaction for all of them would hold every other writer out.
const importBatch = 2000

// TerminologyStore holds the code systems a deployment supplied for itself.
type TerminologyStore struct {
	db *sql.DB
}

// NewTerminologyStore binds the store to one database.
func NewTerminologyStore(db *sql.DB) *TerminologyStore { return &TerminologyStore{db: db} }

// Loaded is one code system an install holds.
type Loaded struct {
	System     string
	Version    string
	Held       int
	Source     string
	ImportedAt time.Time
}

// Import replaces one code system with what a release holds.
//
// It replaces rather than merges: a release is a statement of what the system
// is at a point in time, and a merge would leave codes from an older one sitting
// beside it with nothing to say which release they came from.
//
// It is recorded as the super job it is — work done to the install, against the
// table it was done to — so an operator can see which release is loaded and
// when it arrived.
func (s *TerminologyStore) Import(
	ctx context.Context, release terminology.Release, source string, at time.Time,
) (Loaded, error) {
	held := Loaded{
		System: release.System(), Version: release.Version(),
		Source: source, ImportedAt: at.UTC(),
	}

	done, err := Perform(ctx, s.db, at, SuperJob{
		Name:    "import.terminology." + release.System(),
		Kind:    JobSeed,
		Subject: "terminology_concept",
	}, func(ctx context.Context) (Done, error) {
		count, err := s.replaceSystem(ctx, release, held)
		if err != nil {
			return Done{}, err
		}

		held.Held = count

		return Done{
			Changed:     true,
			Fingerprint: release.System() + " " + release.Version(),
			Detail:      strconv.Itoa(count) + " concepts from " + source,
		}, nil
	})
	if err != nil {
		return Loaded{}, err
	}

	_ = done

	return held, nil
}

// replaceSystem writes the release, in batches, inside one transaction.
func (s *TerminologyStore) replaceSystem(
	ctx context.Context, release terminology.Release, held Loaded,
) (int, error) {
	count := 0

	err := s.withinTransaction(ctx, func(ctx context.Context) error {
		if err := s.forget(ctx, release.System()); err != nil {
			return err
		}

		const declare = "INSERT INTO terminology_system" +
			" (system, version, held, source, imported_at) VALUES (?, ?, 0, ?, ?)"

		if _, err := conn(ctx, s.db).ExecContext(ctx, declare,
			held.System, held.Version, held.Source, held.ImportedAt.UnixMilli()); err != nil {
			return fmt.Errorf("pocketbase: declare %s: %w", held.System, err)
		}

		batch := make([]terminology.Concept, 0, importBatch)

		flush := func() error {
			if len(batch) == 0 {
				return nil
			}

			if err := s.writeConcepts(ctx, release.System(), batch); err != nil {
				return err
			}

			count += len(batch)
			batch = batch[:0]

			return nil
		}

		if err := release.Read(func(concept terminology.Concept) error {
			batch = append(batch, concept)

			if len(batch) < importBatch {
				return nil
			}

			return flush()
		}); err != nil {
			return err
		}

		if err := flush(); err != nil {
			return err
		}

		const record = "UPDATE terminology_system SET held = ? WHERE system = ?"

		if _, err := conn(ctx, s.db).ExecContext(ctx, record, count, held.System); err != nil {
			return fmt.Errorf("pocketbase: record what %s holds: %w", held.System, err)
		}

		return nil
	})

	return count, err
}

// forget removes a system and, by the foreign key, everything in it.
func (s *TerminologyStore) forget(ctx context.Context, system string) error {
	if _, err := conn(ctx, s.db).ExecContext(ctx,
		"DELETE FROM terminology_system WHERE system = ?", system); err != nil {
		return fmt.Errorf("pocketbase: withdraw %s: %w", system, err)
	}

	return nil
}

// writeConcepts inserts one batch. A code stated twice in a release is the
// release's own repetition, and the last one wins rather than failing an import
// that is otherwise fine.
func (s *TerminologyStore) writeConcepts(
	ctx context.Context, system string, held []terminology.Concept,
) error {
	const insert = "INSERT INTO terminology_concept (system, code, display, active)" +
		" VALUES (?, ?, ?, ?)" +
		" ON CONFLICT (system, code) DO UPDATE SET" +
		" display = excluded.display, active = excluded.active"

	for _, concept := range held {
		active := 0
		if concept.Active {
			active = 1
		}

		if _, err := conn(ctx, s.db).ExecContext(ctx, insert,
			system, concept.Code, concept.Display, active); err != nil {
			return fmt.Errorf("pocketbase: store %s|%s: %w", system, concept.Code, err)
		}
	}

	return nil
}

// Lookup returns what one code means, and reports whether the install holds it.
//
// A system the install never loaded and a code that is not in one it did are
// different answers, which is why Holds says which: "I do not hold LOINC" and
// "LOINC has no such code" mean different things to whoever asked.
func (s *TerminologyStore) Lookup(
	ctx context.Context, system, code string,
) (terminology.Concept, bool, error) {
	const query = "SELECT display, active FROM terminology_concept" +
		" WHERE system = ? AND code = ?"

	var (
		display string
		active  int
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, system, code).
		Scan(&display, &active); {
	case errors.Is(err, sql.ErrNoRows):
		return terminology.Concept{}, false, nil
	case err != nil:
		return terminology.Concept{}, false, fmt.Errorf("pocketbase: look a code up: %w", err)
	}

	return terminology.Concept{Code: code, Display: display, Active: active == 1}, true, nil
}

// Holds reports whether the install loaded one code system at all.
func (s *TerminologyStore) Holds(ctx context.Context, system string) (Loaded, bool, error) {
	const query = "SELECT system, version, held, source, imported_at" +
		" FROM terminology_system WHERE system = ?"

	var (
		held     Loaded
		imported int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, system).
		Scan(&held.System, &held.Version, &held.Held, &held.Source, &imported); {
	case errors.Is(err, sql.ErrNoRows):
		return Loaded{}, false, nil
	case err != nil:
		return Loaded{}, false, fmt.Errorf("pocketbase: read a code system: %w", err)
	}

	held.ImportedAt = time.UnixMilli(imported).UTC()

	return held, true, nil
}

// Systems lists what the install holds, for an operator asking what is loaded.
func (s *TerminologyStore) Systems(ctx context.Context) ([]Loaded, error) {
	const query = "SELECT system, version, held, source, imported_at" +
		" FROM terminology_system ORDER BY system"

	rows, err := conn(ctx, s.db).QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: list the code systems: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var loaded []Loaded

	for rows.Next() {
		var (
			held     Loaded
			imported int64
		)

		if err := rows.Scan(&held.System, &held.Version, &held.Held,
			&held.Source, &imported); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a code system: %w", err)
		}

		held.ImportedAt = time.UnixMilli(imported).UTC()
		loaded = append(loaded, held)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: list the code systems: %w", err)
	}

	return loaded, nil
}

// withinTransaction runs work inside one commit boundary, joining one already
// open.
func (s *TerminologyStore) withinTransaction(
	ctx context.Context, work func(ctx context.Context) error,
) error {
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
