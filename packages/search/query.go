package search

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrUnknownParameter reports a parameter this build does not implement for
	// this type. It is refused rather than ignored: a search that silently drops
	// a criterion returns more than it was asked for, and the caller cannot tell
	// (SRC-4).
	ErrUnknownParameter = errors.New("search: this build implements no such parameter")

	// ErrUnsupportedModifier reports a modifier, chain or prefix this build does
	// not apply, refused for the reason an unknown parameter is.
	ErrUnsupportedModifier = errors.New("search: this build applies no such modifier")

	// ErrMalformedValue reports a value this build cannot read.
	ErrMalformedValue = errors.New("search: a parameter value cannot be read")

	// ErrMalformedPaging reports a page size or cursor that is not one.
	ErrMalformedPaging = errors.New("search: a page is named by a count and a cursor")
)

// How many resources one page carries. A caller may ask for fewer, and for more
// up to the ceiling: an unbounded page is one request that can read a Project.
const (
	DefaultCount = 20
	MaxCount     = 200
)

// The parameters that describe the page rather than the resources on it.
const (
	CountParameter  = "_count"
	CursorParameter = "_cursor"
	TotalParameter  = "_total"
)

// Comparison is how a date value is compared. FHIR spells these as prefixes on
// the value itself.
type Comparison string

// The comparisons this build applies.
const (
	CompareEqual        Comparison = "eq"
	CompareGreater      Comparison = "gt"
	CompareLess         Comparison = "lt"
	CompareGreaterEqual Comparison = "ge"
	CompareLessEqual    Comparison = "le"
)

var knownComparisons = []Comparison{
	CompareEqual, CompareGreater, CompareLess, CompareGreaterEqual, CompareLessEqual,
}

// Value is one alternative a criterion accepts.
type Value struct {
	// text is what a token, string or reference matches against.
	text string

	// system qualifies a token, absent when the query named none.
	system string

	// lower and upper bound a date, in milliseconds, inclusive.
	lower, upper int64

	compare  Comparison
	systemed bool
}

// Text returns the value a token, string or reference matches.
func (v Value) Text() string { return v.text }

// System returns the system qualifying a token, and whether one was named.
func (v Value) System() (string, bool) { return v.system, v.systemed }

// Range returns the instants a date value spans, inclusive.
func (v Value) Range() (int64, int64) { return v.lower, v.upper }

// Compare returns how a date value is compared.
func (v Value) Compare() Comparison { return v.compare }

// Criterion is one parameter and the values it accepts. The values are
// alternatives and the criteria are all required, which is what FHIR means by
// repeating a parameter versus comma-separating its values.
type Criterion struct {
	parameter Parameter
	values    []Value
}

// Parameter returns what is being matched.
func (c Criterion) Parameter() Parameter { return c.parameter }

// Values returns the alternatives, any one of which satisfies the criterion.
func (c Criterion) Values() []Value { return slices.Clone(c.values) }

// Query is one search, already checked against the registry: every criterion in
// it is one this build can apply. It is backend-independent — what executes it
// knows how, and nothing here knows that.
type Query struct {
	resourceType storage.ResourceType

	// custom is what this Project added, carried so the parameters a query may
	// name are the ones its own Project defined rather than a global list.
	custom   Custom
	criteria []Criterion
	count    int
	cursor   storage.LogicalID
	total    bool
}

// Type returns the resource type being searched.
func (q Query) Type() storage.ResourceType { return q.resourceType }

// Criteria returns what must match, in a stable order so the same query
// compiles to the same statement.
func (q Query) Criteria() []Criterion { return slices.Clone(q.criteria) }

// Count returns how many resources one page carries.
func (q Query) Count() int { return q.count }

// Cursor returns where this page resumes from, empty on a first page.
func (q Query) Cursor() storage.LogicalID { return q.cursor }

// CountsTotal reports whether the caller asked for a total. A total is present
// only when it was computed, because an absent total and a wrong one are very
// different promises (SRC-2).
func (q Query) CountsTotal() bool { return q.total }

// Parse reads one query and refuses everything this build cannot apply.
func Parse(custom Custom, resourceType storage.ResourceType, asked url.Values) (Query, error) {
	query := Query{custom: custom, resourceType: resourceType, count: DefaultCount}

	names := make([]string, 0, len(asked))
	for name := range asked {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		if err := query.read(name, asked[name]); err != nil {
			return Query{}, err
		}
	}

	return query, nil
}

// read applies one parameter from the query string.
func (q *Query) read(name string, raw []string) error {
	switch name {
	case CountParameter:
		return q.readCount(raw)
	case CursorParameter:
		return q.readCursor(raw)
	case TotalParameter:
		return q.readTotal(raw)
	}

	// A modifier, a chain or a reverse chain. Each is a different search from
	// the one the bare name means, so answering the bare one would answer a
	// question nobody asked.
	if strings.ContainsAny(name, ":.") {
		return fmt.Errorf("%w: %s", ErrUnsupportedModifier, name)
	}

	parameter, implemented := Find(q.custom, q.resourceType, name)
	if !implemented {
		return fmt.Errorf("%w: %s on %s", ErrUnknownParameter, name, q.resourceType)
	}

	for _, stated := range raw {
		values, err := readValues(parameter, stated)
		if err != nil {
			return err
		}

		q.criteria = append(q.criteria, Criterion{parameter: parameter, values: values})
	}

	return nil
}

