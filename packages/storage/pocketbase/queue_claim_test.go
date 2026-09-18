package pocketbase

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// TestAnOlderDatabaseGainsTheQueueClaims. An install that predates claiming has
// queues every replica reads in full, which is a write fanned out twice and a
// subscriber posted to twice. Such a database is brought forward rather than
// served.
func TestAnOlderDatabaseGainsTheQueueClaims(t *testing.T) {
	db := legacyQueueDatabase(t)

	if err := AssertQueueClaims(t.Context(), db); !errors.Is(err, ErrQueueClaimsMissing) {
		t.Fatalf("the legacy queues were accepted: err = %v, want %v", err, ErrQueueClaimsMissing)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare the legacy database: %v", err)
	}

	if err := AssertQueueClaims(t.Context(), db); err != nil {
		t.Fatalf("the prepared database still cannot record a claim: %v", err)
	}

	// The rows it held came across, unclaimed — which is what an entry nobody
	// is working through is.
	for _, held := range []struct {
		query string
		want  []string
	}{
		{"SELECT id FROM subscription_backlog WHERE claimed_by IS NULL ORDER BY id",
			[]string{"wrt_one", "wrt_two"}},
		{"SELECT id FROM subscription_deliveries WHERE claimed_by IS NULL ORDER BY id",
			[]string{"dlv_one"}},
	} {
		if carried := idsFrom(t, db, held.query); !slices.Equal(carried, held.want) {
			t.Errorf("the rebuild carried %v, want %v", carried, held.want)
		}
	}

	// The check came with the columns, so a half-stated claim is refused rather
	// than the table merely holding somewhere to put one.
	if _, err := db.ExecContext(t.Context(),
		"UPDATE subscription_backlog SET claimed_by = 'wkr_one' WHERE id = 'wrt_one'"); err == nil {
		t.Error("the rebuilt table accepted a holder with no lease")
	}

	if _, err := db.ExecContext(t.Context(),
		"UPDATE subscription_deliveries SET claimed_until = 1 WHERE id = 'dlv_one'"); err == nil {
		t.Error("the rebuilt table accepted a lease belonging to nobody")
	}
}

// TestPreparingTwiceDoesNotRebuildTheQueues. A rebuild copies every row, so one
// that ran on every start would make starting cost the size of the queue.
func TestPreparingTwiceDoesNotRebuildTheQueues(t *testing.T) {
	db := legacyQueueDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	rebuilt, err := rebuildQueueClaims(t.Context(), db)
	if err != nil {
		t.Fatalf("ask again: %v", err)
	}

	if rebuilt {
		t.Error("a prepared database was rebuilt a second time")
	}
}

func idsFrom(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var held []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}

		held = append(held, id)
	}

	return held
}

// legacyQueueDatabase builds a database whose notification queues predate
// claiming, with entries already in them. The parents are declared first so the
// current schema's own CREATE TABLE IF NOT EXISTS finds the old tables already
// there, which is the situation an upgrade is.
func legacyQueueDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/legacy.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// The whole current schema, then the two queues put back as they were. That
	// is what an install upgrading from an earlier release actually is: every
	// other table current, and these two predating the change.
	if err := ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("apply the schema: %v", err)
	}

	execAll(t, db, []string{
		"DROP TABLE subscription_deliveries",
		"DROP TABLE subscription_backlog",
	})

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_subscription_queues.sql"))
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
		"INSERT INTO subscription_owners (project_id, subscription_id, membership_id," +
			" principal_kind, principal_id, created_at)" +
			" VALUES ('prj_a', 'sub_1', 'pm_1', 'user', 'usr_1', 0)",
		"INSERT INTO subscription_backlog (project_id, id, res_type, res_id, version_id, at)" +
			" VALUES ('prj_a', 'wrt_one', 'Observation', 'obs-1', '1', 1)," +
			" ('prj_a', 'wrt_two', 'Observation', 'obs-2', '1', 2)",
		"INSERT INTO subscription_deliveries (project_id, id, subscription_id, res_type, res_id," +
			" version_id, state, attempts, due_at, created_at)" +
			" VALUES ('prj_a', 'dlv_one', 'sub_1', 'Observation', 'obs-1', '1', 'pending', 0, 1, 1)",
	})

	return db
}

// TestAClaimNamingNobodyIsRefused. Claiming as the empty worker is not
// claiming: every other process would answer to that name too, which is the one
// thing a claim exists to stop. It is refused where the claim is made, so what
// a misconfigured build sees is that and not a constraint it has to decode.
func TestAClaimNamingNobodyIsRefused(t *testing.T) {
	db := legacyQueueDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	store := NewSubscriptionStore(db)
	at := time.UnixMilli(10).UTC()

	for described, claim := range map[string]subscription.Claim{
		"naming no worker":     {At: at, Until: at.Add(time.Minute), Limit: 10},
		"taking no rows":       {Worker: "wkr_one", At: at, Until: at.Add(time.Minute)},
		"leased into the past": {Worker: "wkr_one", At: at, Until: at.Add(-time.Minute), Limit: 10},
	} {
		if _, err := store.Backlog(t.Context(), claim); !errors.Is(err, subscription.ErrUnclaimable) {
			t.Errorf("a backlog claim %s answered %v", described, err)
		}

		if _, err := store.Due(t.Context(), claim); !errors.Is(err, subscription.ErrUnclaimable) {
			t.Errorf("a delivery claim %s answered %v", described, err)
		}
	}

	// And a claim that names somebody still works, so this refuses the claim
	// rather than the claiming.
	if _, err := store.Backlog(t.Context(), subscription.Claim{
		Worker: "wkr_one", At: at, Until: at.Add(time.Minute), Limit: 10,
	}); err != nil {
		t.Errorf("a claim naming a worker was refused: %v", err)
	}
}
