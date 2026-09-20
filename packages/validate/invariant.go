package validate

import (
	"sync"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhirpath"
)

// Invariants are the rules R4 states that cardinality and datatypes cannot.
//
// "An Organization SHALL have a name or an identifier" is not a fact about
// either element: both are optional on their own, and the rule is about the
// resource. R4 writes those as FHIRPath, and this is where they are applied.
//
// An invariant this build cannot evaluate is never counted as one that passed.
// It is left out of checking and named by Unevaluable, so the gap is a list
// somebody can read rather than a silence.

// compiled caches one parsed expression. Invariants are evaluated on every
// write, and the same few hundred expressions would otherwise be parsed again
// for every resource.
var compiled sync.Map

// parseOnce returns the parsed form of one expression.
func parseOnce(expression string) (fhirpath.Node, error) {
	if held, found := compiled.Load(expression); found {
		if failed, isError := held.(error); isError {
			return nil, failed
		}

		return held.(fhirpath.Node), nil
	}

	parsed, err := fhirpath.Parse(expression)
	if err != nil {
		compiled.Store(expression, err)

		return nil, err
	}

	compiled.Store(expression, parsed)

	return parsed, nil
}

// checkInvariants holds one object to the rules stated about it.
//
// The object is the context: an invariant on Organization.address is about each
// address, and one on Organization is about the resource. That is what lets
// `where(use = 'home').empty()` mean "this address is not a home address".
func (r *Report) checkInvariants(held []conformance.Constraint, node any, where string) {
	for _, one := range held {
		if !one.Applies() {
			continue
		}

		parsed, err := parseOnce(one.Expression)
		if err != nil {
			// Nothing is reported against the resource. The expression is the
			// specification's, not the client's, and a client cannot act on a
			// rule this server could not read.
			continue
		}

		satisfied, err := fhirpath.Holds(parsed, fhirpath.Context{This: node, Resource: r.resource})
		if err != nil {
			continue
		}

		if satisfied {
			continue
		}

		severity := SeverityWarning
		if one.Required() {
			severity = SeverityError
		}

		// Cited by the name R4 gives it, because that is what the specification
		// and every other implementation call this rule — and then in the
		// specification's own words, because most people do not read FHIRPath.
		r.note(severity, where, one.Key+": "+one.Human)
	}
}

// Unevaluable returns the invariants this build cannot apply, by the name R4
// gives each one.
//
// It exists so the gap can be asserted rather than discovered: a rule that
// quietly stopped being checked is indistinguishable from one that passes.
func Unevaluable() ([]string, error) {
	model, err := conformance.Definitions()
	if err != nil {
		return nil, err
	}

	var unevaluable []string

	for _, name := range model.Types() {
		structure, defined := model.Structure(name)
		if !defined {
			continue
		}

		held := append([]conformance.Constraint{}, structure.Root.Constraints...)
		for _, element := range structure.Elements {
			held = append(held, element.Constraints...)
		}

		for _, one := range held {
			if _, err := parseOnce(one.Expression); err != nil {
				unevaluable = appendOnce(unevaluable, one.Key)
			}
		}
	}

	return unevaluable, nil
}

// appendOnce keeps a name once however many types state the same rule.
func appendOnce(held []string, name string) []string {
	for _, one := range held {
		if one == name {
			return held
		}
	}

	return append(held, name)
}
