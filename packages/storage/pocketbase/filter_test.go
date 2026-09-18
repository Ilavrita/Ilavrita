package pocketbase

import (
	"errors"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// The shapes FHIR allows for the same restriction. A rule's author writes
// "category.coding.code" once; whether an element repeats is the specification's
// business, not theirs, so every shape below must read the same way.
const (
	vitalArrays = `{"resourceType":"Observation","status":"final",` +
		`"category":[{"coding":[{"system":"s","code":"vital-signs"},{"code":"other"}]}],"class":{"code":"AMB"}}`
	vitalObjects = `{"resourceType":"Observation","status":"final",` +
		`"category":{"coding":{"code":"vital-signs"}},"class":{"code":"AMB"}}`
	labArrays = `{"resourceType":"Observation","status":"preliminary",` +
		`"category":[{"coding":[{"code":"laboratory"}]}],"class":{"code":"IMP"}}`
	noCategory   = `{"resourceType":"Observation","status":"final"}`
	flatCategory = `{"resourceType":"Observation","status":"final","category":"vital-signs"}`
	nullCategory = `{"resourceType":"Observation","status":"final","category":null}`
	repeatedLeaf = `{"resourceType":"Observation","status":["final","amended"]}`
)

func observationKey(project storage.ProjectID, id storage.LogicalID) storage.ResourceKey {
	return storage.ResourceKey{Project: project, Type: "Observation", ID: id}
}

// seedObservation writes one body under an unfiltered Scope, so what a filtered
// read finds is decided by the filter and never by what the write refused.
func seedObservation(
	t *testing.T, store *ResourceStore, id storage.LogicalID, body string,
) storage.ResourceKey {
	t.Helper()

	key := observationKey("prj_a", id)

	err := store.Create(t.Context(), fullScope("prj_a", "Observation"),
		storage.ResourceRecord{Key: key, Content: []byte(body)})
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}

	return key
}

func mustFilter(t *testing.T, path string, comparator storage.Comparator, values ...string) storage.Filter {
	t.Helper()

	filter, err := storage.NewFilter(path, comparator, values...)
	if err != nil {
		t.Fatalf("build filter %q: %v", path, err)
	}

	return filter
}

// filteredScope narrows every action on Observation by one filter.
func filteredScope(filter storage.Filter) storage.Scope {
	actions := []storage.Action{
		storage.ActionRead, storage.ActionWrite, storage.ActionDelete, storage.ActionHistory,
	}
	grants := make([]storage.Grant, 0, len(actions))

	for _, action := range actions {
		grant := fhirGrant("prj_a", "Observation", action)
		held := filter
		grant.Filter = &held
		grants = append(grants, grant)
	}

	return storage.NewScope(grants...)
}

// readable reports whether the Scope can read the key at all, folding "missing"
// and "refused" together the way the store does: a row outside a Scope reads as
// one that is not there.
func readable(t *testing.T, store *ResourceStore, scope storage.Scope, key storage.ResourceKey) bool {
	t.Helper()

	switch _, err := store.Read(t.Context(), scope, key); {
	case err == nil:
		return true
	case errors.Is(err, storage.ErrNotFound):
		return false
	default:
		t.Fatalf("read %s: %v", key.ID, err)

		return false
	}
}

// TestAFilterReadsThroughWhateverShapeFHIRChose is the traversal's whole point:
// category.coding.code crosses two arrays in one resource and none in the next,
// and one filter reads both. An author who had to know which shape a server
// stored would be writing a restriction against the data rather than the model.
func TestAFilterReadsThroughWhateverShapeFHIRChose(t *testing.T) {
	store, _ := newStore(t)

	arrays := seedObservation(t, store, "obs-arrays", vitalArrays)
	objects := seedObservation(t, store, "obs-objects", vitalObjects)
	lab := seedObservation(t, store, "obs-lab", labArrays)

	scope := filteredScope(mustFilter(t, "category.coding.code", storage.ComparatorEqual, "vital-signs"))

	for _, key := range []storage.ResourceKey{arrays, objects} {
		if !readable(t, store, scope, key) {
			t.Errorf("%s did not read through the filter", key.ID)
		}
	}

	if readable(t, store, scope, lab) {
		t.Error("a laboratory observation read through a vital-signs filter")
	}
}

