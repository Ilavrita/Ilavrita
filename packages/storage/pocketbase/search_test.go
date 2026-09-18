package pocketbase

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// searchScope authorizes a search of one type across the whole Project.
func searchScope(resourceType storage.ResourceType) storage.Scope {
	return storage.NewScope(fhirGrant("prj_a", resourceType, storage.ActionSearch))
}

// found runs one query and returns the ids it matched, in the order the page
// carries them.
func found(t *testing.T, store *ResourceStore, scope storage.Scope, resourceType, query string) []string {
	t.Helper()

	return page(t, store, scope, resourceType, query).ids
}

type searchResult struct {
	ids     []string
	more    bool
	total   int
	counted bool
	cursor  storage.LogicalID
}

func page(
	t *testing.T, store *ResourceStore, scope storage.Scope, resourceType, query string,
) searchResult {
	t.Helper()

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}

	plan, err := search.Parse(storage.ResourceType(resourceType), values)
	if err != nil {
		t.Fatalf("plan %q: %v", query, err)
	}

	held, err := store.Search(t.Context(), scope, plan)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}

	ids := make([]string, 0, len(held.Records))
	for _, record := range held.Records {
		ids = append(ids, string(record.Key.ID))
	}

	return searchResult{
		ids: ids, more: held.More, total: held.Total,
		counted: held.Counted, cursor: held.Cursor(),
	}
}

// seedTyped writes one resource of any type under an unfiltered Scope.
func seedTyped(
	t *testing.T, store *ResourceStore, resourceType storage.ResourceType, id storage.LogicalID, body string,
) storage.ResourceKey {
	t.Helper()

	key := storage.ResourceKey{Project: "prj_a", Type: resourceType, ID: id}

	err := store.Create(t.Context(), fullScope("prj_a", resourceType),
		storage.ResourceRecord{Key: key, Content: []byte(body)})
	if err != nil {
		t.Fatalf("seed %s/%s: %v", resourceType, id, err)
	}

	return key
}

// TestASearchMatchesOnAProjectedToken.
func TestASearchMatchesOnAProjectedToken(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-final", `{"resourceType":"Observation","status":"final"}`)
	seedTyped(t, store, "Observation", "obs-prelim", `{"resourceType":"Observation","status":"preliminary"}`)

	if got := found(t, store, searchScope("Observation"), "Observation", "status=final"); !slices.Equal(got, []string{"obs-final"}) {
		t.Errorf("status=final matched %v", got)
	}

	if got := found(t, store, searchScope("Observation"), "Observation",
		"status=final,preliminary"); len(got) != 2 {
		t.Errorf("two alternatives matched %v", got)
	}
}

// TestCriteriaAreAllRequiredAndValuesAreAlternatives, which is FHIR's own
// reading of a repeated parameter versus a comma-separated one.
func TestCriteriaAreAllRequiredAndValuesAreAlternatives(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-1",
		`{"resourceType":"Observation","status":"final","category":[{"coding":[{"code":"vital-signs"}]}]}`)
	seedTyped(t, store, "Observation", "obs-2",
		`{"resourceType":"Observation","status":"final","category":[{"coding":[{"code":"laboratory"}]}]}`)

	scope := searchScope("Observation")

	if got := found(t, store, scope, "Observation",
		"status=final&category=vital-signs"); !slices.Equal(got, []string{"obs-1"}) {
		t.Errorf("two criteria matched %v, want only what satisfies both", got)
	}

	if got := found(t, store, scope, "Observation",
		"status=final&category=vital-signs,laboratory"); len(got) != 2 {
		t.Errorf("alternatives within one criterion matched %v", got)
	}

	if got := found(t, store, scope, "Observation",
		"status=amended&category=vital-signs"); len(got) != 0 {
		t.Errorf("a criterion nothing satisfies matched %v", got)
	}
}

