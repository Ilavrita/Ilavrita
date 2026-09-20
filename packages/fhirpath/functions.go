package fhirpath

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// evaluateCall applies a function to what precedes it.
//
// A function this subset does not implement is an error rather than an empty
// result, so an invariant written with one is reported as unevaluated instead
// of quietly passing.
func evaluateCall(held Call, ctx Context) ([]any, error) {
	from, err := leftOf(held.Left, ctx)
	if err != nil {
		return nil, err
	}

	switch held.Name {
	// Existence.
	case "empty":
		return []any{len(from) == 0}, nil
	case "exists":
		return exists(held, from, ctx)
	case "count":
		return []any{int64(len(from))}, nil
	case "not":
		return negate(from), nil
	case "allTrue", "anyTrue", "allFalse", "anyFalse":
		return quantified(held.Name, from), nil
	case "isDistinct":
		return []any{len(distinct(from)) == len(from)}, nil
	case "distinct":
		return distinct(from), nil
	case "single":
		if len(from) == 1 {
			return from, nil
		}

		return nil, nil

	// Filtering and projection.
	case "where":
		return filtered(held, from, ctx, true)
	case "select":
		return projected(held, from, ctx)
	case "all":
		return everyOne(held, from, ctx)
	case "ofType":
		return ofType(held, from)
	case "repeat", "aggregate":
		return nil, fmt.Errorf("%w: %s()", ErrUnsupported, held.Name)

	// Subsetting.
	case "first":
		return firstOf(from), nil
	case "last":
		if len(from) == 0 {
			return nil, nil
		}

		return []any{from[len(from)-1]}, nil
	case "tail":
		if len(from) < 2 {
			return nil, nil
		}

		return from[1:], nil
	case "skip", "take":
		return sliced(held, from, ctx)

	// Collections.
	case "subsetOf":
		return subsetOf(held, from, ctx, true)
	case "supersetOf":
		return subsetOf(held, from, ctx, false)
	case "combine", "union":
		return combined(held, from, ctx)
	case "intersect", "exclude":
		return nil, fmt.Errorf("%w: %s()", ErrUnsupported, held.Name)
	case "children", "descendants":
		return walked(from, held.Name == "descendants"), nil

	// Strings.
	case "matches", "startsWith", "endsWith", "contains", "indexOf", "replace",
		"substring", "length", "upper", "lower", "toChars", "replaceMatches":
		return stringFunction(held, from, ctx)

	// Conversion.
	case "toInteger":
		return toInteger(from), nil
	case "toDecimal":
		return toDecimal(from), nil
	case "toString":
		if len(from) != 1 {
			return nil, nil
		}

		return []any{textOf(from[0])}, nil
	case "hasValue":
		return []any{len(from) == 1 && !isObject(from[0])}, nil
	case "convertsToInteger":
		return []any{len(toInteger(from)) == 1}, nil

	// Utility.
	case "trace":
		// A debugging aid that returns what it was given. Evaluating it as a
		// no-op is exactly what it does.
		return from, nil
	case "iif":
		return conditional(held, ctx)
	case "extension":
		return extensions(held, from, ctx)

	default:
		return nil, fmt.Errorf("%w: %s()", ErrUnsupported, held.Name)
	}
}

// exists is empty().not(), optionally filtered first.
func exists(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) == 0 {
		return []any{len(from) > 0}, nil
	}

	kept, err := filtered(held, from, ctx, true)
	if err != nil {
		return nil, err
	}

	return []any{len(kept) > 0}, nil
}

// negate inverts a singleton boolean.
func negate(from []any) []any {
	if len(from) != 1 {
		return nil
	}

	value, is := from[0].(bool)
	if !is {
		return nil
	}

	return []any{!value}
}

// quantified answers allTrue and its siblings.
func quantified(name string, from []any) []any {
	wantTrue := name == "allTrue" || name == "anyTrue"
	all := name == "allTrue" || name == "allFalse"

	found := !all

	for _, one := range from {
		value, is := one.(bool)
		if !is {
			continue
		}

		if all && value != wantTrue {
			return []any{false}
		}

		if !all && value == wantTrue {
			found = true
		}
	}

	return []any{found || all}
}

// distinct drops repeats by value.
func distinct(from []any) []any {
	found := make([]any, 0, len(from))

	for _, one := range from {
		if !containsItem(found, one) {
			found = append(found, one)
		}
	}

	return found
}

// firstOf returns the first item, or nothing.
func firstOf(from []any) []any {
	if len(from) == 0 {
		return nil
	}

	return from[:1]
}

