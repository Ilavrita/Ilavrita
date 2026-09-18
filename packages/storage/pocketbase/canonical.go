package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// CanonicalStore holds the FHIR specification's own definitions.
//
// They belong to no Project. A copy per Project would be the same bytes written
// as many times as there are tenants, and there is nothing tenant-specific about
// what an Observation is.
type CanonicalStore struct {
	db *sql.DB
}

// NewCanonicalStore binds the store to one database.
func NewCanonicalStore(db *sql.DB) *CanonicalStore { return &CanonicalStore{db: db} }

// Seeded is what one install holds.
type Seeded struct {
	Digest  string
	Release string
	Held    int
	At      time.Time
}

// Seed brings the store up to what this build embeds, and does nothing when it
// is already there.
//
// The digest is read first and the definitions are parsed only if it differs, so
// an ordinary start costs one row rather than thirty-five megabytes of JSON. The
// rest runs in one transaction: a seed interrupted halfway would leave an
// install holding some definitions and a marker saying it holds all of them,
// which is the one state nothing would ever correct.
//
// Two processes starting together are safe. SQLite serialises the transaction,
// and whichever arrives second finds the marker already current.
func (s *CanonicalStore) Seed(ctx context.Context, at time.Time) (Seeded, bool, error) {
	digest := conformance.Digest()
	if digest == "" {
		return Seeded{}, false, conformance.ErrUnreadableDefinitions
	}

	held, err := s.Holding(ctx)
	if err != nil {
		return Seeded{}, false, err
	}

	if held.Digest == digest {
		return held, false, nil
	}

	definitions, err := conformance.Canonical()
	if err != nil {
		return Seeded{}, false, err
	}

	seeded := Seeded{
		Digest: digest, Release: conformance.Release, Held: len(definitions), At: at.UTC(),
	}

	// Recorded as the super job it is: work done to the install, against the
	// table it was done to, with the digest that says which bundles it applied.
	if _, err := Perform(ctx, s.db, at, SuperJob{
		Name: "seed.canonical_resource", Kind: JobSeed, Subject: "canonical_resource",
	}, func(ctx context.Context) (Done, error) {
		if err := s.WithinTransaction(ctx, func(ctx context.Context) error {
			return s.replace(ctx, definitions, seeded)
		}); err != nil {
			return Done{}, err
		}

		return Done{
			Changed:     true,
			Fingerprint: digest,
			Detail:      fmt.Sprintf("%d FHIR %s definitions", seeded.Held, seeded.Release),
		}, nil
	}); err != nil {
		return Seeded{}, false, err
	}

	return seeded, true, nil
}

// replace writes every definition and removes whatever this build no longer
// carries, inside the caller's transaction.
func (s *CanonicalStore) replace(
	ctx context.Context, definitions []conformance.Definition, seeded Seeded,
) error {
	const upsert = "INSERT INTO canonical_resource (res_type, res_id, url, version, content)" +
		" VALUES (?, ?, ?, ?, ?)" +
		" ON CONFLICT (res_type, res_id) DO UPDATE SET" +
		" url = excluded.url, version = excluded.version, content = excluded.content"

	for _, definition := range definitions {
		if _, err := conn(ctx, s.db).ExecContext(ctx, upsert,
			definition.Type, definition.ID, definition.URL, definition.Version,
			string(definition.Content)); err != nil {
			return fmt.Errorf("pocketbase: seed %s/%s: %w", definition.Type, definition.ID, err)
		}
	}

	// Anything this build no longer carries. An upgrade that dropped a
	// definition would otherwise leave the old one being served as current.
	if err := s.removeAllBut(ctx, definitions); err != nil {
		return err
	}

	const mark = "INSERT INTO canonical_seed (id, digest, release, held, seeded_at)" +
		" VALUES ('canonical', ?, ?, ?, ?)" +
		" ON CONFLICT (id) DO UPDATE SET" +
		" digest = excluded.digest, release = excluded.release," +
		" held = excluded.held, seeded_at = excluded.seeded_at"

	if _, err := conn(ctx, s.db).ExecContext(ctx, mark,
		seeded.Digest, seeded.Release, seeded.Held, seeded.At.UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: record what was seeded: %w", err)
	}

	return nil
}

