package pocketbase

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

// JobKind is what sort of install-wide work a job is.
type JobKind string

// The kinds of work this server does to itself.
const (
	// JobMigration brings a database's shape forward — a column added, a table
	// rebuilt to adopt a constraint SQLite cannot add in place.
	JobMigration JobKind = "migration"

	// JobSeed writes content this build carries into the install.
	JobSeed JobKind = "seed"

	// JobBackfill fills something that exists but is empty, such as a search
	// index an install has only just gained.
	JobBackfill JobKind = "backfill"
)

// ErrUnnamedJob reports a job with no name or no subject, which is a job nobody
// could look up afterwards.
var ErrUnnamedJob = errors.New("pocketbase: a super job names itself and the table it acts on")

// SuperJob is one piece of work done to the whole install.
//
// It belongs to no Project — that is what makes it super rather than ordinary
// work. Name identifies it across runs, and Subject is the table it acts on, so
// "what has been done to user_second_factors" is one query rather than an
// inference from the shape of the table.
type SuperJob struct {
	Name    string
	Kind    JobKind
	Subject string
}

// Done is what one run of a job turned out to be.
//
// Changed is the whole of what decides whether a row is written. A job that
// found nothing to do writes nothing: a server starts far more often than its
// schema changes, and a row per start per job would bury the ones that matter.
//
// Fingerprint is what the job applied, or what made it unnecessary — a digest, a
// column list, a release. It is what a later run compares against, and what an
// operator reads to know which version of the work ran.
type Done struct {
	Changed     bool
	Fingerprint string
	Detail      string
}

// Perform runs one job and records what it did.
//
// The job decides for itself whether there is anything to do: it looks at the
// database, or compares a fingerprint of what it would apply. Running it twice
// does nothing the second time, and that is the job's own property rather than
// something this enforces — what this adds is the record.
//
// A failure is recorded before it is returned, because the run that broke an
// install is exactly the one somebody needs to find afterwards. If the record
// cannot be written either, both failures are returned together: a database too
// broken to take the row is itself worth knowing, and neither should hide the
// other.
func Perform(
	ctx context.Context,
	db *sql.DB,
	at time.Time,
	job SuperJob,
	work func(context.Context) (Done, error),
) (Done, error) {
	if job.Name == "" || job.Subject == "" {
		return Done{}, fmt.Errorf("%w: %+v", ErrUnnamedJob, job)
	}

	began := at.UTC()

	done, err := work(ctx)
	if err != nil {
		return Done{}, errors.Join(err,
			writeJob(ctx, db, job, done, began, time.Now().UTC(), "failed", err.Error()))
	}

	if !done.Changed {
		return done, nil
	}

	if err := writeJob(ctx, db, job, done, began, time.Now().UTC(), "applied", done.Detail); err != nil {
		return Done{}, err
	}

	return done, nil
}

// writeJob inserts one run.
func writeJob(
	ctx context.Context,
	db *sql.DB,
	job SuperJob,
	done Done,
	began, ended time.Time,
	outcome, detail string,
) error {
	const insert = "INSERT INTO super_jobs" +
		" (id, name, kind, subject, fingerprint, outcome, detail, started_at, finished_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"

	id, err := MintJobID(rand.Reader)
	if err != nil {
		return err
	}

	if _, err := conn(ctx, db).ExecContext(ctx, insert,
		id, job.Name, string(job.Kind), job.Subject, done.Fingerprint,
		outcome, detail, began.UnixMilli(), ended.UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: record the %s job: %w", job.Name, err)
	}

	return nil
}

// jobPrefix marks a job identifier, the way every other identifier here is
// marked, so a row's own id says what it is.
const jobPrefix = "job_"

// MintJobID draws an identifier for one run.
func MintJobID(random io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("pocketbase: cannot mint a job identifier: %w", err)
	}

	return jobPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Ran is one recorded run, as an operator reads it.
type Ran struct {
	ID          string
	Name        string
	Kind        JobKind
	Subject     string
	Fingerprint string
	Outcome     string
	Detail      string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// JobsOn returns what has been done to one table, newest first. This is the
// question the record exists to answer.
func JobsOn(ctx context.Context, db *sql.DB, subject string, limit int) ([]Ran, error) {
	const query = "SELECT id, name, kind, subject, fingerprint, outcome, detail," +
		" started_at, finished_at FROM super_jobs WHERE subject = ?" +
		" ORDER BY started_at DESC, id LIMIT ?"

	return readJobs(ctx, db, query, subject, limit)
}

// EveryJob returns what has been done to the install, newest first.
func EveryJob(ctx context.Context, db *sql.DB, limit int) ([]Ran, error) {
	const query = "SELECT id, name, kind, subject, fingerprint, outcome, detail," +
		" started_at, finished_at FROM super_jobs ORDER BY started_at DESC, id LIMIT ?"

	return readJobs(ctx, db, query, limit)
}

func readJobs(ctx context.Context, db *sql.DB, query string, args ...any) ([]Ran, error) {
	rows, err := conn(ctx, db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read the job record: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var held []Ran

	for rows.Next() {
		var (
			ran                 Ran
			kind                string
			startedAt, finished int64
		)

		if err := rows.Scan(&ran.ID, &ran.Name, &kind, &ran.Subject, &ran.Fingerprint,
			&ran.Outcome, &ran.Detail, &startedAt, &finished); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a job record: %w", err)
		}

		ran.Kind = JobKind(kind)
		ran.StartedAt = time.UnixMilli(startedAt).UTC()
		ran.FinishedAt = time.UnixMilli(finished).UTC()

		held = append(held, ran)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read the job record: %w", err)
	}

	return held, nil
}