// filtered keeps the items an argument holds for.
func filtered(held Call, from []any, ctx Context, keep bool) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: %s() takes one argument", ErrUnsupported, held.Name)
	}

	var found []any

	for _, one := range from {
		settled, err := booleanOf(held.Arguments[0], Context{This: one, Resource: ctx.Resource})
		if err != nil {
			return nil, err
		}

		if settled != nil && *settled == keep {
			found = append(found, one)
		}
	}

	return found, nil
}

// projected evaluates an argument against each item and flattens the results.
func projected(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: select() takes one argument", ErrUnsupported)
	}

	var found []any

	for _, one := range from {
		held, err := Evaluate(held.Arguments[0], Context{This: one, Resource: ctx.Resource})
		if err != nil {
			return nil, err
		}

		found = append(found, held...)
	}

	return found, nil
}

// everyOne reports whether an argument holds for every item. An empty
// collection satisfies it, which is what "all of nothing" means.
func everyOne(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: all() takes one argument", ErrUnsupported)
	}

	for _, one := range from {
		settled, err := booleanOf(held.Arguments[0], Context{This: one, Resource: ctx.Resource})
		if err != nil {
			return nil, err
		}

		if settled == nil || !*settled {
			return []any{false}, nil
		}
	}

	return []any{true}, nil
}

// ofType keeps the items of one type.
func ofType(held Call, from []any) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: ofType() takes one argument", ErrUnsupported)
	}

	named, ok := held.Arguments[0].(Path)
	if !ok || named.Left != nil {
		return nil, fmt.Errorf("%w: ofType() takes a type name", ErrUnsupported)
	}

	var found []any

	for _, one := range from {
		if isOfType(one, named.Name) {
			found = append(found, one)
		}
	}

	return found, nil
}

// sliced applies skip and take.
func sliced(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: %s() takes one argument", ErrUnsupported, held.Name)
	}

	count, err := Evaluate(held.Arguments[0], ctx)
	if err != nil {
		return nil, err
	}

	if len(count) != 1 {
		return nil, nil
	}

	at, ok := integerOf(count[0])
	if !ok || at < 0 {
		return nil, nil
	}

	if at > int64(len(from)) {
		at = int64(len(from))
	}

	if held.Name == "skip" {
		return from[at:], nil
	}

	return from[:at], nil
}

// subsetOf compares two collections by membership.
func subsetOf(held Call, from []any, ctx Context, forward bool) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: %s() takes one argument", ErrUnsupported, held.Name)
	}

	other, err := Evaluate(held.Arguments[0], ctx)
	if err != nil {
		return nil, err
	}

	smaller, larger := from, other
	if !forward {
		smaller, larger = other, from
	}

	for _, one := range smaller {
		if !containsItem(larger, one) {
			return []any{false}, nil
		}
	}

	return []any{true}, nil
}

// combined appends or unions another collection.
func combined(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: %s() takes one argument", ErrUnsupported, held.Name)
	}

	other, err := Evaluate(held.Arguments[0], ctx)
	if err != nil {
		return nil, err
	}

	if held.Name == "union" {
		return union(from, other), nil
	}

	return append(append([]any{}, from...), other...), nil
}

// walked returns the members of every item, and optionally their members too.
func walked(from []any, deep bool) []any {
	var found []any

	for _, one := range from {
		object, is := one.(map[string]any)
		if !is {
			continue
		}

		for _, value := range object {
			items := itemsOf(value)
			found = append(found, items...)

			if deep {
				found = append(found, walked(items, true)...)
			}
		}
	}

	return found
}

// conditional applies iif(test, then, otherwise).
func conditional(held Call, ctx Context) ([]any, error) {
	if len(held.Arguments) < 2 || len(held.Arguments) > 3 {
		return nil, fmt.Errorf("%w: iif() takes two or three arguments", ErrUnsupported)
	}

	settled, err := booleanOf(held.Arguments[0], ctx)
	if err != nil {
		return nil, err
	}

	if settled != nil && *settled {
		return Evaluate(held.Arguments[1], ctx)
	}

	if len(held.Arguments) == 3 {
		return Evaluate(held.Arguments[2], ctx)
	}

	return nil, nil
}