// TestAQualifiedTokenMatchesOnlyItsOwnSystem, so a search cannot pick up
// another coding system's identical code.
func TestAQualifiedTokenMatchesOnlyItsOwnSystem(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-loinc",
		`{"resourceType":"Observation","category":[{"coding":[{"system":"http://loinc.org","code":"vs"}]}]}`)
	seedTyped(t, store, "Observation", "obs-local",
		`{"resourceType":"Observation","category":[{"coding":[{"system":"http://local","code":"vs"}]}]}`)

	scope := searchScope("Observation")

	got := found(t, store, scope, "Observation", "category=http%3A%2F%2Floinc.org%7Cvs")
	if !slices.Equal(got, []string{"obs-loinc"}) {
		t.Errorf("a qualified token matched %v", got)
	}

	if bare := found(t, store, scope, "Observation", "category=vs"); len(bare) != 2 {
		t.Errorf("a bare token matched %v, want whatever system recorded it", bare)
	}
}

// TestAStringMatchesAPrefixWhateverTheCapitalisation.
func TestAStringMatchesAPrefixWhateverTheCapitalisation(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Patient", "pat-1", `{"resourceType":"Patient","name":[{"family":"MacDonald"}]}`)
	seedTyped(t, store, "Patient", "pat-2", `{"resourceType":"Patient","name":[{"family":"Smith"}]}`)

	scope := searchScope("Patient")

	for _, asked := range []string{"family=mac", "family=MAC", "family=MacDon"} {
		if got := found(t, store, scope, "Patient", asked); !slices.Equal(got, []string{"pat-1"}) {
			t.Errorf("%s matched %v", asked, got)
		}
	}

	// Anchored: a search never scans for a value inside another.
	if got := found(t, store, scope, "Patient", "family=onald"); len(got) != 0 {
		t.Errorf("a string matched inside a value: %v", got)
	}
}

// TestAWildcardInAValueIsNotAWildcard, so nothing a caller types changes what
// the pattern means.
func TestAWildcardInAValueIsNotAWildcard(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Patient", "pat-1", `{"resourceType":"Patient","name":[{"family":"Smith"}]}`)

	if got := found(t, store, searchScope("Patient"), "Patient", "family=%25"); len(got) != 0 {
		t.Errorf("a percent sign matched every name: %v", got)
	}

	if got := found(t, store, searchScope("Patient"), "Patient", "family=_mith"); len(got) != 0 {
		t.Errorf("an underscore matched a single character: %v", got)
	}
}

// TestAReferenceMatchesTheRowItNames.
func TestAReferenceMatchesTheRowItNames(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-1",
		`{"resourceType":"Observation","subject":{"reference":"Patient/pat-1"}}`)
	seedTyped(t, store, "Observation", "obs-2",
		`{"resourceType":"Observation","subject":{"reference":"Patient/pat-2"}}`)

	scope := searchScope("Observation")

	if got := found(t, store, scope, "Observation", "subject=Patient%2Fpat-1"); !slices.Equal(got, []string{"obs-1"}) {
		t.Errorf("subject matched %v", got)
	}

	// patient is the same element under another name, which is how R4 spells it.
	if got := found(t, store, scope, "Observation", "patient=Patient%2Fpat-2"); !slices.Equal(got, []string{"obs-2"}) {
		t.Errorf("patient matched %v", got)
	}
}

// TestADateMatchesTheSpanItNames.
func TestADateMatchesTheSpanItNames(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Patient", "pat-early", `{"resourceType":"Patient","birthDate":"1980-07-04"}`)
	seedTyped(t, store, "Patient", "pat-late", `{"resourceType":"Patient","birthDate":"1999-01-01"}`)

	scope := searchScope("Patient")

	cases := map[string][]string{
		"birthdate=1980-07-04":   {"pat-early"},
		"birthdate=ge1999-01-01": {"pat-late"},
		"birthdate=le1980-07-04": {"pat-early"},
		"birthdate=gt1980-07-04": {"pat-late"},
		"birthdate=lt1999-01-01": {"pat-early"},
		"birthdate=1985-01-01":   {},
	}

	for asked, want := range cases {
		got := found(t, store, scope, "Patient", asked)
		if len(got) != len(want) || (len(want) > 0 && got[0] != want[0]) {
			t.Errorf("%s matched %v, want %v", asked, got, want)
		}
	}
}

