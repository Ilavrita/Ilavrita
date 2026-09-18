package validate

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

// primitiveShapes is what each R4 primitive type's value has to look like.
//
// Only the ones whose shape a string can be wrong about are here. A "string" or
// a "markdown" is any non-empty string, which the representation rules already
// hold, so naming them would add a pattern that never fails.
var primitiveShapes = map[string]*regexp.Regexp{
	"date":         regexp.MustCompile(`^[0-9]{4}(-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01]))?)?$`),
	"dateTime":     datePattern,
	"instant":      regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\.[0-9]+)?(Z|[+-]((0[0-9]|1[0-3]):[0-5][0-9]|14:00))$`),
	"time":         regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\.[0-9]+)?$`),
	"code":         regexp.MustCompile(`^[^\s]+( [^\s]+)*$`),
	"id":           idPattern,
	"oid":          regexp.MustCompile(`^urn:oid:[0-2](\.(0|[1-9][0-9]*))+$`),
	"uuid":         regexp.MustCompile(`^urn:uuid:[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`),
	"base64Binary": regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`),
}

// numericTypes are the primitives R4 writes as JSON numbers rather than strings.
var numericTypes = map[string]bool{
	"integer": true, "decimal": true, "positiveInt": true, "unsignedInt": true,
}

// terminology is what this build can decide a code against. A build that cannot
// read its own value sets checks no binding, which is silence rather than a
// refusal: the sets are what this server knows, not what a client did wrong.
var terminology = sync.OnceValue(func() conformance.Terminology {
	held, err := conformance.Terminologies()
	if err != nil {
		return conformance.Terminology{}
	}

	return held
})

// againstDefinition checks a resource against what its own definition says it
// may hold.
//
// This is where validation stops being about how JSON represents FHIR and starts
// being about what an Observation is. Without a definition there is nothing to
// say — a type this build has no definition for is checked by the rules above
// and no further — so a missing one is silence rather than a refusal.
func (r *Report) againstDefinition(model conformance.Model, resourceType string, held map[string]any) {
	structure, defined := model.Structure(resourceType)
	if !defined {
		return
	}

	r.checkObject(model, structure, "", held, resourceType, 0)
}

// checkObject holds one object to the elements declared beneath one path.
func (r *Report) checkObject(
	model conformance.Model,
	structure conformance.Structure,
	under string,
	held map[string]any,
	where string,
	depth int,
) {
	if depth > maximumDepth {
		return
	}

	for name, value := range held {
		r.checkMember(model, structure, under, name, value, where, depth)
	}

	r.checkRequired(structure, under, held, where)
	r.checkOneOfEachChoice(structure, under, held, where)
}

// checkOneOfEachChoice refuses two members of the same choice. An element
// written "value[x]" holds one value; a resource carrying valueString beside
// valueQuantity is saying two things where it may say one, and nothing
// downstream could tell which was meant.
func (r *Report) checkOneOfEachChoice(
	structure conformance.Structure, under string, held map[string]any, where string,
) {
	for prefix, element := range structure.Choices(under) {
		var written []string

		for _, code := range element.Types {
			if _, present := held[prefix+raised(code)]; present {
				written = append(written, prefix+raised(code))
			}
		}

		if len(written) > 1 {
			slices.Sort(written)
			r.note(SeverityError, where+"."+prefix+"[x]",
				"One of these may be present, and "+strings.Join(written, ", ")+" are.")
		}
	}
}

// checkMember resolves one member against the definition and descends into it.
func (r *Report) checkMember(
	model conformance.Model,
	structure conformance.Structure,
	under string,
	name string,
	value any,
	where string,
	depth int,
) {
	// The members every resource carries that no element declares. resourceType
	// is JSON's way of naming the type and is not an element of it.
	if under == "" && name == "resourceType" {
		return
	}

	// A primitive's extensions are written under the same name with an
	// underscore, so what they belong to is the element without it.
	named := strings.TrimPrefix(name, "_")
	extension := named != name

	element, path, found := resolve(structure, under, named)
	if !found {
		r.note(SeverityError, where+"."+name,
			"No such element. "+resourceType(structure)+" declares "+
				stated(structure.Names(under))+".")

		return
	}

	if extension {
		// What an extension on a primitive holds is an Element, which this
		// build does not model beyond the representation rules already applied.
		return
	}

	r.checkCardinality(element, value, where+"."+name)
	r.checkBinding(element, value, where+"."+name)

	// An element this build cannot say what is inside is left alone. Walking
	// into one would report every element it holds as one nobody declared,
	// which is a refusal for something the resource got right.
	if element.Opaque {
		return
	}

	for _, each := range members(value) {
		r.checkDefined(model, structure, path, element, name, each.value, where+"."+name+each.at, depth)
	}
}

// resolve finds the element one member names, which is the member itself or the
// choice whose type it carries.
func resolve(
	structure conformance.Structure, under, name string,
) (conformance.Element, string, bool) {
	path := name
	if under != "" {
		path = under + "." + name
	}

	if element, found := structure.Element(path); found {
		return element, path, true
	}

	// A choice is written "value[x]" and present as "valueQuantity". The suffix
	// is the type code with its first letter raised, which is how R4 spells it.
	for prefix, element := range structure.Choices(under) {
		suffix, carries := strings.CutPrefix(name, prefix)
		if !carries || suffix == "" {
			continue
		}

		for _, code := range element.Types {
			if suffix != raised(code) {
				continue
			}

			chosen := element
			chosen.Types = []string{code}

			choicePath := prefix + "[x]"
			if under != "" {
				choicePath = under + "." + choicePath
			}

			return chosen, choicePath, true
		}
	}

	return conformance.Element{}, "", false
}

// raised is a type code as a choice member spells it.
func raised(code string) string {
	if code == "" {
		return ""
	}

	return strings.ToUpper(code[:1]) + code[1:]
}

// held is one value of a member, and where it is if the member repeats.
type heldValue struct {
	value any
	at    string
}

// members opens an array into its values, so a repeating element and a single
// one are checked alike.
func members(value any) []heldValue {
	list, repeated := value.([]any)
	if !repeated {
		return []heldValue{{value: value}}
	}

	held := make([]heldValue, 0, len(list))
	for index, each := range list {
		held = append(held, heldValue{value: each, at: "[" + strconv.Itoa(index) + "]"})
	}

	return held
}

// checkBinding holds a coded element to the value set it is bound to.
//
// Only a required binding is a rule. An extensible one says a code should come
// from the set and R4 permits another; a preferred or example one is a
// suggestion. Refusing any of those would refuse resources the specification
// allows.
//
// A set this build did not resolve decides nothing. R4 binds elements to MIME
// types, to UCUM units and to sets published elsewhere, and a code in one of
// those is unchecked rather than refused.
func (r *Report) checkBinding(element conformance.Element, value any, where string) {
	if !element.Binding.Required() {
		return
	}

	admitted, resolved := terminology().Admits(element.Binding.ValueSet)
	if !resolved {
		return
	}

	for _, each := range members(value) {
		r.checkCoded(admitted, element, each.value, where+each.at)
	}
}

// checkCoded holds one value to the set, whether it is written as a bare code or
// inside a Coding.
func (r *Report) checkCoded(
	admitted conformance.Admitted, element conformance.Element, value any, where string,
) {
	switch held := value.(type) {
	case string:
		// A `code` carries no system of its own: the binding is what says which
		// system it is in.
		if !admitted.Holds(conformance.Coded{Code: held}) {
			r.note(SeverityError, where, notInTheSet(element))
		}

	case map[string]any:
		// A Coding names its own system; a CodeableConcept holds Codings.
		for _, coding := range codingsIn(held) {
			if coding.Code == "" {
				continue
			}

			if !admitted.Holds(coding) {
				r.note(SeverityError, where, notInTheSet(element))
			}
		}
	}
}

// codingsIn reads the codes one object states, whether it is a Coding itself or
// a CodeableConcept holding them.
func codingsIn(held map[string]any) []conformance.Coded {
	if code, stated := held["code"].(string); stated {
		system, _ := held["system"].(string)

		return []conformance.Coded{{System: system, Code: code}}
	}

	list, nested := held["coding"].([]any)
	if !nested {
		return nil
	}

	var found []conformance.Coded

	for _, each := range list {
		coding, isObject := each.(map[string]any)
		if !isObject {
			continue
		}

		found = append(found, codingsIn(coding)...)
	}

	return found
}

// notInTheSet says which set a code was judged against, because a refusal that
// only says "not allowed" leaves a client with nowhere to look.
func notInTheSet(element conformance.Element) string {
	return "This element is bound to " + element.Binding.ValueSet +
		", which does not hold that code."
}

// checkCardinality holds a member to how many of it there may be.
//
// R4's JSON writes a repeating element as an array and a single one as the value
// itself, always: one of two members is not a matter of taste, it is what tells
// a reader whether more may follow.
func (r *Report) checkCardinality(element conformance.Element, value any, where string) {
	_, isArray := value.([]any)

	switch {
	case element.Max == "0":
		r.note(SeverityError, where, "This element is not permitted here.")
	case element.Repeats() && !isArray:
		r.note(SeverityError, where, "This element repeats, so it is written as an array.")
	case !element.Repeats() && isArray:
		r.note(SeverityError, where, "This element occurs once, so it is not written as an array.")
	}
}

// checkRequired reports the elements the definition says a resource must carry.
func (r *Report) checkRequired(
	structure conformance.Structure, under string, held map[string]any, where string,
) {
	for _, name := range structure.Names(under) {
		path := name
		if under != "" {
			path = under + "." + name
		}

		element, found := structure.Element(path)
		if !found {
			// A choice: declared under its [x] name.
			element, found = structure.Element(path + "[x]")
		}

		if !found || !element.Required() {
			continue
		}

		if !carries(held, name, element) {
			r.note(SeverityError, where+"."+name, "This element is required.")
		}
	}
}

// carries reports whether an object holds one element, under its own name or
// under whichever type a choice was written with.
func carries(held map[string]any, name string, element conformance.Element) bool {
	if _, present := held[name]; present {
		return true
	}

	if !element.Choice {
		return false
	}

	for _, code := range element.Types {
		if _, present := held[name+raised(code)]; present {
			return true
		}
	}

	return false
}

// checkDefined descends into one value of one member.
func (r *Report) checkDefined(
	model conformance.Model,
	structure conformance.Structure,
	path string,
	element conformance.Element,
	name string,
	value any,
	where string,
	depth int,
) {
	held, isObject := value.(map[string]any)

	// One type, and it is another definition's: a CodeableConcept's elements are
	// in CodeableConcept, not in whatever holds one.
	if len(element.Types) == 1 {
		r.checkTyped(model, element.Types[0], value, where, depth)
	}

	if !isObject {
		return
	}

	// A contained or referenced resource is checked as what it says it is, not
	// as an element of what holds it.
	if len(element.Types) == 1 && isResourceType(element.Types[0]) {
		return
	}

	// A backbone element's children are declared in this same structure, beneath
	// this same path. BackboneElement and Element are definitions of their own —
	// they declare an id and extensions and nothing else — so resolving one as a
	// datatype would check a resource against three elements it has hundreds of.
	if len(element.Types) == 1 && !declaredInPlace(element.Types[0]) {
		if named, defined := model.Structure(element.Types[0]); defined && named.Kind != "primitive-type" {
			r.checkObject(model, named, "", held, where, depth+1)

			return
		}
	}

	_ = name

	r.checkObject(model, structure, path, held, where, depth+1)
}

// checkTyped holds a primitive's value to the shape its type has.
func (r *Report) checkTyped(
	model conformance.Model, code string, value any, where string, depth int,
) {
	_, _ = model, depth

	switch held := value.(type) {
	case string:
		if shape, known := primitiveShapes[code]; known && !shape.MatchString(held) {
			r.note(SeverityError, where, "This is not a valid "+code+".")
		}

		if code == "boolean" {
			r.note(SeverityError, where, "A boolean is true or false, not a string.")
		}

		if numericTypes[code] {
			r.note(SeverityError, where, "A "+code+" is a number, not a string.")
		}
	case bool:
		if code != "boolean" && code != "" {
			r.note(SeverityError, where, "This element is a "+code+", not a boolean.")
		}
	case float64:
		r.checkNumber(code, held, where)
	}
}

// checkNumber holds a numeric primitive to its own range.
func (r *Report) checkNumber(code string, held float64, where string) {
	whole := held == float64(int64(held))

	switch code {
	case "integer":
		if !whole {
			r.note(SeverityError, where, "An integer is a whole number.")
		}
	case "positiveInt":
		if !whole || held < 1 {
			r.note(SeverityError, where, "A positiveInt is a whole number of at least 1.")
		}
	case "unsignedInt":
		if !whole || held < 0 {
			r.note(SeverityError, where, "An unsignedInt is a whole number of at least 0.")
		}
	case "decimal", "":
	default:
		r.note(SeverityError, where, "This element is a "+code+", not a number.")
	}
}

// declaredInPlace reports whether an element's children are written beneath it
// in its own resource's snapshot rather than in another definition.
func declaredInPlace(code string) bool {
	return code == "BackboneElement" || code == "Element"
}

// isResourceType reports whether an element holds a whole resource, whose own
// resourceType says what to check it against rather than the element's type.
func isResourceType(code string) bool {
	return code == "Resource" || code == "DomainResource"
}

// resourceType names the structure an issue is about, for the message.
func resourceType(structure conformance.Structure) string { return structure.Type }

// stated lists element names for a message, bounded so a refusal does not print
// a definition.
func stated(names []string) string {
	const most = 12

	if len(names) == 0 {
		return "no elements here"
	}

	if len(names) > most {
		return strings.Join(names[:most], ", ") + " and " +
			strconv.Itoa(len(names)-most) + " more"
	}

	return strings.Join(names, ", ")
}