func (q *Query) readCount(raw []string) error {
	if len(raw) != 1 {
		return fmt.Errorf("%w: %s is named once", ErrMalformedPaging, CountParameter)
	}

	count, err := strconv.Atoi(raw[0])
	if err != nil || count < 1 || count > MaxCount {
		return fmt.Errorf("%w: %s must be between 1 and %d", ErrMalformedPaging, CountParameter, MaxCount)
	}

	q.count = count

	return nil
}

func (q *Query) readCursor(raw []string) error {
	if len(raw) != 1 || raw[0] == "" {
		return fmt.Errorf("%w: %s is named once", ErrMalformedPaging, CursorParameter)
	}

	q.cursor = storage.LogicalID(raw[0])

	return nil
}

// readTotal reads whether a total is wanted. "none" and "accurate" are the two
// this build answers; "estimate" is refused because this build has no estimate
// to give and answering with an accurate one would make the parameter a lie.
func (q *Query) readTotal(raw []string) error {
	if len(raw) != 1 {
		return fmt.Errorf("%w: %s is named once", ErrMalformedPaging, TotalParameter)
	}

	switch raw[0] {
	case "none":
		q.total = false
	case "accurate":
		q.total = true
	default:
		return fmt.Errorf("%w: %s=%s", ErrUnsupportedModifier, TotalParameter, raw[0])
	}

	return nil
}

// readValues splits one stated value into the alternatives it names. A comma
// separates alternatives; an escaped comma is part of a value.
func readValues(parameter Parameter, stated string) ([]Value, error) {
	if stated == "" {
		return nil, fmt.Errorf("%w: %s names no value", ErrMalformedValue, parameter.Name())
	}

	parts := splitAlternatives(stated)
	values := make([]Value, 0, len(parts))

	for _, part := range parts {
		value, err := readValue(parameter, part)
		if err != nil {
			return nil, err
		}

		values = append(values, value)
	}

	return values, nil
}

// splitAlternatives splits on unescaped commas, which is how FHIR writes "any
// one of these".
func splitAlternatives(stated string) []string {
	var (
		parts  []string
		held   strings.Builder
		escape bool
	)

	for _, letter := range stated {
		switch {
		case escape:
			held.WriteRune(letter)

			escape = false
		case letter == '\\':
			escape = true
		case letter == ',':
			parts = append(parts, held.String())
			held.Reset()
		default:
			held.WriteRune(letter)
		}
	}

	return append(parts, held.String())
}

func readValue(parameter Parameter, stated string) (Value, error) {
	if stated == "" {
		return Value{}, fmt.Errorf("%w: %s names an empty value", ErrMalformedValue, parameter.Name())
	}

	switch parameter.Kind() {
	case KindToken:
		return readToken(stated), nil
	case KindString, KindReference:
		return Value{text: stated}, nil
	case KindDate:
		return readDate(parameter, stated)
	default:
		return Value{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(parameter.Kind()))
	}
}

// readToken reads "code" or "system|code". A bare code matches whatever system
// it was recorded under, which is what a caller who named none asked for.
func readToken(stated string) Value {
	system, code, qualified := strings.Cut(stated, "|")
	if !qualified {
		return Value{text: stated}
	}

	return Value{text: code, system: system, systemed: true}
}

// readDate reads an optional comparison prefix and the instant it applies to.
func readDate(parameter Parameter, stated string) (Value, error) {
	compare := CompareEqual

	if len(stated) > 2 {
		if candidate := Comparison(stated[:2]); slices.Contains(knownComparisons, candidate) {
			compare, stated = candidate, stated[2:]
		}
	}

	lower, upper, err := readInstant(stated)
	if err != nil {
		return Value{}, fmt.Errorf("%w: %s: %w", ErrMalformedValue, parameter.Name(), err)
	}

	return Value{compare: compare, lower: lower, upper: upper}, nil
}

// readInstant reads the two date forms this build matches: a whole day, and one
// instant. A partial date such as "2026-01" is refused rather than guessed at,
// because the guess decides which resources a search returns.
func readInstant(stated string) (int64, int64, error) {
	if day, err := time.Parse(time.DateOnly, stated); err == nil {
		start := day.UTC()

		return start.UnixMilli(), start.AddDate(0, 0, 1).Add(-time.Millisecond).UnixMilli(), nil
	}

	moment, err := time.Parse(time.RFC3339, stated)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is neither a date nor an instant", stated)
	}

	return moment.UTC().UnixMilli(), moment.UTC().UnixMilli(), nil
}

// Narrowed returns the query asking for at most this many matches.
//
// A conditional interaction needs to know whether its condition matched one
// resource or several, and nothing beyond that: counting the rest of a type
// answers a question nobody asked and reads rows nobody will look at.
func (q Query) Narrowed(count int) Query {
	if count > 0 && count < q.count {
		q.count = count
	}

	return q
}