// TestTheUniversalParametersAnswerFromTheRowItself, so nothing is projected
// twice and nothing can fall out of step with the row.
func TestTheUniversalParametersAnswerFromTheRowItself(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation"}`)
	seedTyped(t, store, "Observation", "obs-2", `{"resourceType":"Observation"}`)

	if got := found(t, store, searchScope("Observation"), "Observation", "_id=obs-1"); !slices.Equal(got, []string{"obs-1"}) {
		t.Errorf("_id matched %v", got)
	}

	if got := found(t, store, searchScope("Observation"), "Observation", "_id=obs-1,obs-2"); len(got) != 2 {
		t.Errorf("two ids matched %v", got)
	}
}

// TestADeletedResourceIsNotSearchable. A tombstone is excluded by the row it
// joins rather than by clearing its index, so this is what proves the exclusion
// actually happens.
func TestADeletedResourceIsNotSearchable(t *testing.T) {
	store, _ := newStore(t)

	key := seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"final"}`)

	if got := found(t, store, searchScope("Observation"), "Observation", "status=final"); len(got) != 1 {
		t.Fatalf("the resource was not searchable before the delete: %v", got)
	}

	if err := store.Delete(t.Context(), fullScope("prj_a", "Observation"), key, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if got := found(t, store, searchScope("Observation"), "Observation", "status=final"); len(got) != 0 {
		t.Errorf("a tombstone was searchable: %v", got)
	}
}

// TestAnUpdateReindexesTheResource. An index maintained on create alone answers
// searches from whatever the resource used to say.
func TestAnUpdateReindexesTheResource(t *testing.T) {
	store, _ := newStore(t)

	key := seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"preliminary"}`)

	amended := storage.ResourceRecord{Key: key, Content: []byte(`{"resourceType":"Observation","status":"final"}`)}
	if err := store.Update(t.Context(), fullScope("prj_a", "Observation"), amended, ""); err != nil {
		t.Fatalf("amend: %v", err)
	}

	scope := searchScope("Observation")

	if got := found(t, store, scope, "Observation", "status=final"); len(got) != 1 {
		t.Errorf("the amended status is not searchable: %v", got)
	}

	if got := found(t, store, scope, "Observation", "status=preliminary"); len(got) != 0 {
		t.Errorf("the status it no longer holds still matches: %v", got)
	}
}

// TestASearchIsBoundedByTheScopeItRunsUnder. The criteria are added to the
// authorization predicate and nothing is taken out of it (SRC-3).
func TestASearchIsBoundedByTheScopeItRunsUnder(t *testing.T) {
	store, db := newStore(t)

	mine := seedTyped(t, store, "Observation", "obs-mine", `{"resourceType":"Observation","status":"final"}`)
	seedTyped(t, store, "Observation", "obs-theirs", `{"resourceType":"Observation","status":"final"}`)

	own := storage.Compartment{Type: "Patient", ID: "pat-1"}
	attachCompartment(t, db, mine, own)

	confined := storage.NewScope(compartmentGrant("prj_a", "Observation", storage.ActionSearch, own))

	if got := found(t, store, confined, "Observation", "status=final"); !slices.Equal(got, []string{"obs-mine"}) {
		t.Errorf("a confined search matched %v", got)
	}

	// The unconfined Scope sees both, so the confinement is what narrowed it.
	if got := found(t, store, searchScope("Observation"), "Observation", "status=final"); len(got) != 2 {
		t.Errorf("an unconfined search matched %v", got)
	}

	// A Scope authorizing no search matches nothing at all.
	if got := found(t, store, storage.NewScope(), "Observation", "status=final"); len(got) != 0 {
		t.Errorf("the zero Scope matched %v", got)
	}
}

// TestASearchIsNarrowedByAGrantsFilterAndProjection, because a search is a read
// and every restriction a read carries applies to it.
func TestASearchIsNarrowedByAGrantsFilterAndProjection(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-final",
		`{"resourceType":"Observation","status":"final","note":[{"text":"private"}]}`)
	seedTyped(t, store, "Observation", "obs-prelim",
		`{"resourceType":"Observation","status":"preliminary","note":[{"text":"private"}]}`)

	grant := fhirGrant("prj_a", "Observation", storage.ActionSearch)

	onlyFinal := mustFilter(t, "status", storage.ComparatorEqual, "final")
	grant.Filter = &onlyFinal

	summary := mustStoreProjection(t, "status")
	grant.Projection = &summary

	scope := storage.NewScope(grant)

	held := page(t, store, scope, "Observation", "status=final,preliminary")
	if !slices.Equal(held.ids, []string{"obs-final"}) {
		t.Errorf("a filtered search matched %v", held.ids)
	}

	values, err := url.ParseQuery("status=final")
	if err != nil {
		t.Fatal(err)
	}

	plan, err := search.Parse("Observation", values)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.Search(t.Context(), scope, plan)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(result.Records) != 1 {
		t.Fatalf("matched %d resources", len(result.Records))
	}

	if got := string(result.Records[0].Content); got == "" || strings.Contains(got, "private") {
		t.Errorf("a projected search returned %s", got)
	}
}

