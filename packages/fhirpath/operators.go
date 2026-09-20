package fhirpath

import (
	"fmt"
	"math"
	"strings"
)

// evaluateBinary applies an operator to both sides.
//
// The boolean operators are three-valued: FHIRPath says `false and {}` is
// false, because nothing the empty side could have been would change it, and
// `true and {}` is empty, because something would. An invariant written
// `a.exists() implies b.exists()` depends on exactly that.
func evaluateBinary(held Binary, ctx Context) ([]any, error) {
	switch held.Operator {
	case "and", "or", "xor", "implies":
		return evaluateLogical(held, ctx)
	}

	left, err := Evaluate(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	right, err := Evaluate(held.Right, ctx)
	if err != nil {
		return nil, err
	}

	switch held.Operator {
	case "|":
		return union(left, right), nil
	case "&":
		return []any{stringOf(left) + stringOf(right)}, nil
	case "=", "!=", "~", "!~":
		return equality(held.Operator, left, right), nil
	case "<", ">", "<=", ">=":
		return comparison(held.Operator, left, right)
	case "+", "-", "*", "/", "div", "mod":
		return arithmetic(held.Operator, left, right)
	case "in":
		return within(left, right), nil
	case "contains":
		return within(right, left), nil
	default:
		return nil, fmt.Errorf("%w: the operator %q", ErrUnsupported, held.Operator)
	}
}

// evaluateLogical applies a three-valued boolean operator.
func evaluateLogical(held Binary, ctx Context) ([]any, error) {
	left, err := booleanOf(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	// Short circuits that hold whatever the other side is.
	switch {
	case held.Operator == "and" && left != nil && !*left:
		return []any{false}, nil
	case held.Operator == "or" && left != nil && *left:
		return []any{true}, nil
	case held.Operator == "implies" && left != nil && !*left:
		return []any{true}, nil
	}

	right, err := booleanOf(held.Right, ctx)
	if err != nil {
		return nil, err
	}

	switch held.Operator {
	case "and":
		if left == nil || right == nil {
			if right != nil && !*right {
				return []any{false}, nil
			}

			return nil, nil
		}

		return []any{*left && *right}, nil

	case "or":
		if left == nil || right == nil {
			if right != nil && *right {
				return []any{true}, nil
			}

			return nil, nil
		}

		return []any{*left || *right}, nil

	case "xor":
		if left == nil || right == nil {
			return nil, nil
		}

		return []any{*left != *right}, nil

	default: // implies
		if right != nil && *right {
			return []any{true}, nil
		}

		if left == nil || right == nil {
			return nil, nil
		}

		return []any{!*left || *right}, nil
	}
}

// booleanOf evaluates one side of a logical operator to true, false or nothing.
func booleanOf(node Node, ctx Context) (*bool, error) {
	held, err := Evaluate(node, ctx)
	if err != nil {
		return nil, err
	}

	if len(held) != 1 {
		return nil, nil
	}

	value, is := held[0].(bool)
	if !is {
		return nil, nil
	}

	return &value, nil
}

// union concatenates, dropping what is already there: FHIRPath's | is a set
// union rather than an append.
func union(left, right []any) []any {
	found := make([]any, 0, len(left)+len(right))

	for _, one := range append(append([]any{}, left...), right...) {
		if !containsItem(found, one) {
			found = append(found, one)
		}
	}

	return found
}

// within reports whether every item on the left is on the right.
func within(left, right []any) []any {
	if len(left) == 0 {
		return nil
	}

	for _, one := range left {
		if !containsItem(right, one) {
			return []any{false}
		}
	}

	return []any{true}
}

// containsItem reports membership by value.
func containsItem(held []any, wanted any) bool {
	for _, one := range held {
		if sameValue(one, wanted) {
			return true
		}
	}

	return false
}

// equality compares two collections.
//
// Empty on either side is empty, not false: FHIRPath will not say two things
// are different when it does not have both of them.
func equality(operator string, left, right []any) []any {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}

	same := len(left) == len(right)

	if same {
		for i := range left {
			if !sameValue(left[i], right[i]) {
				same = false

				break
			}
		}
	}

	if operator == "!=" || operator == "!~" {
		return []any{!same}
	}

	return []any{same}
}

// sameValue compares two items, treating numbers of different Go types as the
// numbers they are and objects as equal when every member matches.
func sameValue(left, right any) bool {
	if leftNumber, ok := numberOf(left); ok {
		if rightNumber, ok := numberOf(right); ok {
			return leftNumber == rightNumber
		}

		return false
	}

	switch held := left.(type) {
	case map[string]any:
		other, is := right.(map[string]any)
		if !is || len(held) != len(other) {
			return false
		}

		for key, value := range held {
			if !sameValue(value, other[key]) {
				return false
			}
		}

		return true

	case []any:
		other, is := right.([]any)
		if !is || len(held) != len(other) {
			return false
		}

		for i := range held {
			if !sameValue(held[i], other[i]) {
				return false
			}
		}

		return true

	default:
		return left == right
	}
}

// comparison orders two singletons.
func comparison(operator string, left, right []any) ([]any, error) {
	if len(left) != 1 || len(right) != 1 {
		return nil, nil
	}

	if leftNumber, ok := numberOf(left[0]); ok {
		rightNumber, ok := numberOf(right[0])
		if !ok {
			return nil, nil
		}

		return []any{orders(operator, leftNumber > rightNumber, leftNumber == rightNumber)}, nil
	}

	leftText, ok := left[0].(string)
	if !ok {
		return nil, nil
	}

	rightText, ok := right[0].(string)
	if !ok {
		return nil, nil
	}

	return []any{orders(operator, leftText > rightText, leftText == rightText)}, nil
}

// orders settles one comparison from "greater" and "equal".
func orders(operator string, greater, equal bool) bool {
	switch operator {
	case "<":
		return !greater && !equal
	case ">":
		return greater
	case "<=":
		return !greater
	default: // >=
		return greater || equal
	}
}

// arithmetic applies a numeric operator, or concatenation for +.
func arithmetic(operator string, left, right []any) ([]any, error) {
	if len(left) != 1 || len(right) != 1 {
		return nil, nil
	}

	if operator == "+" {
		if leftText, is := left[0].(string); is {
			rightText, is := right[0].(string)
			if !is {
				return nil, nil
			}

			return []any{leftText + rightText}, nil
		}
	}

	leftNumber, ok := numberOf(left[0])
	if !ok {
		return nil, nil
	}

	rightNumber, ok := numberOf(right[0])
	if !ok {
		return nil, nil
	}

	switch operator {
	case "+":
		return []any{leftNumber + rightNumber}, nil
	case "-":
		return []any{leftNumber - rightNumber}, nil
	case "*":
		return []any{leftNumber * rightNumber}, nil
	case "/":
		if rightNumber == 0 {
			return nil, nil
		}

		return []any{leftNumber / rightNumber}, nil
	case "div":
		if rightNumber == 0 {
			return nil, nil
		}

		return []any{math.Trunc(leftNumber / rightNumber)}, nil
	default: // mod
		if rightNumber == 0 {
			return nil, nil
		}

		return []any{math.Mod(leftNumber, rightNumber)}, nil
	}
}

// stringOf renders a collection for &, which treats nothing as the empty
// string rather than propagating it.
func stringOf(held []any) string {
	var built strings.Builder

	for _, one := range held {
		built.WriteString(textOf(one))
	}

	return built.String()
}

// textOf renders one item.
func textOf(held any) string {
	switch value := held.(type) {
	case string:
		return value
	case bool:
		if value {
			return "true"
		}

		return "false"
	default:
		if number, ok := numberOf(held); ok {
			if number == math.Trunc(number) {
				return fmt.Sprintf("%d", int64(number))
			}

			return fmt.Sprintf("%g", number)
		}

		return ""
	}
}
