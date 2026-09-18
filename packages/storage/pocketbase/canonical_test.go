package pocketbase

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

func canonicalStore(t *testing.T) (*CanonicalStore, *sql.DB) {
	t.Helper()

	_, db := newStore(t)

	return NewCanonicalStore(db), db
}

func heldCount(t *testing.T, db *sql.DB) int {
	t.Helper()

	var held int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM canonical_resource").Scan(&held); err != nil {
		t.Fatalf("count the definitions: %v", err)
	}

	return held
}

var seededAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// TestSeedingBringsTheDefinitionsIn, which is what makes this server able to say
// what a resource is rather than only what it is called.
func TestSeedingBringsTheDefinitionsIn(t *testing.T) {
	store, db := canonicalStore(t)

	seeded, changed, err := store.Seed(t.Context(), seededAt)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if !changed {
		t.Error("an empty install reported nothing to seed")
	}

	if seeded.Release != conformance.Release || seeded.Held < 200 {
		t.Errorf("it seeded %d definitions of %s", seeded.Held, seeded.Release)
	}

	if held := heldCount(t, db); held != seeded.Held {
		t.Errorf("%d rows for %d definitions", held, seeded.Held)
	}

	record, found, err := store.Read(t.Context(), "StructureDefinition", "Observation")
	if err != nil || !found {
		t.Fatalf("read Observation's definition: %v (found=%v)", err, found)
	}

	if len(record.Content) < 1000 {
		t.Errorf("it is %d bytes, which is not a definition", len(record.Content))
	}
}

// TestSeedingTwiceChangesNothing. A server starts more often than the
// specification changes, so a start that would change nothing has to cost a row
// rather than thirty-five megabytes of JSON.
func TestSeedingTwiceChangesNothing(t *testing.T) {
	store, db := canonicalStore(t)

	first, _, err := store.Seed(t.Context(), seededAt)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Something a reseed would overwrite. It survives exactly because the
	// second seed does nothing at all.
	if _, err := db.ExecContext(t.Context(),
		"UPDATE canonical_resource SET content = '{\"resourceType\":\"StructureDefinition\"}'"+
			" WHERE res_id = 'Observation'"); err != nil {
		t.Fatalf("mark a row: %v", err)
	}

	second, changed, err := store.Seed(t.Context(), seededAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("seed again: %v", err)
	}

	if changed {
		t.Error("the second seed reported a change")
	}

	if second.Digest != first.Digest || !second.At.Equal(first.At) {
		t.Errorf("the marker moved: %+v then %+v", first, second)
	}

	record, _, err := store.Read(t.Context(), "StructureDefinition", "Observation")
	if err != nil {
		t.Fatalf("read it back: %v", err)
	}

	if len(record.Content) != len(`{"resourceType":"StructureDefinition"}`) {
		t.Error("the second seed rewrote a row it was not asked to")
	}
}

// TestAChangedBundleIsSeededAgain, and takes the old rows with it: an upgrade
// that dropped a definition would otherwise leave the old one being served.
func TestAChangedBundleIsSeededAgain(t *testing.T) {
	store, db := canonicalStore(t)

	if _, _, err := store.Seed(t.Context(), seededAt); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// An install that last seeded something else, and a definition from that
	// something else still sitting in the table.
	if _, err := db.ExecContext(t.Context(),
		"UPDATE canonical_seed SET digest = 'an earlier bundle'"); err != nil {
		t.Fatalf("age the marker: %v", err)
	}

	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO canonical_resource (res_type, res_id, url, version, content)"+
			" VALUES ('StructureDefinition', 'SomethingWithdrawn',"+
			" 'http://example.test/withdrawn', '0.1', '{}')"); err != nil {
		t.Fatalf("seed a withdrawn definition: %v", err)
	}

	seeded, changed, err := store.Seed(t.Context(), seededAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("reseed: %v", err)
	}

	if !changed || seeded.Digest != conformance.Digest() {
		t.Errorf("the reseed reported %+v (changed=%v)", seeded, changed)
	}

	if _, found, err := store.Read(t.Context(), "StructureDefinition", "SomethingWithdrawn"); found || err != nil {
		t.Errorf("a withdrawn definition is still held: found=%v (%v)", found, err)
	}

	if held := heldCount(t, db); held != seeded.Held {
		t.Errorf("%d rows for %d definitions", held, seeded.Held)
	}
}

// TestAnInterruptedSeedLeavesNothingBehind. A seed half-done beside a marker
// saying it is done is the one state nothing would ever correct, so the two
// commit together or neither does.
func TestAnInterruptedSeedLeavesNothingBehind(t *testing.T) {
	store, db := canonicalStore(t)

	// The marker table still reads — the seed asks what is held before it does
	// anything — and refuses to be written. Every definition is written first,
	// so this fails at the last statement with the rows already inserted, which
	// is the interruption worth asking about.
	if _, err := db.ExecContext(t.Context(),
		"CREATE TRIGGER canonical_seed_refuses BEFORE INSERT ON canonical_seed"+
			" BEGIN SELECT RAISE(ABORT, 'the marker cannot be written'); END"); err != nil {
		t.Fatalf("make the marker unwritable: %v", err)
	}

	if _, _, err := store.Seed(t.Context(), seededAt); err == nil {
		t.Fatal("a seed that could not record itself reported success")
	}

	if held := heldCount(t, db); held != 0 {
		t.Errorf("%d definitions were left behind by a seed that did not finish", held)
	}
}

// TestReadingWhatWasNeverSeededFindsNothing, rather than reporting an error a
// caller would have to tell apart from a failure.
func TestReadingWhatWasNeverSeededFindsNothing(t *testing.T) {
	store, _ := canonicalStore(t)

	if _, found, err := store.Read(t.Context(), "StructureDefinition", "Observation"); found || err != nil {
		t.Errorf("an unseeded install answered found=%v (%v)", found, err)
	}

	if held, err := store.Holding(t.Context()); err != nil || held.Digest != "" {
		t.Errorf("it reports holding %+v (%v)", held, err)
	}
}

// TestTwoProcessesSeedingTogetherSeedOnce. Replicas start together, and both
// find an empty install: what keeps that from being two seeds racing is that
// each is one transaction, so whichever arrives second either waits or finds
// the marker already current.
func TestTwoProcessesSeedingTogetherSeedOnce(t *testing.T) {
	store, db := canonicalStore(t)

	var (
		wait    sync.WaitGroup
		mutex   sync.Mutex
		changed int
		failed  []error
	)

	for range 4 {
		wait.Add(1)

		go func() {
			defer wait.Done()

			_, seeded, err := store.Seed(context.Background(), seededAt)

			mutex.Lock()
			defer mutex.Unlock()

			if err != nil {
				failed = append(failed, err)
			}

			if seeded {
				changed++
			}
		}()
	}

	wait.Wait()

	if len(failed) != 0 {
		t.Fatalf("a concurrent seed failed: %v", failed)
	}

	if changed == 0 {
		t.Error("four processes seeded nothing at all")
	}

	// However many wrote, the install holds one copy of each definition.
	held, err := store.Holding(t.Context())
	if err != nil {
		t.Fatalf("read what was seeded: %v", err)
	}

	if count := heldCount(t, db); count != held.Held {
		t.Errorf("%d rows for the %d the marker names", count, held.Held)
	}

	if held.Digest != conformance.Digest() {
		t.Errorf("the install holds %q", held.Digest)
	}
}