// TestAPageOffersANextOnlyWhenOneFollows. A link offered when nothing follows
// is a promise the next request breaks (SRC-2).
func TestAPageOffersANextOnlyWhenOneFollows(t *testing.T) {
	store, _ := newStore(t)

	for _, id := range []storage.LogicalID{"obs-1", "obs-2", "obs-3"} {
		seedTyped(t, store, "Observation", id, `{"resourceType":"Observation","status":"final"}`)
	}

	scope := searchScope("Observation")

	first := page(t, store, scope, "Observation", "status=final&_count=2")
	if !slices.Equal(first.ids, []string{"obs-1", "obs-2"}) || !first.more {
		t.Fatalf("the first page was %v (more=%v)", first.ids, first.more)
	}

	second := page(t, store, scope, "Observation",
		"status=final&_count=2&_cursor="+string(first.cursor))
	if !slices.Equal(second.ids, []string{"obs-3"}) || second.more {
		t.Errorf("the second page was %v (more=%v)", second.ids, second.more)
	}

	exact := page(t, store, scope, "Observation", "status=final&_count=3")
	if len(exact.ids) != 3 || exact.more {
		t.Errorf("a page holding everything offered a next: %v (more=%v)", exact.ids, exact.more)
	}
}

// TestATotalIsPresentOnlyWhenItWasComputed.
func TestATotalIsPresentOnlyWhenItWasComputed(t *testing.T) {
	store, _ := newStore(t)

	for _, id := range []storage.LogicalID{"obs-1", "obs-2", "obs-3"} {
		seedTyped(t, store, "Observation", id, `{"resourceType":"Observation","status":"final"}`)
	}

	scope := searchScope("Observation")

	quiet := page(t, store, scope, "Observation", "status=final&_count=1")
	if quiet.counted {
		t.Error("a query that asked for no total was given one")
	}

	counted := page(t, store, scope, "Observation", "status=final&_count=1&_total=accurate")
	if !counted.counted || counted.total != 3 {
		t.Errorf("an accurate total was %d (counted=%v), want every match rather than the page",
			counted.total, counted.counted)
	}
}

