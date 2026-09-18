package storage_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// TestAFilterIsBuiltOnlyFromAPathThisServerCanRead. A filter nobody validated
// could only be dropped by the backend that failed to read it, and a dropped
// filter widens the Grant it was carried on, so every bad shape fails here
// rather than reaching a Grant.
func TestAFilterIsBuiltOnlyFromAPathThisServerCanRead(t *testing.T) {
	refused := map[string]struct {
		path string
		want error
	}{
		"no path at all":                 {"", storage.ErrMissingFilterPath},
		"an empty segment":               {"category..code", storage.ErrMalformedFilterPath},
		"a trailing dot":                 {"status.", storage.ErrMalformedFilterPath},
		"a leading dot":                  {".status", storage.ErrMalformedFilterPath},
		"a quote":                        {"status'", storage.ErrMalformedFilterPath},
		"a path operator":                {"$.status", storage.ErrMalformedFilterPath},
		"an array subscript":             {"category[0].code", storage.ErrMalformedFilterPath},
		"a wildcard":                     {"category.*.code", storage.ErrMalformedFilterPath},
		"a leading digit":                {"1status", storage.ErrMalformedFilterPath},
		"a space":                        {"status code", storage.ErrMalformedFilterPath},
		"deeper than a backend compiles": {"a.b.c.d.e", storage.ErrFilterPathTooDeep},
	}

	for name, refusal := range refused {
		_, err := storage.NewFilter(refusal.path, storage.ComparatorEqual, "final")
		if !errors.Is(err, refusal.want) {
			t.Errorf("%s (%q): err = %v, want %v", name, refusal.path, err, refusal.want)
		}
	}
}

// TestAFilterCarriesAValueSetItsComparatorCanTake, because equality over two
// values and membership over none are both a rule nobody meant to write.
func TestAFilterCarriesAValueSetItsComparatorCanTake(t *testing.T) {
	refused := map[string]struct {
		comparator storage.Comparator
		values     []string
		want       error
	}{
		"equality over two values": {storage.ComparatorEqual, []string{"final", "amended"}, storage.ErrFilterValues},
		"equality over none":       {storage.ComparatorEqual, nil, storage.ErrFilterValues},
		"membership over none":     {storage.ComparatorIn, nil, storage.ErrFilterValues},
		"an empty value":           {storage.ComparatorEqual, []string{""}, storage.ErrFilterValues},
		"an empty value among many": {
			storage.ComparatorIn, []string{"final", ""}, storage.ErrFilterValues,
		},
		"a comparator outside the enum": {storage.Comparator("like"), []string{"fin%"}, storage.ErrUnknownComparator},
	}

	for name, refusal := range refused {
		_, err := storage.NewFilter("status", refusal.comparator, refusal.values...)
		if !errors.Is(err, refusal.want) {
			t.Errorf("%s: err = %v, want %v", name, err, refusal.want)
		}
	}
}

// TestAPathThisServerCanReadIsAccepted, so the guard above refuses bad shapes
// rather than every shape.
func TestAPathThisServerCanReadIsAccepted(t *testing.T) {
	accepted := []string{"status", "class", "category.coding.code", "code2", "a.b.c.d"}

	for _, path := range accepted {
		filter, err := storage.NewFilter(path, storage.ComparatorIn, "one", "two")
		if err != nil {
			t.Errorf("%q: %v", path, err)

			continue
		}

		if got := len(filter.Path()); got == 0 {
			t.Errorf("%q compiled to no hops", path)
		}
	}
}

// TestTheZeroFilterNamesNothing. A Grant carries *Filter so that absent means
// unfiltered; a present one nobody built must be distinguishable from that, or
// a backend would read it as no restriction at all.
func TestTheZeroFilterNamesNothing(t *testing.T) {
	var zero storage.Filter

	if !zero.IsZero() {
		t.Error("the zero Filter does not report itself as one")
	}

	built, err := storage.NewFilter("status", storage.ComparatorEqual, "final")
	if err != nil {
		t.Fatalf("build filter: %v", err)
	}

	if built.IsZero() {
		t.Error("a built Filter reports itself as the zero one")
	}
}

// TestAFilterCannotBeWidenedByItsHolder, because Path and Values hand out the
// slices a compiled predicate is built from.
func TestAFilterCannotBeWidenedByItsHolder(t *testing.T) {
	filter, err := storage.NewFilter("category.coding.code", storage.ComparatorIn, "vital-signs")
	if err != nil {
		t.Fatalf("build filter: %v", err)
	}

	values := filter.Values()
	values[0] = "laboratory"

	path := filter.Path()
	path[0] = "elsewhere"

	if got := filter.Values(); !slices.Equal(got, []string{"vital-signs"}) {
		t.Errorf("a caller widened the value set to %v", got)
	}

	if got := filter.Path(); !slices.Equal(got, []string{"category", "coding", "code"}) {
		t.Errorf("a caller redirected the path to %v", got)
	}
}
