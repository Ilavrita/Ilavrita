package fhirpath

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// ErrUnsupported reports a construct this subset does not evaluate.
//
// It is an error rather than an empty result on purpose. An invariant that
// could not be evaluated must not be reported as one that passed: a caller has
// to be able to tell "this resource is fine" from "nobody checked".
var ErrUnsupported = errors.New("fhirpath: this build does not evaluate that")

// Context is what an expression is evaluated against.
type Context struct {
	// This is the node the expression starts from — the element an invariant is
	// attached to, which for most of them is the resource itself.
	This any

	// Resource is what %resource reads, which is the whole resource however
	// deep inside it the invariant sits.
	Resource any
}

// Evaluate returns the collection an expression produces.
//
// Everything in FHIRPath is a collection, including a single value and
// including nothing, which is what makes `name.family` work whether a resource
// holds one name or five.
func Evaluate(node Node, ctx Context) ([]any, error) {
	switch held := node.(type) {
	case Literal:
		if held.Value == nil {
			return nil, nil
		}

		return []any{held.Value}, nil

	case Variable:
		return evaluateVariable(held, ctx)

	case Path:
		return evaluatePath(held, ctx)

	case Call:
		return evaluateCall(held, ctx)

	case Index:
		return evaluateIndex(held, ctx)

	case Unary:
		return evaluateUnary(held, ctx)

	case Binary:
		return evaluateBinary(held, ctx)

	case TypeTest:
		return evaluateTypeTest(held, ctx)

	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupported, node)
	}
}

// Holds reports whether an expression is satisfied.
//
// Only an explicit false fails. An expression that produces nothing is one this
// evaluator could not decide — a path into something absent, most often — and
// refusing a resource on that would refuse resources nobody could show were
// wrong.
func Holds(node Node, ctx Context) (bool, error) {
	held, err := Evaluate(node, ctx)
	if err != nil {
		return false, err
	}

	if len(held) != 1 {
		return true, nil
	}

	settled, isBoolean := held[0].(bool)

	return !isBoolean || settled, nil
}

// evaluateVariable reads %resource, %context or $this.
func evaluateVariable(held Variable, ctx Context) ([]any, error) {
	switch held.Name {
	case "%resource", "%context", "%rootResource":
		return itemsOf(ctx.Resource), nil
	case "$this":
		return itemsOf(ctx.This), nil
	default:
		return nil, fmt.Errorf("%w: the variable %s", ErrUnsupported, held.Name)
	}
}

// evaluatePath navigates one step.
func evaluatePath(held Path, ctx Context) ([]any, error) {
	from, err := leftOf(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	var found []any

	for _, one := range from {
		found = append(found, memberOf(one, held.Name)...)
	}

	return found, nil
}

// leftOf evaluates what a step navigates from, which is the context when the
// step begins an expression.
func leftOf(left Node, ctx Context) ([]any, error) {
	if left == nil {
		return itemsOf(ctx.This), nil
	}

	return Evaluate(left, ctx)
}

// memberOf reads one named member, flattening a repeating element the way
// FHIRPath does: a resource with three names and one with a single name are
// navigated identically.
func memberOf(one any, name string) []any {
	object, isObject := one.(map[string]any)
	if !isObject {
		return nil
	}

	if held, carried := object[name]; carried {
		return itemsOf(held)
	}

	// A choice is written under the name plus its type — "value" is present as
	// "valueQuantity" — and R4 writes the expression against the name alone.
	for key, held := range object {
		if len(key) <= len(name) || !strings.HasPrefix(key, name) {
			continue
		}

		if unicode.IsUpper(rune(key[len(name)])) {
			return itemsOf(held)
		}
	}

	return nil
}

// itemsOf turns a value into the collection it stands for. A JSON array is its
// members; anything else is itself; nothing is nothing.
func itemsOf(held any) []any {
	switch value := held.(type) {
	case nil:
		return nil
	case []any:
		found := make([]any, 0, len(value))
		for _, one := range value {
			if one != nil {
				found = append(found, one)
			}
		}

		return found
	default:
		return []any{value}
	}
}

// evaluateIndex applies a subscript.
func evaluateIndex(held Index, ctx Context) ([]any, error) {
	from, err := leftOf(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	at, err := Evaluate(held.Where, ctx)
	if err != nil {
		return nil, err
	}

	if len(at) != 1 {
		return nil, nil
	}

	position, ok := integerOf(at[0])
	if !ok || position < 0 || int(position) >= len(from) {
		return nil, nil
	}

	return []any{from[position]}, nil
}

// evaluateUnary applies a sign.
func evaluateUnary(held Unary, ctx Context) ([]any, error) {
	operand, err := Evaluate(held.Operand, ctx)
	if err != nil {
		return nil, err
	}

	if len(operand) != 1 {
		return nil, nil
	}

	number, ok := numberOf(operand[0])
	if !ok {
		return nil, nil
	}

	return []any{-number}, nil
}

// evaluateTypeTest applies `is` or `as`.
//
// The type of a JSON value is only as specific as JSON is, so this answers for
// the types an invariant actually tests: the primitives, and a resource by its
// resourceType.
func evaluateTypeTest(held TypeTest, ctx Context) ([]any, error) {
	from, err := Evaluate(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	if len(from) != 1 {
		if held.Operator == "is" {
			return []any{false}, nil
		}

		return nil, nil
	}

	matches := isOfType(from[0], held.Type)

	if held.Operator == "is" {
		return []any{matches}, nil
	}

	if matches {
		return from, nil
	}

	return nil, nil
}

// isOfType reports whether a value is of the named FHIRPath or FHIR type.
func isOfType(held any, named string) bool {
	switch strings.TrimPrefix(strings.TrimPrefix(named, "System."), "FHIR.") {
	case "Boolean", "boolean":
		_, is := held.(bool)

		return is
	case "String", "string", "code", "uri", "id", "markdown", "canonical":
		_, is := held.(string)

		return is
	case "Integer", "integer", "positiveInt", "unsignedInt":
		value, is := numberOf(held)

		return is && value == math.Trunc(value)
	case "Decimal", "decimal":
		_, is := numberOf(held)

		return is
	case "Quantity", "dateTime", "date", "time", "instant":
		// A Quantity is an object; the temporal primitives are strings. Both
		// are tested by shape, which is as far as JSON says.
		if _, is := held.(map[string]any); is && named == "Quantity" {
			return true
		}

		_, is := held.(string)

		return is && named != "Quantity"
	default:
		object, is := held.(map[string]any)
		if !is {
			return false
		}

		return object["resourceType"] == named
	}
}

// integerOf reads a whole number.
func integerOf(held any) (int64, bool) {
	value, ok := numberOf(held)
	if !ok || value != math.Trunc(value) {
		return 0, false
	}

	return int64(value), true
}

// numberOf reads a number however JSON delivered it.
func numberOf(held any) (float64, bool) {
	switch value := held.(type) {
	case float64:
		return value, true
	case int64:
		return float64(value), true
	case int:
		return float64(value), true
	default:
		return 0, false
	}
}
