package storage

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A filter this server cannot read denies the Grant carrying it. Dropping one
// would widen that Grant, which is the single thing a filter must never do, so
// every way of building a bad filter is an error here rather than a Filter some
// backend later decides to ignore.
var (
	// ErrMissingFilterPath reports a filter naming no element to read.
	ErrMissingFilterPath = errors.New("storage: a filter names one element path")

	// ErrMalformedFilterPath reports a path outside the dotted element form.
	ErrMalformedFilterPath = errors.New("storage: filter path is not a dotted element path")

	// ErrFilterPathTooDeep reports a path longer than a backend compiles, so no
	// filter can turn one authorization check into an unbounded walk.
	ErrFilterPathTooDeep = errors.New("storage: filter path is deeper than a backend compiles")

	// ErrUnknownComparator reports a comparator outside the enum.
	ErrUnknownComparator = errors.New("storage: unknown filter comparator")

	// ErrFilterValues reports a value set the comparator cannot take.
	ErrFilterValues = errors.New("storage: filter value set does not fit its comparator")
)

// Comparator is how a Filter compares the element it names to its values. Both
// forms narrow: no comparator admits a resource that the same rule without a
// filter would refuse.
type Comparator string

// The comparisons a Filter can make.
const (
	// ComparatorEqual admits a resource whose element holds the one value.
	ComparatorEqual Comparator = "eq"

	// ComparatorIn admits a resource whose element holds any of the values.
	ComparatorIn Comparator = "in"
)

// MaxFilterPathDepth bounds how far into a resource a filter reads. It is four
// because that is what the deepest useful restriction needs — the coded
// category at category.coding.code is three — and because a bound is what stops
// one policy row from describing an arbitrarily expensive traversal.
const MaxFilterPathDepth = 4

// Filter narrows a Grant to the resources whose named element matches. Its
// fields are unexported, so every Filter in existence is one NewFilter
// validated: a filter nobody validated could only be dropped by the backend
// that failed to read it, and a dropped filter widens its Grant.
//
// The zero Filter names no path and matches nothing, which is why a Grant
// carries *Filter: absent is unfiltered, present is narrowed.
type Filter struct {
	path       []string
	comparator Comparator
	values     []string
}

// NewFilter builds a filter over one dotted element path, such as "status" or
// "category.coding.code". Whether an element along the path repeats is not
// stated here: a backend reads a single element and a repeating one the same
// way, so a rule's author does not have to know which one FHIR chose.
func NewFilter(path string, comparator Comparator, values ...string) (Filter, error) {
	segments, err := parseFilterPath(path)
	if err != nil {
		return Filter{}, err
	}

	if comparator != ComparatorEqual && comparator != ComparatorIn {
		return Filter{}, fmt.Errorf("%w: %q", ErrUnknownComparator, string(comparator))
	}

	if err := checkFilterValues(comparator, values); err != nil {
		return Filter{}, err
	}

	return Filter{path: segments, comparator: comparator, values: slices.Clone(values)}, nil
}

// parseFilterPath splits a path into the hops a backend walks.
func parseFilterPath(path string) ([]string, error) {
	if path == "" {
		return nil, ErrMissingFilterPath
	}

	segments := strings.Split(path, ".")
	if len(segments) > MaxFilterPathDepth {
		return nil, fmt.Errorf("%w: %q has %d segments", ErrFilterPathTooDeep, path, len(segments))
	}

	for _, segment := range segments {
		if !isElementName(segment) {
			return nil, fmt.Errorf("%w: %q in %q", ErrMalformedFilterPath, segment, path)
		}
	}

	return segments, nil
}

// isElementName reports whether a segment is a FHIR element name: a letter
// followed by letters and digits. Nothing else may appear, so a segment can
// carry no quote, bracket or path operator into the statement it is bound to.
func isElementName(segment string) bool {
	if segment == "" {
		return false
	}

	for index, letter := range segment {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= 'A' && letter <= 'Z':
		case index > 0 && letter >= '0' && letter <= '9':
		default:
			return false
		}
	}

	return true
}

// checkFilterValues refuses a value set its comparator cannot take. Equality
// names one value and membership at least one; neither names an empty string,
// which no FHIR code is and which would more likely be an unbound variable than
// an intended restriction.
func checkFilterValues(comparator Comparator, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%w: %s names no value", ErrFilterValues, comparator)
	}

	if comparator == ComparatorEqual && len(values) != 1 {
		return fmt.Errorf("%w: %s names %d values", ErrFilterValues, comparator, len(values))
	}

	if slices.Contains(values, "") {
		return fmt.Errorf("%w: %s names an empty value", ErrFilterValues, comparator)
	}

	return nil
}

// Path returns the element path, one segment per hop.
func (f Filter) Path() []string {
	return slices.Clone(f.path)
}

// Comparator returns how the element is compared to the values.
func (f Filter) Comparator() Comparator {
	return f.comparator
}

// Values returns a copy, so a caller holding a Filter cannot widen it.
func (f Filter) Values() []string {
	return slices.Clone(f.values)
}

// IsZero reports whether this is a Filter nobody built. A backend handed one
// must refuse the Grant rather than read it as no restriction at all.
func (f Filter) IsZero() bool {
	return len(f.path) == 0
}

// Equal reports whether two filters state the same restriction.
func (f Filter) Equal(other Filter) bool {
	return f.comparator == other.comparator &&
		slices.Equal(f.path, other.path) && slices.Equal(f.values, other.values)
}

// String renders the filter for an error or an audit line.
func (f Filter) String() string {
	return strings.Join(f.path, ".") + " " + string(f.comparator) + " " + strings.Join(f.values, ",")
}
