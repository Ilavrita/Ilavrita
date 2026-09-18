package pocketbase

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

const wholeObservation = `{"resourceType":"Observation","status":"final",` +
	`"note":[{"text":"private"}],"valueQuantity":{"value":7}}`

func mustStoreProjection(t *testing.T, elements ...string) storage.Projection {
	t.Helper()

	projection, err := storage.NewProjection(elements...)
	if err != nil {
		t.Fatalf("NewProjection(%v): %v", elements, err)
	}

	return projection
}

// projectedGrant narrows one Grant to the elements it returns.
func projectedGrant(action storage.Action, projection storage.Projection) storage.Grant {
	grant := fhirGrant("prj_a", "Observation", action)
	grant.Projection = &projection

	return grant
}

// membersOf reads back what a record actually carries.
func membersOf(t *testing.T, record storage.ResourceRecord) map[string]json.RawMessage {
	t.Helper()

	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(record.Content, &members); err != nil {
		t.Fatalf("decode the returned resource: %v", err)
	}

	return members
}

func assertMembers(t *testing.T, record storage.ResourceRecord, kept, withheld []string) {
	t.Helper()

	members := membersOf(t, record)

	for _, name := range kept {
		if _, present := members[name]; !present {
			t.Errorf("%s was withheld but should have been returned", name)
		}
	}

	for _, name := range withheld {
		if _, present := members[name]; present {
			t.Errorf("%s was returned but should have been withheld", name)
		}
	}
}

