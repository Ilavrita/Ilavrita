package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	// ErrEmptyProjection reports a projection naming no element. A rule that
	// returns nothing is not a rule anyone meant to write, and it is
	// indistinguishable from one whose element list was never bound.
	ErrEmptyProjection = errors.New("storage: a projection names at least one element")

	// ErrMalformedProjectionElement reports an element name this server cannot
	// read. A name nothing matches would withhold everything, which is a
	// restriction nobody stated.
	ErrMalformedProjectionElement = errors.New("storage: projection names something that is not an element")
)

// alwaysReturned are the members every projection carries whatever it names.
//
// A resource that cannot be addressed or version-checked is not usable, and
// withholding these buys nothing: the reader already holds the row. meta is
// returned whole rather than only its versionId, because it is the server's own
// bookkeeping and a version id without the instant beside it is a record a
// client cannot reason about.
var alwaysReturned = []string{"resourceType", "id", "meta"}

// Projection is the set of elements a Grant returns. An element it does not
// name is absent from the response rather than empty, so a reader cannot tell a
// withheld value from one nobody recorded.
//
// Its fields are unexported for the reason Filter's are: a projection nobody
// validated could only be dropped by whatever failed to read it, and a dropped
// projection returns the whole resource.
type Projection struct {
	elements []string
}

// NewProjection names the elements a rule returns, beside the ones every
// projection carries.
func NewProjection(elements ...string) (Projection, error) {
	if len(elements) == 0 {
		return Projection{}, ErrEmptyProjection
	}

	named := make([]string, 0, len(elements)+len(alwaysReturned))
	named = append(named, alwaysReturned...)

	for _, element := range elements {
		if !isElementName(element) {
			return Projection{}, fmt.Errorf("%w: %q", ErrMalformedProjectionElement, element)
		}

		if !slices.Contains(named, element) {
			named = append(named, element)
		}
	}

	slices.Sort(named)

	return Projection{elements: named}, nil
}

// Elements returns every member this projection carries, sorted, including the
// ones it carries whatever it was asked for.
func (p Projection) Elements() []string {
	return slices.Clone(p.elements)
}

// IsZero reports whether this is a Projection nobody built. A backend handed
// one must refuse the Grant rather than read it as returning everything.
func (p Projection) IsZero() bool {
	return len(p.elements) == 0
}

// Equal reports whether two projections return the same members.
func (p Projection) Equal(other Projection) bool {
	return slices.Equal(p.elements, other.elements)
}

// String renders the projection for an error or an audit line.
func (p Projection) String() string {
	return strings.Join(p.elements, ",")
}

// Apply returns the resource with every element the projection does not name
// removed. Removing rather than emptying is the point: an absent element reads
// as one nobody recorded, and an empty one reads as one that was withheld.
func (p Projection) Apply(content []byte) ([]byte, error) {
	if p.IsZero() {
		return nil, ErrEmptyProjection
	}

	if len(content) == 0 {
		return content, nil
	}

	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &members); err != nil {
		return nil, fmt.Errorf("storage: read a resource to narrow it: %w", err)
	}

	for name := range members {
		if !slices.Contains(p.elements, name) {
			delete(members, name)
		}
	}

	narrowed, err := json.Marshal(members)
	if err != nil {
		return nil, fmt.Errorf("storage: write a narrowed resource: %w", err)
	}

	return narrowed, nil
}

// WidestProjection returns the projection that returns the most, which is how
// the projections of several Grants covering one resource combine: Grants are
// held together, so a member any one of them returns is one the reader may
// have.
//
// A nil projection among them returns everything, so the result is nil too. No
// projection at all returns nothing beyond the members every projection
// carries, which is what a reader covered by no Grant would be owed — and a
// caller reaching that state has a predicate and a coverage check disagreeing,
// which is reported rather than answered.
func WidestProjection(projections []*Projection) *Projection {
	combined := Projection{}

	for _, projection := range projections {
		if projection == nil {
			return nil
		}

		for _, element := range projection.elements {
			if !slices.Contains(combined.elements, element) {
				combined.elements = append(combined.elements, element)
			}
		}
	}

	slices.Sort(combined.elements)

	return &combined
}