// removeAllBut deletes the definitions this build does not carry.
//
// It reads the ids it is keeping into a temporary table rather than building one
// statement out of them: a NOT IN over two hundred literals is a statement whose
// length depends on the specification.
func (s *CanonicalStore) removeAllBut(
	ctx context.Context, definitions []conformance.Definition,
) error {
	held := conn(ctx, s.db)

	for _, statement := range []string{
		"CREATE TEMPORARY TABLE IF NOT EXISTS seeding (res_type TEXT, res_id TEXT)",
		"DELETE FROM seeding",
	} {
		if _, err := held.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("pocketbase: prepare the seed: %w", err)
		}
	}

	for _, definition := range definitions {
		if _, err := held.ExecContext(ctx,
			"INSERT INTO seeding (res_type, res_id) VALUES (?, ?)",
			definition.Type, definition.ID); err != nil {
			return fmt.Errorf("pocketbase: prepare the seed: %w", err)
		}
	}

	if _, err := held.ExecContext(ctx,
		"DELETE FROM canonical_resource WHERE (res_type, res_id) NOT IN"+
			" (SELECT res_type, res_id FROM seeding)"); err != nil {
		return fmt.Errorf("pocketbase: withdraw what is no longer defined: %w", err)
	}

	if _, err := held.ExecContext(ctx, "DELETE FROM seeding"); err != nil {
		return fmt.Errorf("pocketbase: finish the seed: %w", err)
	}

	return nil
}

// Holding reports what this install last seeded, and the zero value when it has
// seeded nothing.
func (s *CanonicalStore) Holding(ctx context.Context) (Seeded, error) {
	const query = "SELECT digest, release, held, seeded_at FROM canonical_seed WHERE id = 'canonical'"

	var (
		held     Seeded
		seededAt int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query).
		Scan(&held.Digest, &held.Release, &held.Held, &seededAt); {
	case errors.Is(err, sql.ErrNoRows):
		return Seeded{}, nil
	case err != nil:
		return Seeded{}, fmt.Errorf("pocketbase: read what was seeded: %w", err)
	}

	held.At = time.UnixMilli(seededAt).UTC()

	return held, nil
}

// Read returns one canonical resource, and reports whether there is one.
//
// It takes no Scope. These are the specification, not anybody's data: what a
// caller may read is decided before this is reached, by the same rule that
// governs the Project's own copy of that type.
func (s *CanonicalStore) Read(
	ctx context.Context, resourceType storage.ResourceType, id storage.LogicalID,
) (storage.ResourceRecord, bool, error) {
	const query = "SELECT content FROM canonical_resource WHERE res_type = ? AND res_id = ?"

	var content []byte

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query,
		string(resourceType), string(id)).Scan(&content); {
	case errors.Is(err, sql.ErrNoRows):
		return storage.ResourceRecord{}, false, nil
	case err != nil:
		return storage.ResourceRecord{}, false,
			fmt.Errorf("pocketbase: read a canonical resource: %w", err)
	}

	return storage.ResourceRecord{
		Key:     storage.ResourceKey{Type: resourceType, ID: id},
		Version: canonicalVersion,
		Content: content,
	}, true, nil
}

// canonicalVersion is what a canonical resource reports as its version. It is
// the release rather than a counter: these are never written twice, and a
// version that counted would be counting something that does not happen.
const canonicalVersion = storage.VersionID(conformance.Release)

// WithinTransaction runs work inside one commit boundary, joining one already
// open.
func (s *CanonicalStore) WithinTransaction(
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