// TestASearchRefusesAGrantItCannotCompile, for the reason a by-key read does.
func TestASearchRefusesAGrantItCannotCompile(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"final"}`)

	grant := fhirGrant("prj_a", "Observation", storage.ActionSearch)
	grant.Filter = &storage.Filter{}

	values, _ := url.ParseQuery("status=final")
	plan, _ := search.Parse("Observation", values)

	if _, err := store.Search(t.Context(), storage.NewScope(grant), plan); !errors.Is(err, ErrUncompilableFilter) {
		t.Errorf("err = %v, want %v", err, ErrUncompilableFilter)
	}
}

// TestASearchReachesOnlyItsOwnProject.
func TestASearchReachesOnlyItsOwnProject(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-mine", `{"resourceType":"Observation","status":"final"}`)

	elsewhere := storage.ResourceKey{Project: "prj_b", Type: "Observation", ID: "obs-theirs"}
	if err := store.Create(t.Context(), fullScope("prj_b", "Observation"),
		storage.ResourceRecord{Key: elsewhere, Content: []byte(`{"resourceType":"Observation","status":"final"}`)}); err != nil {
		t.Fatalf("seed the other project: %v", err)
	}

	if got := found(t, store, searchScope("Observation"), "Observation", "status=final"); !slices.Equal(got, []string{"obs-mine"}) {
		t.Errorf("a search matched %v, want only its own Project's", got)
	}
}

// TestASearchUsesTheSearchGrantAndNotAReadOne. A Scope can hold both, narrowed
// differently: a clinician may read any chart they are handed the id of, and
// search only their own patients. Compiling the read Grant into a search would
// answer the wider question.
func TestASearchUsesTheSearchGrantAndNotAReadOne(t *testing.T) {
	store, db := newStore(t)

	mine := seedTyped(t, store, "Observation", "obs-mine", `{"resourceType":"Observation","status":"final"}`)
	seedTyped(t, store, "Observation", "obs-theirs", `{"resourceType":"Observation","status":"final"}`)

	own := storage.Compartment{Type: "Patient", ID: "pat-1"}
	attachCompartment(t, db, mine, own)

	scope := storage.NewScope(
		// Reading by key reaches any chart.
		fhirGrant("prj_a", "Observation", storage.ActionRead),
		// Searching reaches only this clinician's own patient.
		compartmentGrant("prj_a", "Observation", storage.ActionSearch, own),
	)

	if got := found(t, store, scope, "Observation", "status=final"); !slices.Equal(got, []string{"obs-mine"}) {
		t.Errorf("a search matched %v, want only what its own Grant reaches", got)
	}
}

// TestASearchIsNotAuthorizedByAReadAlone.
func TestASearchIsNotAuthorizedByAReadAlone(t *testing.T) {
	store, _ := newStore(t)

	seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"final"}`)

	readOnly := storage.NewScope(fhirGrant("prj_a", "Observation", storage.ActionRead))

	if got := found(t, store, readOnly, "Observation", "status=final"); len(got) != 0 {
		t.Errorf("a read grant authorized a search: %v", got)
	}
}

// TestAnInstallThatPredatesTheIndexIsBackfilled. The index table is created by
// the schema like any other, so an install that predates it would come up with
// every resource in it unsearchable — a predicate with nothing behind it,
// answering "no matches" for data that is plainly there.
func TestAnInstallThatPredatesTheIndexIsBackfilled(t *testing.T) {
	store, db := newStore(t)

	seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"final"}`)
	seedTyped(t, store, "Observation", "obs-2", `{"resourceType":"Observation","status":"preliminary"}`)

	buried := seedTyped(t, store, "Observation", "obs-gone", `{"resourceType":"Observation","status":"final"}`)
	if err := store.Delete(t.Context(), fullScope("prj_a", "Observation"), buried, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// What an install that predates the index looks like.
	if _, err := db.ExecContext(t.Context(), "DROP TABLE fhir_search_index"); err != nil {
		t.Fatalf("drop the index: %v", err)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	scope := searchScope("Observation")

	if got := found(t, store, scope, "Observation", "status=final"); !slices.Equal(got, []string{"obs-1"}) {
		t.Errorf("after the backfill status=final matched %v", got)
	}

	if got := found(t, store, scope, "Observation", "status=preliminary"); !slices.Equal(got, []string{"obs-2"}) {
		t.Errorf("after the backfill status=preliminary matched %v", got)
	}

	// A tombstone holds no content and carries no index.
	var buriedRows int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM fhir_search_index WHERE res_id = 'obs-gone'").Scan(&buriedRows); err != nil {
		t.Fatalf("count: %v", err)
	}

	if buriedRows != 0 {
		t.Errorf("the backfill indexed %d rows for a tombstone", buriedRows)
	}
}

// TestPreparingTwiceDoesNotRebuildTheIndex, so a restart is not a reindex of
// every resource the install holds.
func TestPreparingTwiceDoesNotRebuildTheIndex(t *testing.T) {
	store, db := newStore(t)

	seedTyped(t, store, "Observation", "obs-1", `{"resourceType":"Observation","status":"final"}`)

	// A row nothing would derive: it survives only if the backfill is skipped.
	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO fhir_search_index (project_id, res_type, res_id, param, kind, code)"+
			" VALUES ('prj_a', 'Observation', 'obs-1', 'status', 'token', 'sentinel')"); err != nil {
		t.Fatalf("mark the index: %v", err)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	var marks int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM fhir_search_index WHERE code = 'sentinel'").Scan(&marks); err != nil {
		t.Fatalf("count: %v", err)
	}

	if marks != 1 {
		t.Error("preparing an install that already has an index rebuilt it")
	}
}
