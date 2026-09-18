package search

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrMissingParameterName reports a parameter with no name.
	ErrMissingParameterName = errors.New("search: a parameter needs a name")

	// ErrUnknownKind reports a parameter kind this build does not match on.
	ErrUnknownKind = errors.New("search: unknown parameter kind")

	// ErrParameterSource reports a parameter that names neither an element to
	// project from nor a column to read. One or the other answers it; a
	// parameter answering from nothing would match nothing while looking like a
	// restriction somebody stated.
	ErrParameterSource = errors.New("search: a parameter reads one element or one column")
)

// Kind is how a parameter's values are matched. It decides both what is
// projected on write and what predicate a search compiles, so the two cannot
// disagree about what a parameter means.
type Kind string

// The kinds this build matches on.
const (
	// KindToken matches a code exactly, optionally qualified by its system as
	// "system|code". It is what status, category and identifier are.
	KindToken Kind = "token"

	// KindString matches the start of a human name or address, case-folded. It
	// is deliberately not a substring match: a search that scans every value is
	// one a large Project cannot serve.
	KindString Kind = "string"

	// KindReference matches a "Type/id" link.
	KindReference Kind = "reference"

	// KindDate matches an instant against a range.
	KindDate Kind = "date"
)

var knownKinds = []Kind{KindToken, KindString, KindReference, KindDate}

// Parameter is one search parameter this build implements. A parameter absent
// from the registry is refused rather than ignored, so nothing this server
// cannot apply is quietly dropped from a query (SRC-4).
type Parameter struct {
	name   string
	kind   Kind
	path   []string
	code   string
	system string
	column string
}

// Token declares a coded parameter.
//
// A token has two halves in FHIR — the system that defines the code and the
// code itself — and they have to be read from the same element or a search
// could pair one coding's system with another's code. So a token names the
// element holding both and the members within it: "category.coding" with "code"
// and "system", or "status" with neither, because a plain status is the code.
func Token(name, element, codeMember, systemMember string) (Parameter, error) {
	held, err := projected(name, KindToken, element)
	if err != nil {
		return Parameter{}, err
	}

	if held.code, err = optionalMember(name, codeMember); err != nil {
		return Parameter{}, err
	}

	if held.system, err = optionalMember(name, systemMember); err != nil {
		return Parameter{}, err
	}

	return held, nil
}

// optionalMember reads one member name within a token's element, absent when
// the element is itself the value.
func optionalMember(parameter, member string) (string, error) {
	if member == "" {
		return "", nil
	}

	if _, err := storage.ParseElementPath(member); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrParameterSource, parameter, err)
	}

	return member, nil
}

// Text declares a parameter matched against the start of a human-readable
// value, case-folded.
func Text(name, path string) (Parameter, error) {
	return projected(name, KindString, path)
}

// Reference declares a parameter matched against a "Type/id" link.
func Reference(name, path string) (Parameter, error) {
	return projected(name, KindReference, path)
}

// Date declares a parameter matched against an instant.
func Date(name, path string) (Parameter, error) {
	return projected(name, KindDate, path)
}

// projected declares a parameter read out of the resource's own content, which
// is what a write projects into the index.
func projected(name string, kind Kind, path string) (Parameter, error) {
	if name == "" {
		return Parameter{}, ErrMissingParameterName
	}

	if !slices.Contains(knownKinds, kind) {
		return Parameter{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(kind))
	}

	segments, err := storage.ParseElementPath(path)
	if err != nil {
		return Parameter{}, fmt.Errorf("%w: %s: %w", ErrParameterSource, name, err)
	}

	return Parameter{name: name, kind: kind, path: segments}, nil
}

// Stored declares a parameter answered from the row itself rather than from
// anything projected out of its content, which is what _id and _lastUpdated
// are: the store already holds them, and projecting a second copy would be a
// second thing to keep in step.
func Stored(name string, kind Kind, column string) (Parameter, error) {
	if name == "" {
		return Parameter{}, ErrMissingParameterName
	}

	if !slices.Contains(knownKinds, kind) {
		return Parameter{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(kind))
	}

	if column == "" {
		return Parameter{}, fmt.Errorf("%w: %s names no column", ErrParameterSource, name)
	}

	return Parameter{name: name, kind: kind, column: column}, nil
}

// CodeMember returns the member holding a token's code within the element the
// path reaches, empty when the element is itself the code.
func (p Parameter) CodeMember() string { return p.code }

// SystemMember returns the member holding a token's system, empty when the
// parameter indexes none.
func (p Parameter) SystemMember() string { return p.system }

// Name returns the parameter as a query states it.
func (p Parameter) Name() string { return p.name }

// Kind returns how its values are matched.
func (p Parameter) Kind() Kind { return p.kind }

// Path returns the element a write projects from, empty on a stored parameter.
func (p Parameter) Path() []string { return slices.Clone(p.path) }

// Column returns the row column a search reads, empty on a projected parameter.
func (p Parameter) Column() string { return p.column }

// Projects reports whether a write has to index this parameter.
func (p Parameter) Projects() bool { return p.column == "" }

// IsZero reports whether this is a Parameter nobody declared.
func (p Parameter) IsZero() bool { return p.name == "" }