// TestAResourceShapedUnlikeThePathFailsTheFilter, rather than failing the query.
// A body whose element is a bare string where the path expected an object is
// the shape that makes a naive traversal raise "malformed JSON" mid-statement,
// which would turn one odd resource into a refused read of every resource
// beside it.
func TestAResourceShapedUnlikeThePathFailsTheFilter(t *testing.T) {
	store, _ := newStore(t)

	odd := map[storage.LogicalID]struct {
		why  string
		body string
	}{
		"obs-flat": {"a scalar where an object was expected", flatCategory},
		"obs-null": {"an element that is null", nullCategory},
		"obs-none": {"an element that is absent", noCategory},
	}

	scope := filteredScope(mustFilter(t, "category.coding.code", storage.ComparatorEqual, "vital-signs"))

	for id, shape := range odd {
		if readable(t, store, scope, seedObservation(t, store, id, shape.body)) {
			t.Errorf("%s matched the filter", shape.why)
		}
	}

	// The odd rows above are in the same table, so a filtered read of a good one
	// proves the query survived them.
	good := seedObservation(t, store, "obs-good", vitalArrays)
	if !readable(t, store, scope, good) {
		t.Error("a matching resource was unreadable beside resources shaped unlike the path")
	}
}

// TestAFilterMatchesAnyValueOfARepeatingLeaf, because FHIR models several
// filterable elements as repeating and a restriction naming one of the values
// means the resource carries it, not that it carries only it.
func TestAFilterMatchesAnyValueOfARepeatingLeaf(t *testing.T) {
	store, _ := newStore(t)

	key := seedObservation(t, store, "obs-repeated", repeatedLeaf)
	scope := filteredScope(mustFilter(t, "status", storage.ComparatorEqual, "amended"))

	if !readable(t, store, scope, key) {
		t.Error("a repeating element did not match on one of its values")
	}
}

// TestMembershipAdmitsEveryValueItNames and nothing else.
func TestMembershipAdmitsEveryValueItNames(t *testing.T) {
	store, _ := newStore(t)

	vital := seedObservation(t, store, "obs-vital", vitalArrays)
	lab := seedObservation(t, store, "obs-lab", labArrays)

	scope := filteredScope(mustFilter(t, "category.coding.code",
		storage.ComparatorIn, "laboratory", "imaging"))

	if !readable(t, store, scope, lab) {
		t.Error("a named value did not match")
	}

	if readable(t, store, scope, vital) {
		t.Error("a value the filter does not name matched")
	}
}

// TestAFilterOnlyNarrows. This is the property POL-1 states and the one a
// filter must never violate: whatever a filtered Grant reaches, the same Grant
// without the filter reaches too.
func TestAFilterOnlyNarrows(t *testing.T) {
	store, _ := newStore(t)

	bodies := map[storage.LogicalID]string{
		"obs-1": vitalArrays, "obs-2": vitalObjects, "obs-3": labArrays,
		"obs-4": noCategory, "obs-5": flatCategory, "obs-6": nullCategory, "obs-7": repeatedLeaf,
	}

	keys := make([]storage.ResourceKey, 0, len(bodies))
	for id, body := range bodies {
		keys = append(keys, seedObservation(t, store, id, body))
	}

	unfiltered := fullScope("prj_a", "Observation")

	filters := []storage.Filter{
		mustFilter(t, "status", storage.ComparatorEqual, "final"),
		mustFilter(t, "category.coding.code", storage.ComparatorIn, "vital-signs", "laboratory"),
		mustFilter(t, "class.code", storage.ComparatorEqual, "AMB"),
		mustFilter(t, "status", storage.ComparatorEqual, "nothing-holds-this"),
	}

	for _, filter := range filters {
		narrowed := filteredScope(filter)

		for _, key := range keys {
			if readable(t, store, narrowed, key) && !readable(t, store, unfiltered, key) {
				t.Errorf("filter %q widened the grant at %s", filter, key.ID)
			}
		}
	}
}