// TestAProjectedGrantReturnsOnlyWhatItNames.
func TestAProjectedGrantReturnsOnlyWhatItNames(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	scope := storage.NewScope(projectedGrant(storage.ActionRead, mustStoreProjection(t, "status")))

	record, err := store.Read(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	assertMembers(t, record, []string{"resourceType", "status"}, []string{"note", "valueQuantity"})
}

// TestAnUnprojectedGrantStillReturnsTheWholeResource, so the narrowing above is
// the projection acting and not something the store does to every read.
func TestAnUnprojectedGrantStillReturnsTheWholeResource(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	record, err := store.Read(t.Context(), fullScope("prj_a", "Observation"), key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	assertMembers(t, record, []string{"resourceType", "status", "note", "valueQuantity"}, nil)
}

// TestAGrantThatDoesNotReachTheRowDoesNotWidenIt. This is the whole reason
// coverage is worked out per row rather than taken from the Scope: a clinician
// holding one patient's chart in full and another's status alone must not read
// the second patient in full because the first Grant exists.
func TestAGrantThatDoesNotReachTheRowDoesNotWidenIt(t *testing.T) {
	store, db := newStore(t)

	mine := seedObservation(t, store, "obs-mine", wholeObservation)
	theirs := seedObservation(t, store, "obs-theirs", wholeObservation)

	own := storage.Compartment{Type: "Patient", ID: "pat-1"}
	other := storage.Compartment{Type: "Patient", ID: "pat-2"}

	attachCompartment(t, db, mine, own)
	attachCompartment(t, db, theirs, other)

	// One Grant reads the caller's own patient in full; the other reads a second
	// patient's status alone.
	whole := compartmentGrant("prj_a", "Observation", storage.ActionRead, own)

	narrow := compartmentGrant("prj_a", "Observation", storage.ActionRead, other)
	status := mustStoreProjection(t, "status")
	narrow.Projection = &status

	scope := storage.NewScope(whole, narrow)

	reachable, err := store.Read(t.Context(), scope, mine)
	if err != nil {
		t.Fatalf("read the caller's own patient: %v", err)
	}

	assertMembers(t, reachable, []string{"status", "note", "valueQuantity"}, nil)

	restricted, err := store.Read(t.Context(), scope, theirs)
	if err != nil {
		t.Fatalf("read the second patient: %v", err)
	}

	assertMembers(t, restricted, []string{"status"}, []string{"note", "valueQuantity"})
}

// TestAFilterDecidesCoverageForAProjectionToo, because a Grant the content
// fails does not reach the row and so has no say in how much of it is returned.
func TestAFilterDecidesCoverageForAProjectionToo(t *testing.T) {
	store, _ := newStore(t)

	final := seedObservation(t, store, "obs-final", wholeObservation)
	preliminary := seedObservation(t, store, "obs-prelim",
		`{"resourceType":"Observation","status":"preliminary","note":[{"text":"private"}]}`)

	// Whole resource for final readings; status alone for anything else.
	whole := fhirGrant("prj_a", "Observation", storage.ActionRead)
	onlyFinal := mustFilter(t, "status", storage.ComparatorEqual, "final")
	whole.Filter = &onlyFinal

	narrow := projectedGrant(storage.ActionRead, mustStoreProjection(t, "status"))

	scope := storage.NewScope(whole, narrow)

	full, err := store.Read(t.Context(), scope, final)
	if err != nil {
		t.Fatalf("read the final reading: %v", err)
	}

	assertMembers(t, full, []string{"status", "note", "valueQuantity"}, nil)

	partial, err := store.Read(t.Context(), scope, preliminary)
	if err != nil {
		t.Fatalf("read the preliminary reading: %v", err)
	}

	assertMembers(t, partial, []string{"status"}, []string{"note"})
}

// TestTwoProjectedGrantsReturnTheWidestOfThem over a real row.
func TestTwoProjectedGrantsReturnTheWidestOfThem(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	scope := storage.NewScope(
		projectedGrant(storage.ActionRead, mustStoreProjection(t, "status")),
		projectedGrant(storage.ActionRead, mustStoreProjection(t, "valueQuantity")),
	)

	record, err := store.Read(t.Context(), scope, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	assertMembers(t, record, []string{"status", "valueQuantity"}, []string{"note"})
}

// TestHistoryIsNarrowedByTheGrantsReachingEachVersion, checked against where
// that version landed rather than where the resource has since moved.
func TestHistoryIsNarrowedByTheGrantsReachingEachVersion(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	amended := storage.ResourceRecord{Key: key, Content: []byte(
		`{"resourceType":"Observation","status":"amended","note":[{"text":"private"}]}`)}

	if err := store.Update(t.Context(), fullScope("prj_a", "Observation"), amended, ""); err != nil {
		t.Fatalf("amend: %v", err)
	}

	scope := storage.NewScope(projectedGrant(storage.ActionHistory, mustStoreProjection(t, "status")))

	held, err := store.ListVersions(t.Context(), scope, key, wholeHistory())
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}

	versions := held.Records

	if len(versions) != 2 {
		t.Fatalf("history served %d versions, want both", len(versions))
	}

	for _, version := range versions {
		assertMembers(t, version, []string{"status"}, []string{"note", "valueQuantity"})
	}

	one, err := store.ReadVersion(t.Context(), scope, key, "1")
	if err != nil {
		t.Fatalf("read version 1: %v", err)
	}

	assertMembers(t, one, []string{"status"}, []string{"note", "valueQuantity"})
}

// TestACallerWhoReadsPartOfAResourceMayNotReplaceAllOfIt. An update replaces
// content wholesale, so such a caller would send back what they were shown and
// silently drop what their own policy withheld — data loss produced by an
// authorization rule rather than by anyone's intent.
func TestACallerWhoReadsPartOfAResourceMayNotReplaceAllOfIt(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	scope := storage.NewScope(
		projectedGrant(storage.ActionRead, mustStoreProjection(t, "status")),
		fhirGrant("prj_a", "Observation", storage.ActionWrite),
	)

	replacement := storage.ResourceRecord{Key: key, Content: []byte(
		`{"resourceType":"Observation","status":"amended"}`)}

	err := store.Update(t.Context(), scope, replacement, "")
	if !errors.Is(err, ErrPartialView) || !errors.Is(err, storage.ErrDenied) {
		t.Fatalf("err = %v, want a refusal that reads as both %v and %v", err, ErrPartialView, storage.ErrDenied)
	}

	// Nothing was written.
	stored, err := store.Read(t.Context(), fullScope("prj_a", "Observation"), key)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	assertMembers(t, stored, []string{"note", "valueQuantity"}, nil)
}

// TestACallerWhoReadsTheWholeResourceMayReplaceIt, so the refusal above is the
// projection acting and not a Scope that authorizes nothing.
func TestACallerWhoReadsTheWholeResourceMayReplaceIt(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	scope := storage.NewScope(
		projectedGrant(storage.ActionRead, mustStoreProjection(t, "status")),
		fhirGrant("prj_a", "Observation", storage.ActionRead),
		fhirGrant("prj_a", "Observation", storage.ActionWrite),
	)

	replacement := storage.ResourceRecord{Key: key, Content: []byte(
		`{"resourceType":"Observation","status":"amended"}`)}

	if err := store.Update(t.Context(), scope, replacement, ""); err != nil {
		t.Errorf("a caller holding an unprojected read grant could not replace: %v", err)
	}
}

// TestAWriteOnlyCallerIsNotBlind. Nothing showed them a partial resource to
// send back, so the guard does not apply and the read-back that renders the
// write answers for them.
func TestAWriteOnlyCallerIsNotBlind(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	scope := storage.NewScope(fhirGrant("prj_a", "Observation", storage.ActionWrite))

	replacement := storage.ResourceRecord{Key: key, Content: []byte(
		`{"resourceType":"Observation","status":"amended"}`)}

	if err := store.Update(t.Context(), scope, replacement, ""); err != nil {
		t.Errorf("a write-only caller was refused as blind: %v", err)
	}
}

// TestAProjectionThisStoreCannotApplyRefusesTheRead. Returning the whole
// resource because the restriction could not be read is the one outcome a
// projection exists to prevent.
func TestAProjectionThisStoreCannotApplyRefusesTheRead(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", wholeObservation)

	grant := fhirGrant("prj_a", "Observation", storage.ActionRead)
	grant.Projection = &storage.Projection{}

	if _, err := store.Read(t.Context(), storage.NewScope(grant), key); !errors.Is(err, storage.ErrEmptyProjection) {
		t.Errorf("err = %v, want %v", err, storage.ErrEmptyProjection)
	}
}

// TestAVersionIsNarrowedByWhereItLandedNotWhereTheResourceWent. A resource that
// has since moved to another patient must not hand over its older versions
// under the Grant covering where it is now: those versions were the first
// patient's, and how much of them is returned is that patient's Grant to decide.
func TestAVersionIsNarrowedByWhereItLandedNotWhereTheResourceWent(t *testing.T) {
	store, _ := newStore(t)

	first := storage.Compartment{Type: "Patient", ID: "pat-1"}
	second := storage.Compartment{Type: "Patient", ID: "pat-2"}

	key := observationKey("prj_a", "obs-moved")

	err := store.Create(t.Context(), fullScope("prj_a", "Observation"), storage.ResourceRecord{
		Key: key, Content: []byte(wholeObservation), Compartments: []storage.Compartment{first},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	err = store.Update(t.Context(), fullScope("prj_a", "Observation"), storage.ResourceRecord{
		Key:          key,
		Content:      []byte(wholeObservation),
		Compartments: []storage.Compartment{second},
	}, "")
	if err != nil {
		t.Fatalf("move the resource: %v", err)
	}

	// The first patient's Grant returns a status; the second patient's returns
	// everything. Which one applies to a version is decided by where that
	// version landed.
	narrow := compartmentGrant("prj_a", "Observation", storage.ActionHistory, first)
	status := mustStoreProjection(t, "status")
	narrow.Projection = &status

	whole := compartmentGrant("prj_a", "Observation", storage.ActionHistory, second)

	scope := storage.NewScope(narrow, whole)

	one, err := store.ReadVersion(t.Context(), scope, key, "1")
	if err != nil {
		t.Fatalf("read version 1: %v", err)
	}

	assertMembers(t, one, []string{"status"}, []string{"note", "valueQuantity"})

	two, err := store.ReadVersion(t.Context(), scope, key, "2")
	if err != nil {
		t.Fatalf("read version 2: %v", err)
	}

	assertMembers(t, two, []string{"status", "note", "valueQuantity"}, nil)
}
