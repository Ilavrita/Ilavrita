package pocketbase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/terminology"
)

var importedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

const aTable = `"LOINC_NUM","LONG_COMMON_NAME","SHORTNAME","STATUS"
"99991-1","Glucose in Serum or Plasma","Gluc","ACTIVE"
"99992-2","A measurement nobody makes any more","Old","DEPRECATED"
`

func aRelease(table string) terminology.Release {
	return terminology.NewLOINCRelease(strings.NewReader(table), "2.77")
}

// TestAnImportedSystemIsLookedUpLocally, which is the whole point of importing
// one: a deployment that loaded LOINC can resolve a LOINC code without asking
// anybody.
func TestAnImportedSystemIsLookedUpLocally(t *testing.T) {
	_, db := newStore(t)
	store := NewTerminologyStore(db)

	held, err := store.Import(t.Context(), aRelease(aTable), "Loinc.csv", importedAt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if held.Held != 2 || held.Version != "2.77" {
		t.Errorf("it loaded %+v", held)
	}

	concept, found, err := store.Lookup(t.Context(), terminology.LOINC, "99991-1")
	if err != nil || !found {
		t.Fatalf("look up a loaded code: %v (found=%v)", err, found)
	}

	if concept.Display != "Glucose in Serum or Plasma" || !concept.Active {
		t.Errorf("it resolves to %+v", concept)
	}

	// A retired code still resolves. A resource already carrying it would
	// otherwise become unreadable.
	if retired, found, _ := store.Lookup(t.Context(), terminology.LOINC, "99992-2"); !found ||
		retired.Active {
		t.Errorf("a retired code reads found=%v %+v", found, retired)
	}
}

// TestNotHoldingASystemIsADifferentAnswerFromNotHoldingACode. "I do not hold
// LOINC" and "LOINC has no such code" mean different things to whoever asked,
// and a directory that conflated them would tell a client their code is wrong
// when it is the install that is empty.
func TestNotHoldingASystemIsADifferentAnswerFromNotHoldingACode(t *testing.T) {
	_, db := newStore(t)
	store := NewTerminologyStore(db)

	if _, loaded, err := store.Holds(t.Context(), terminology.LOINC); loaded || err != nil {
		t.Errorf("an empty install holds LOINC: %v", err)
	}

	if _, found, err := store.Lookup(t.Context(), terminology.LOINC, "99991-1"); found || err != nil {
		t.Errorf("an empty install resolved a code: %v", err)
	}

	if _, err := store.Import(t.Context(), aRelease(aTable), "Loinc.csv", importedAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	if _, loaded, _ := store.Holds(t.Context(), terminology.LOINC); !loaded {
		t.Error("a loaded install does not hold LOINC")
	}

	// Loaded, and this code is genuinely not in it.
	if _, found, _ := store.Lookup(t.Context(), terminology.LOINC, "00000-0"); found {
		t.Error("a code nobody loaded resolved")
	}
}

// TestAReleaseReplacesTheOneBeforeIt. A release states what a system is at a
// point in time; merging would leave codes from an older one beside it with
// nothing to say which release they came from.
func TestAReleaseReplacesTheOneBeforeIt(t *testing.T) {
	_, db := newStore(t)
	store := NewTerminologyStore(db)

	if _, err := store.Import(t.Context(), aRelease(aTable), "Loinc.csv", importedAt); err != nil {
		t.Fatalf("the first import: %v", err)
	}

	const next = `"LOINC_NUM","LONG_COMMON_NAME","SHORTNAME","STATUS"
"99991-1","Glucose in Serum or Plasma, renamed","Gluc","ACTIVE"
"99994-4","Something new","New","ACTIVE"
`

	held, err := store.Import(t.Context(),
		terminology.NewLOINCRelease(strings.NewReader(next), "2.78"),
		"Loinc.csv", importedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("the second import: %v", err)
	}

	if held.Held != 2 || held.Version != "2.78" {
		t.Errorf("the second release loaded %+v", held)
	}

	// The withdrawn code is gone, the renamed one is renamed, the new one is in.
	if _, found, _ := store.Lookup(t.Context(), terminology.LOINC, "99992-2"); found {
		t.Error("a code the new release does not carry is still held")
	}

	if got, _, _ := store.Lookup(t.Context(), terminology.LOINC, "99991-1"); !strings.HasSuffix(
		got.Display, "renamed") {
		t.Errorf("the renamed code still reads %q", got.Display)
	}

	if _, found, _ := store.Lookup(t.Context(), terminology.LOINC, "99994-4"); !found {
		t.Error("a code the new release carries is not held")
	}
}

// TestAnImportIsRecordedAsASuperJob, against the table it wrote to, so an
// operator can see which release is loaded and when it arrived.
func TestAnImportIsRecordedAsASuperJob(t *testing.T) {
	_, db := newStore(t)

	if _, err := NewTerminologyStore(db).Import(
		t.Context(), aRelease(aTable), "Loinc.csv", importedAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	ran, err := JobsOn(t.Context(), db, "terminology_concept", 10)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if len(ran) != 1 {
		t.Fatalf("the import recorded %d runs", len(ran))
	}

	if !strings.Contains(ran[0].Fingerprint, "2.77") || ran[0].Outcome != "applied" {
		t.Errorf("it is recorded as %+v", ran[0])
	}
}

// TestAnImportThatFailsPartWayLeavesNothing. A code system half loaded answers
// "no such code" for everything it did not reach, which is worse than answering
// "I do not hold this system".
func TestAnImportThatFailsPartWayLeavesNothing(t *testing.T) {
	_, db := newStore(t)
	store := NewTerminologyStore(db)

	if _, err := store.Import(t.Context(), aRelease(aTable), "Loinc.csv", importedAt); err != nil {
		t.Fatalf("the first import: %v", err)
	}

	// A release that reads for a while and then cannot be read at all: a quoted
	// field nothing closes, which is what a download cut short looks like.
	const truncated = "\"LOINC_NUM\",\"LONG_COMMON_NAME\",\"SHORTNAME\",\"STATUS\"\n" +
		"\"99995-5\",\"Fine\",\"F\",\"ACTIVE\"\n" +
		"\"99996-6\",\"Never closed"

	if _, err := store.Import(t.Context(),
		terminology.NewLOINCRelease(strings.NewReader(truncated), "2.79"),
		"Loinc.csv", importedAt); err == nil {
		t.Fatal("a release that could not be read was imported")
	}

	// The install still holds exactly what it held before.
	held, loaded, err := store.Holds(t.Context(), terminology.LOINC)
	if err != nil || !loaded {
		t.Fatalf("the earlier release is gone: %v (loaded=%v)", err, loaded)
	}

	if held.Version != "2.77" || held.Held != 2 {
		t.Errorf("the install holds %+v", held)
	}

	if _, found, _ := store.Lookup(t.Context(), terminology.LOINC, "99995-5"); found {
		t.Error("a code from the failed import was left behind")
	}
}

// TestSystemsListsWhatIsLoaded, which is what an operator asks before trusting
// a lookup.
func TestSystemsListsWhatIsLoaded(t *testing.T) {
	_, db := newStore(t)
	store := NewTerminologyStore(db)

	if held, err := store.Systems(context.Background()); err != nil || len(held) != 0 {
		t.Fatalf("an empty install lists %v (%v)", held, err)
	}

	if _, err := store.Import(t.Context(), aRelease(aTable), "Loinc.csv", importedAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	held, err := store.Systems(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatalf("it lists %v (%v)", held, err)
	}

	if held[0].System != terminology.LOINC || held[0].Source != "Loinc.csv" {
		t.Errorf("it lists %+v", held[0])
	}
}