// TestAFilterComposesWithACompartment. They restrict different things, so a
// Grant carrying both reaches what satisfies both.
func TestAFilterComposesWithACompartment(t *testing.T) {
	store, db := newStore(t)

	mine := seedObservation(t, store, "obs-mine", vitalArrays)
	theirs := seedObservation(t, store, "obs-theirs", vitalArrays)

	compartment := storage.Compartment{Type: "Patient", ID: "pat-1"}
	attachCompartment(t, db, mine, compartment)
	attachCompartment(t, db, theirs, storage.Compartment{Type: "Patient", ID: "pat-2"})

	grant := compartmentGrant("prj_a", "Observation", storage.ActionRead, compartment)
	filter := mustFilter(t, "category.coding.code", storage.ComparatorEqual, "vital-signs")
	grant.Filter = &filter
	scope := storage.NewScope(grant)

	if !readable(t, store, scope, mine) {
		t.Error("a resource satisfying both restrictions was unreadable")
	}

	if readable(t, store, scope, theirs) {
		t.Error("a resource outside the compartment read through a matching filter")
	}
}

// TestAFilterIsCheckedAgainstTheVersionBeingReturned, not against whatever the
// current row now says. A resource amended out of a Grant's reach must not hand
// over the versions it was once inside, and one amended into reach must not
// retroactively disclose the versions it was outside.
func TestAFilterIsCheckedAgainstTheVersionBeingReturned(t *testing.T) {
	store, _ := newStore(t)

	key := seedObservation(t, store, "obs-amended", vitalArrays)

	// The amendment is made under an unfiltered Scope, so what the filtered
	// reader sees afterwards is decided only by the filter.
	amended := `{"resourceType":"Observation","status":"entered-in-error",` +
		`"category":[{"coding":[{"code":"laboratory"}]}]}`

	err := store.Update(t.Context(), fullScope("prj_a", "Observation"),
		storage.ResourceRecord{Key: key, Content: []byte(amended)}, "")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}

	scope := filteredScope(mustFilter(t, "category.coding.code", storage.ComparatorEqual, "vital-signs"))

	if readable(t, store, scope, key) {
		t.Error("the amended current row read through a filter it no longer matches")
	}

	held, err := store.ListVersions(t.Context(), scope, key, wholeHistory())
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}

	versions := held.Records

	if len(versions) != 1 {
		t.Fatalf("history returned %d versions, want only the one the filter matches", len(versions))
	}

	if versions[0].Version != "1" {
		t.Errorf("history returned version %s, want the one that matched", versions[0].Version)
	}
}

// TestAFilteredGrantCannotWriteOutsideItsFilter. The same predicate a read
// compiles is the one a write requires, so a row the caller cannot read is one
// they cannot change either.
func TestAFilteredGrantCannotWriteOutsideItsFilter(t *testing.T) {
	store, _ := newStore(t)

	lab := seedObservation(t, store, "obs-lab", labArrays)
	scope := filteredScope(mustFilter(t, "category.coding.code", storage.ComparatorEqual, "vital-signs"))

	replacement := storage.ResourceRecord{Key: lab, Content: []byte(vitalArrays)}

	if err := store.Update(t.Context(), scope, replacement, ""); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("update outside the filter: err = %v, want %v", err, storage.ErrNotFound)
	}

	if err := store.Delete(t.Context(), scope, lab, ""); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("delete outside the filter: err = %v, want %v", err, storage.ErrNotFound)
	}

	// A row the filter does reach is writable, so the refusals above are the
	// filter's doing and not a Scope that authorizes nothing.
	vital := seedObservation(t, store, "obs-vital", vitalArrays)
	if err := store.Delete(t.Context(), scope, vital, ""); err != nil {
		t.Errorf("delete inside the filter: %v", err)
	}
}