// extensions keeps the extensions with one url.
func extensions(held Call, from []any, ctx Context) ([]any, error) {
	if len(held.Arguments) != 1 {
		return nil, fmt.Errorf("%w: extension() takes one argument", ErrUnsupported)
	}

	wanted, err := Evaluate(held.Arguments[0], ctx)
	if err != nil {
		return nil, err
	}

	if len(wanted) != 1 {
		return nil, nil
	}

	var found []any

	for _, one := range from {
		for _, extension := range memberOf(one, "extension") {
			if object, is := extension.(map[string]any); is && sameValue(object["url"], wanted[0]) {
				found = append(found, extension)
			}
		}
	}

	return found, nil
}

// toInteger converts a singleton to a whole number.
func toInteger(from []any) []any {
	if len(from) != 1 {
		return nil
	}

	if value, ok := integerOf(from[0]); ok {
		return []any{value}
	}

	if text, is := from[0].(string); is {
		if value, err := strconv.ParseInt(text, 10, 64); err == nil {
			return []any{value}
		}
	}

	return nil
}

// toDecimal converts a singleton to a number.
func toDecimal(from []any) []any {
	if len(from) != 1 {
		return nil
	}

	if value, ok := numberOf(from[0]); ok {
		return []any{value}
	}

	if text, is := from[0].(string); is {
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			return []any{value}
		}
	}

	return nil
}

// isObject reports whether an item is a structure rather than a value, which is
// what hasValue() asks.
func isObject(held any) bool {
	_, is := held.(map[string]any)

	return is
}

// stringFunction applies the string functions, which all read one subject.
func stringFunction(held Call, from []any, ctx Context) ([]any, error) {
	if len(from) != 1 {
		return nil, nil
	}

	subject, is := from[0].(string)
	if !is {
		return nil, nil
	}

	if held.Name == "length" {
		return []any{int64(len(subject))}, nil
	}

	if held.Name == "upper" {
		return []any{strings.ToUpper(subject)}, nil
	}

	if held.Name == "lower" {
		return []any{strings.ToLower(subject)}, nil
	}

	if held.Name == "toChars" {
		found := make([]any, 0, len(subject))
		for _, r := range subject {
			found = append(found, string(r))
		}

		return found, nil
	}

	arguments, err := stringArguments(held, ctx)
	if err != nil {
		return nil, err
	}

	if arguments == nil {
		return nil, nil
	}

	return applyString(held.Name, subject, arguments)
}

// stringArguments evaluates a string function's arguments to strings, or
// reports that one of them was not a single value.
func stringArguments(held Call, ctx Context) ([]string, error) {
	found := make([]string, 0, len(held.Arguments))

	for _, argument := range held.Arguments {
		value, err := Evaluate(argument, ctx)
		if err != nil {
			return nil, err
		}

		if len(value) != 1 {
			return nil, nil
		}

		found = append(found, textOf(value[0]))
	}

	return found, nil
}

// applyString is the string functions themselves.
func applyString(name, subject string, arguments []string) ([]any, error) {
	switch name {
	case "matches", "replaceMatches":
		expression, err := regexp.Compile(arguments[0])
		if err != nil {
			// A pattern the specification wrote and this build cannot compile
			// is not a resource that failed: it is a rule nobody applied.
			return nil, fmt.Errorf("%w: the pattern %q: %w", ErrUnsupported, arguments[0], err)
		}

		if name == "matches" {
			return []any{expression.MatchString(subject)}, nil
		}

		if len(arguments) != 2 {
			return nil, fmt.Errorf("%w: replaceMatches() takes two arguments", ErrUnsupported)
		}

		return []any{expression.ReplaceAllString(subject, arguments[1])}, nil

	case "startsWith":
		return []any{strings.HasPrefix(subject, arguments[0])}, nil
	case "endsWith":
		return []any{strings.HasSuffix(subject, arguments[0])}, nil
	case "contains":
		return []any{strings.Contains(subject, arguments[0])}, nil
	case "indexOf":
		return []any{int64(strings.Index(subject, arguments[0]))}, nil
	case "replace":
		if len(arguments) != 2 {
			return nil, fmt.Errorf("%w: replace() takes two arguments", ErrUnsupported)
		}

		return []any{strings.ReplaceAll(subject, arguments[0], arguments[1])}, nil

	case "substring":
		start, err := strconv.Atoi(arguments[0])
		if err != nil || start < 0 || start >= len(subject) {
			return nil, nil
		}

		if len(arguments) == 1 {
			return []any{subject[start:]}, nil
		}

		length, err := strconv.Atoi(arguments[1])
		if err != nil || length < 0 {
			return nil, nil
		}

		if start+length > len(subject) {
			length = len(subject) - start
		}

		return []any{subject[start : start+length]}, nil

	default:
		return nil, fmt.Errorf("%w: %s()", ErrUnsupported, name)
	}
}
