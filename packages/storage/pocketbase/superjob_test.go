package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

var ranAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

func jobCount(t *testing.T, db *sql.DB) int {
	t.Helper()

	var held int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM super_jobs").Scan(&held); err != nil {
		t.Fatalf("count the jobs: %v", err)
	}

	return held
}

// TestAJobThatChangedNothingIsNotRecorded. A server starts far more often than
// its schema changes, and a row per start per job would bury the ones that
// matter.
func TestAJobThatChangedNothingIsNotRecorded(t *testing.T) {
	_, db := newStore(t)

	held := SuperJob{Name: "migrate.nothing", Kind: JobMigration, Subject: "projects"}

	for range 5 {
		done, err := Perform(t.Context(), db, ranAt, held,
			func(context.Context) (Done, error) {
				return Done{Changed: false, Fingerprint: "already there"}, nil
			})
		if err != nil {
			t.Fatalf("perform: %v", err)
		}

		if done.Changed {
			t.Error("a job that did nothing reported a change")
		}
	}

	if count := jobCount(t, db); count != 0 {
		t.Errorf("%d rows for five runs that changed nothing", count)
	}
}

// TestWhatAJobDidIsRecordedAgainstTheTable, which is the question the record
// exists to answer: what has been done to this table.
func TestWhatAJobDidIsRecordedAgainstTheTable(t *testing.T) {
	_, db := newStore(t)

	held := SuperJob{
		Name: "migrate.user_second_factors.replacement",
		Kind: JobMigration, Subject: "user_second_factors",
	}

	if _, err := Perform(t.Context(), db, ranAt, held,
		func(context.Context) (Done, error) {
			return Done{Changed: true, Fingerprint: "pending_secret", Detail: "a column"}, nil
		}); err != nil {
		t.Fatalf("perform: %v", err)
	}

	ran, err := JobsOn(t.Context(), db, "user_second_factors", 10)
	if err != nil {
		t.Fatalf("read what was done to the table: %v", err)
	}

	if len(ran) != 1 {
		t.Fatalf("%d runs recorded against the table", len(ran))
	}

	switch {
	case ran[0].Name != held.Name || ran[0].Kind != JobMigration:
		t.Errorf("the row reads %s/%s", ran[0].Name, ran[0].Kind)
	case ran[0].Fingerprint != "pending_secret" || ran[0].Outcome != "applied":
		t.Errorf("the row reads %s/%s", ran[0].Fingerprint, ran[0].Outcome)
	case !ran[0].StartedAt.Equal(ranAt) || ran[0].FinishedAt.Before(ran[0].StartedAt):
		t.Errorf("the row ran %s to %s", ran[0].StartedAt, ran[0].FinishedAt)
	}

	// And nothing else claims that table.
	if other, err := JobsOn(t.Context(), db, "projects", 10); err != nil || len(other) != 0 {
		t.Errorf("another table holds %d runs (%v)", len(other), err)
	}
}

// TestAFailedJobIsRecordedBeforeItIsReturned. The run that broke an install is
// exactly the one somebody needs to find afterwards.
func TestAFailedJobIsRecordedBeforeItIsReturned(t *testing.T) {
	_, db := newStore(t)

	broken := errors.New("the rebuild could not finish")

	_, err := Perform(t.Context(), db, ranAt, SuperJob{
		Name: "migrate.broken", Kind: JobMigration, Subject: "projects",
	}, func(context.Context) (Done, error) {
		return Done{}, broken
	})

	if !errors.Is(err, broken) {
		t.Fatalf("the failure answered %v", err)
	}

	ran, err := JobsOn(t.Context(), db, "projects", 10)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if len(ran) != 1 || ran[0].Outcome != "failed" {
		t.Fatalf("the failure recorded %+v", ran)
	}

	if ran[0].Detail != broken.Error() {
		t.Errorf("the row says %q", ran[0].Detail)
	}
}

// TestAJobRecordIsNotRevised. A record of what a server did to itself that the
// server can edit afterwards is not a record.
func TestAJobRecordIsNotRevised(t *testing.T) {
	_, db := newStore(t)

	if _, err := Perform(t.Context(), db, ranAt, SuperJob{
		Name: "seed.something", Kind: JobSeed, Subject: "projects",
	}, func(context.Context) (Done, error) {
		return Done{Changed: true, Fingerprint: "one"}, nil
	}); err != nil {
		t.Fatalf("perform: %v", err)
	}

	if _, err := db.ExecContext(t.Context(),
		"UPDATE super_jobs SET outcome = 'applied', detail = 'rewritten'"); err == nil {
		t.Error("a job row was revised")
	}
}

// TestAJobNamesItselfAndItsTable, because one that does neither is a run nobody
// could look up afterwards.
func TestAJobNamesItselfAndItsTable(t *testing.T) {
	_, db := newStore(t)

	for described, held := range map[string]SuperJob{
		"naming no job":   {Kind: JobMigration, Subject: "projects"},
		"naming no table": {Name: "migrate.something", Kind: JobMigration},
	} {
		_, err := Perform(t.Context(), db, ranAt, held,
			func(context.Context) (Done, error) {
				return Done{Changed: true}, nil
			})

		if !errors.Is(err, ErrUnnamedJob) {
			t.Errorf("a job %s answered %v", described, err)
		}
	}

	if count := jobCount(t, db); count != 0 {
		t.Errorf("%d rows for jobs that were refused", count)
	}
}