// TestAFilterThisBackendCannotCompileRefusesTheRequest. Compiling the Grant
// without it would answer under a Scope wider than the caller holds, which is
// the one outcome a filter exists to prevent — so the request fails loudly
// instead.
func TestAFilterThisBackendCannotCompileRefusesTheRequest(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", vitalArrays)

	broken := map[string]storage.Filter{
		"a filter nobody built": {},
	}

	for name, filter := range broken {
		grant := fhirGrant("prj_a", "Observation", storage.ActionRead)
		held := filter
		grant.Filter = &held
		scope := storage.NewScope(grant)

		if _, err := store.Read(t.Context(), scope, key); !errors.Is(err, ErrUncompilableFilter) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrUncompilableFilter)
		}
	}
}

// TestOneUncompilableGrantFailsTheWholeRequest. Keeping the arms that did
// compile would answer under a narrower Scope than the caller holds, and
// dropping the failing arm's restriction would answer under a wider one;
// neither is an answer this store may give.
func TestOneUncompilableGrantFailsTheWholeRequest(t *testing.T) {
	store, _ := newStore(t)
	key := seedObservation(t, store, "obs-1", vitalArrays)

	good := fhirGrant("prj_a", "Observation", storage.ActionRead)
	sound := mustFilter(t, "status", storage.ComparatorEqual, "final")
	good.Filter = &sound

	bad := fhirGrant("prj_a", "Observation", storage.ActionRead)
	bad.Filter = &storage.Filter{}

	if _, err := store.Read(t.Context(), storage.NewScope(good, bad), key); !errors.Is(err, ErrUncompilableFilter) {
		t.Errorf("err = %v, want %v", err, ErrUncompilableFilter)
	}
}

// TestAFilterBindsItsPathRatherThanSplicingIt. The path reaches SQLite as a
// value, so the statement's shape cannot depend on what a policy row holds.
func TestAFilterBindsItsPathRatherThanSplicingIt(t *testing.T) {
	filter := mustFilter(t, "category.coding.code", storage.ComparatorIn, "vital-signs", "laboratory")

	text, args, err := filterPredicate("r.content", &filter)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for _, segment := range filter.Path() {
		if strings.Contains(text, segment) {
			t.Errorf("the statement carries the element name %q instead of binding it: %s", segment, text)
		}
	}

	for _, value := range filter.Values() {
		if strings.Contains(text, value) {
			t.Errorf("the statement carries the value %q instead of binding it: %s", value, text)
		}
	}

	if placeholders := strings.Count(text, "?"); placeholders != len(args) {
		t.Errorf("the statement has %d placeholders and %d arguments: %s", placeholders, len(args), text)
	}
}

// TestAFilteredCallerCannotWriteAResourceItsFilterWouldHide. Writing a row into
// a hole is refused for the reason a resource landing in no compartment is: the
// caller could not read back what it just wrote, so the write leaves a row
// nobody who made it can address.
func TestAFilteredCallerCannotWriteAResourceItsFilterWouldHide(t *testing.T) {
	store, _ := newStore(t)
	scope := filteredScope(mustFilter(t, "status", storage.ComparatorEqual, "final"))

	hidden := storage.ResourceRecord{
		Key:     observationKey("prj_a", "obs-hidden"),
		Content: []byte(`{"resourceType":"Observation","status":"preliminary"}`),
	}

	if err := store.Create(t.Context(), scope, hidden); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a create outside the caller's own filter: err = %v, want %v", err, storage.ErrDenied)
	}

	// The same write inside the filter succeeds, so the refusal above is the
	// filter's and not a Scope that authorizes nothing.
	visible := storage.ResourceRecord{
		Key:     observationKey("prj_a", "obs-visible"),
		Content: []byte(`{"resourceType":"Observation","status":"final"}`),
	}

	if err := store.Create(t.Context(), scope, visible); err != nil {
		t.Fatalf("a create inside the caller's own filter: %v", err)
	}

	if !readable(t, store, scope, visible.Key) {
		t.Error("a resource the caller created is not one it can read")
	}
}

