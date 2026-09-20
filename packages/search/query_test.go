package search_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/search"
)

func parsed(t *testing.T, resourceType, query string) search.Query {
	t.Helper()

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}

	held, err := search.Parse(nil, storageType(resourceType), values)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}

	return held
}

func refused(t *testing.T, resourceType, query string) error {
	t.Helper()

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}

	_, err = search.Parse(nil, storageType(resourceType), values)

	return err
}

// TestAParameterThisBuildDoesNotImplementIsRefused. A search that silently
// drops a criterion returns more than it was asked for, and the caller cannot
// tell (SRC-4).
func TestAParameterThisBuildDoesNotImplementIsRefused(t *testing.T) {
	cases := map[string]struct {
		resourceType, query string
		want                error
	}{
		"a parameter nobody declared":    {"Observation", "colour=blue", search.ErrUnknownParameter},
		"one declared for another type":  {"Observation", "gender=female", search.ErrUnknownParameter},
		"a modifier needing a hierarchy": {"Patient", "gender:above=female", search.ErrUnsupportedModifier},
		"a modifier for the wrong kind":  {"Patient", "gender:contains=fem", search.ErrUnsupportedModifier},
		"a chain":                        {"Observation", "subject.name=Ada", search.ErrUnsupportedModifier},
		"an include":                     {"Observation", "_include=Observation:subject", search.ErrUnknownParameter},
		"a sort":                         {"Observation", "_sort=date", search.ErrUnknownParameter},
		"a summary":                      {"Observation", "_summary=true", search.ErrUnknownParameter},
		"an estimated total":             {"Observation", "_total=estimate", search.ErrUnsupportedModifier},
	}

	for name, tc := range cases {
		if err := refused(t, tc.resourceType, tc.query); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// TestTheUniversalParametersAnswerForEveryType, so nothing has to declare them
// and no type is without a way to be paged through.
func TestTheUniversalParametersAnswerForEveryType(t *testing.T) {
	for _, resourceType := range []string{"Observation", "Patient", "CodeSystem", "Binary"} {
		query := parsed(t, resourceType, "_id=abc&_lastUpdated=2026-01-01")
		if len(query.Criteria()) != 2 {
			t.Errorf("%s answered %d universal criteria", resourceType, len(query.Criteria()))
		}
	}
}

// TestCommasAreAlternativesAndRepeatsAreRequirements, which is the whole of
// FHIR's and/or convention for a query string.
func TestCommasAreAlternativesAndRepeatsAreRequirements(t *testing.T) {
	query := parsed(t, "Observation", "status=final,amended&category=vital-signs")

	if len(query.Criteria()) != 2 {
		t.Fatalf("two parameters read as %d criteria", len(query.Criteria()))
	}

	for _, criterion := range query.Criteria() {
		switch criterion.Parameter().Name() {
		case "status":
			if len(criterion.Values()) != 2 {
				t.Errorf("a comma-separated status read as %d alternatives", len(criterion.Values()))
			}
		case "category":
			if len(criterion.Values()) != 1 {
				t.Errorf("one category read as %d alternatives", len(criterion.Values()))
			}
		}
	}

	repeated := parsed(t, "Observation", "status=final&status=amended")
	if len(repeated.Criteria()) != 2 {
		t.Errorf("a repeated parameter read as %d criteria, want one each", len(repeated.Criteria()))
	}
}

// TestAQualifiedTokenNamesItsSystem, and a bare one matches whatever system it
// was recorded under, which is what a caller who named none asked for.
func TestAQualifiedTokenNamesItsSystem(t *testing.T) {
	query := parsed(t, "Observation", "category=http://loinc.org|vital-signs&status=final")

	for _, criterion := range query.Criteria() {
		value := criterion.Values()[0]
		system, qualified := value.System()

		switch criterion.Parameter().Name() {
		case "category":
			if !qualified || system != "http://loinc.org" || value.Text() != "vital-signs" {
				t.Errorf("a qualified token read as %q|%q", system, value.Text())
			}
		case "status":
			if qualified {
				t.Errorf("a bare token read as qualified by %q", system)
			}
		}
	}
}

// TestADateComparisonIsReadFromItsPrefix.
func TestADateComparisonIsReadFromItsPrefix(t *testing.T) {
	query := parsed(t, "Observation", "date=ge2026-01-01&_lastUpdated=2026-02-03T04:05:06Z")

	for _, criterion := range query.Criteria() {
		value := criterion.Values()[0]

		switch criterion.Parameter().Name() {
		case "date":
			if value.Compare() != search.CompareGreaterEqual {
				t.Errorf("ge2026-01-01 read as %q", value.Compare())
			}

			lower, upper := value.Range()
			if upper <= lower {
				t.Error("a whole day read as a single instant")
			}
		case "_lastUpdated":
			lower, upper := value.Range()
			if lower != upper {
				t.Error("an instant read as a span")
			}
		}
	}
}

// TestADateThisBuildCannotReadIsRefused rather than widened to the year it
// names, because the widening decides which resources come back.
func TestADateThisBuildCannotReadIsRefused(t *testing.T) {
	for _, stated := range []string{"date=2026-01", "date=2026", "date=last-tuesday", "date=ge2026-01"} {
		if err := refused(t, "Observation", stated); !errors.Is(err, search.ErrMalformedValue) {
			t.Errorf("%s: err = %v, want %v", stated, err, search.ErrMalformedValue)
		}
	}
}

// TestAPageIsBoundedWhateverTheCallerAsksFor. An unbounded page is one request
// that can read a Project.
func TestAPageIsBoundedWhateverTheCallerAsksFor(t *testing.T) {
	if got := parsed(t, "Observation", "status=final").Count(); got != search.DefaultCount {
		t.Errorf("an unstated count read as %d", got)
	}

	if got := parsed(t, "Observation", "_count=5").Count(); got != 5 {
		t.Errorf("_count=5 read as %d", got)
	}

	for _, stated := range []string{"_count=0", "_count=-1", "_count=201", "_count=many", "_count=1&_count=2"} {
		if err := refused(t, "Observation", stated); !errors.Is(err, search.ErrMalformedPaging) {
			t.Errorf("%s: err = %v, want %v", stated, err, search.ErrMalformedPaging)
		}
	}
}

// TestATotalIsOnlyPromisedWhenItIsAskedFor, because an absent total and a wrong
// one are very different promises (SRC-2).
func TestATotalIsOnlyPromisedWhenItIsAskedFor(t *testing.T) {
	if parsed(t, "Observation", "status=final").CountsTotal() {
		t.Error("a query that asked for no total wants one")
	}

	if !parsed(t, "Observation", "_total=accurate").CountsTotal() {
		t.Error("_total=accurate did not ask for one")
	}

	if parsed(t, "Observation", "_total=none").CountsTotal() {
		t.Error("_total=none asked for one")
	}
}

// TestAnEscapedCommaIsPartOfTheValue, not a separator between two.
func TestAnEscapedCommaIsPartOfTheValue(t *testing.T) {
	query := parsed(t, "Organization", `name=Smith\,Jones`)

	values := query.Criteria()[0].Values()
	if len(values) != 1 || values[0].Text() != "Smith,Jones" {
		t.Errorf("an escaped comma read as %v", values)
	}
}

// TestTheSameQueryReadsTheSameWayTwice, so what a search compiles to does not
// depend on the order a map happened to hand its keys over in.
func TestTheSameQueryReadsTheSameWayTwice(t *testing.T) {
	const asked = "status=final&category=vital-signs&subject=Patient/pat-1&_count=7"

	first := parsed(t, "Observation", asked)

	for range 20 {
		again := parsed(t, "Observation", asked)

		if len(again.Criteria()) != len(first.Criteria()) {
			t.Fatalf("the same query read as %d and %d criteria", len(first.Criteria()), len(again.Criteria()))
		}

		for index := range first.Criteria() {
			if again.Criteria()[index].Parameter().Name() != first.Criteria()[index].Parameter().Name() {
				t.Fatalf("the same query read its criteria in a different order")
			}
		}
	}
}
