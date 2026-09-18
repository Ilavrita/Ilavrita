package pocketbase

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ErrUncompilableFilter reports a Grant carrying a filter this backend cannot
// compile. The Grant is refused rather than compiled without it: a dropped
// predicate widens the Grant that carried it, and that is the bug a filter
// exists to prevent.
var ErrUncompilableFilter = errors.New("pocketbase: grant carries a filter this backend cannot compile")

// hopPathBindings is how many times one hop binds its element name. SQLite has
// no way to name an expression once inside a CASE, so the path is bound for the
// test and for each branch.
const hopPathBindings = 3

// filterPredicate compiles a Filter into an EXISTS over whatever JSON the
// source expression yields: a content column when a row is read, a bound
// parameter when a write is checked against the content it submits. One
// generator answers both, so the rule a read enforces and the rule a write is
// checked against cannot drift apart.
//
// Each path segment is one hop, and a hop normalises what it finds to an array
// before iterating it. That is what lets one path read an element FHIR models
// as repeating and one it models as single without saying which it is:
// category.coding.code crosses two arrays, class.code crosses none, and the
// same three hops serve both. A hop finding a scalar where the path expected an
// object yields no row, so a resource shaped unlike the path fails the filter
// rather than failing the query.
//
// A nil Filter compiles to no predicate, which is the unfiltered Grant.
func filterPredicate(source string, filter *storage.Filter) (string, []any, error) {
	if filter == nil {
		return "", nil, nil
	}

	segments := filter.Path()
	if len(segments) == 0 || len(segments) > storage.MaxFilterPathDepth {
		return "", nil, fmt.Errorf("%w: %d path segments", ErrUncompilableFilter, len(segments))
	}

	hops := make([]string, 0, len(segments))
	args := make([]any, 0, len(segments)*hopPathBindings+len(filter.Values()))
	held := source

	for index, segment := range segments {
		alias := "fp" + strconv.Itoa(index)

		hops = append(hops, "json_each("+normalisedHop(held)+") "+alias)

		path := elementPath(segment)
		for range hopPathBindings {
			args = append(args, path)
		}

		held = "json_quote(" + alias + ".value)"
	}

	leaf := "fp" + strconv.Itoa(len(segments)-1) + ".value"

	comparison, values, err := compareLeaf(leaf, *filter)
	if err != nil {
		return "", nil, err
	}

	return "EXISTS (SELECT 1 FROM " + strings.Join(hops, ", ") + " WHERE " + comparison + ")",
		append(args, values...), nil
}

// normalisedHop reads one element and presents it as an array, so a single
// element and a repeating one are iterated identically.
//
// json_quote carries the previous hop's value back as JSON text: json_each
// hands a scalar element over as an SQL scalar, which the next hop would
// otherwise read as malformed JSON and fail the whole query on.
func normalisedHop(held string) string {
	return "CASE WHEN json_type(" + held + " -> ?) = 'array' THEN " + held + " -> ?" +
		" ELSE json_array(json(" + held + " -> ?)) END"
}

// elementPath is the SQLite path for one element name. The name is bound as a
// value and never spliced into the statement, so nothing a path carries can be
// read as SQL.
func elementPath(segment string) string {
	return "$." + segment
}

// compareLeaf compiles the comparison the filter states against the value the
// last hop reached. A comparator this backend does not implement is refused,
// never treated as the nearest one it does.
func compareLeaf(leaf string, filter storage.Filter) (string, []any, error) {
	values := filter.Values()

	switch filter.Comparator() {
	case storage.ComparatorEqual:
		if len(values) != 1 {
			return "", nil, fmt.Errorf("%w: %s over %d values",
				ErrUncompilableFilter, storage.ComparatorEqual, len(values))
		}

		return leaf + " = ?", []any{values[0]}, nil
	case storage.ComparatorIn:
		if len(values) == 0 {
			return "", nil, fmt.Errorf("%w: %s over no value", ErrUncompilableFilter, storage.ComparatorIn)
		}

		args := make([]any, 0, len(values))
		for _, value := range values {
			args = append(args, value)
		}

		return leaf + " IN (" + placeholders(len(values)) + ")", args, nil
	default:
		return "", nil, fmt.Errorf("%w: comparator %q", ErrUncompilableFilter, string(filter.Comparator()))
	}
}

// placeholders renders one bind marker per value.
func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}