// TestAFilteredCallerCannotAmendAResourceOutOfItsOwnView. The row is reachable
// now and the caller may write it; what is refused is the content, because
// afterwards the same caller could neither read nor correct it.
func TestAFilteredCallerCannotAmendAResourceOutOfItsOwnView(t *testing.T) {
	store, _ := newStore(t)

	key := seedObservation(t, store, "obs-1", `{"resourceType":"Observation","status":"final"}`)
	scope := filteredScope(mustFilter(t, "status", storage.ComparatorEqual, "final"))

	if !readable(t, store, scope, key) {
		t.Fatal("the caller cannot reach the row it is about to amend")
	}

	away := storage.ResourceRecord{
		Key:     key,
		Content: []byte(`{"resourceType":"Observation","status":"entered-in-error"}`),
	}

	if err := store.Update(t.Context(), scope, away, ""); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("an amendment out of the caller's own filter: err = %v, want %v", err, storage.ErrDenied)
	}

	// The row is untouched, so the refusal happened before the write and not
	// after it.
	if !readable(t, store, scope, key) {
		t.Error("the refused amendment still moved the row out of view")
	}

	staying := storage.ResourceRecord{
		Key:     key,
		Content: []byte(`{"resourceType":"Observation","status":"final","note":[{"text":"amended"}]}`),
	}

	if err := store.Update(t.Context(), scope, staying, ""); err != nil {
		t.Errorf("an amendment staying inside the filter: %v", err)
	}
}

// TestACreateTellsANarrowedCallerNothingAboutATakenID. A filtered caller can no
// more tell a taken id from one it was never allowed to name than a
// compartment-confined one can, so the create answers the same way for both.
func TestACreateTellsANarrowedCallerNothingAboutATakenID(t *testing.T) {
	store, _ := newStore(t)

	taken := seedObservation(t, store, "obs-taken", `{"resourceType":"Observation","status":"preliminary"}`)

	scope := filteredScope(mustFilter(t, "status", storage.ComparatorEqual, "final"))
	attempt := storage.ResourceRecord{
		Key:     taken,
		Content: []byte(`{"resourceType":"Observation","status":"final"}`),
	}

	if err := store.Create(t.Context(), scope, attempt); !errors.Is(err, storage.ErrDenied) {
		t.Errorf("a create over an unreadable row: err = %v, want %v", err, storage.ErrDenied)
	}
}

// TestTheWriteCheckReadsTheFilterTheSameWayAReadDoes. The two run the same
// compiled predicate against the same body, so a shape one admits is one the
// other admits: a resource a caller is allowed to write is one it can read.
func TestTheWriteCheckReadsTheFilterTheSameWayAReadDoes(t *testing.T) {
	store, _ := newStore(t)

	bodies := map[storage.LogicalID]string{
		"obs-1": vitalArrays, "obs-2": vitalObjects, "obs-3": labArrays,
		"obs-4": noCategory, "obs-5": flatCategory, "obs-6": nullCategory, "obs-7": repeatedLeaf,
	}

	filters := []storage.Filter{
		mustFilter(t, "status", storage.ComparatorEqual, "final"),
		mustFilter(t, "category.coding.code", storage.ComparatorIn, "vital-signs", "laboratory"),
		mustFilter(t, "class.code", storage.ComparatorEqual, "AMB"),
	}

	for _, filter := range filters {
		scope := filteredScope(filter)

		for id, body := range bodies {
			key := observationKey("prj_a", id+"-"+storage.LogicalID(filter.Path()[0]))
			record := storage.ResourceRecord{Key: key, Content: []byte(body)}

			written := store.Create(t.Context(), scope, record) == nil
			if written != readable(t, store, scope, key) {
				t.Errorf("filter %q at %s: writable = %v but readable = %v",
					filter, id, written, !written)
			}
		}
	}
}
