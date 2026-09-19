package main

import (
	"encoding/json"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

// mimeTypeValueSet is R4's binding for a media type. It is matched by prefix
// because the binding names a version and the canonical does not.
const mimeTypeValueSet = "http://hl7.org/fhir/ValueSet/mimetypes"

// fixtureDepth bounds how far a fixture fills required elements into required
// elements. R4 does not nest requirements deeply, and a bound is what stops a
// definition that pointed back at itself from building for ever.
const fixtureDepth = 6

// submissionRequirements returns the elements a type must carry, filled with
// values of the shape their own definition declares.
//
// It is built from the same model the validator reads, which is the point: a
// fixture written by hand drifts from what the specification requires, and a
// suite driving every served type through every interaction would then be
// driving them through bodies no client could send.
//
// A build that cannot read its own definitions stops the suite rather than
// quietly producing the fixtures it used to: what would follow is every type
// passing against a body the server would refuse.
func submissionRequirements(resourceType string) map[string]json.RawMessage {
	model, err := conformance.Definitions()
	if err != nil {
		panic("the fixtures need the definitions: " + err.Error())
	}

	structure, defined := model.Structure(resourceType)
	if !defined {
		return map[string]json.RawMessage{}
	}

	return filledUnder(model, structure, "", 0)
}

// filledUnder fills the required elements declared directly beneath one path.
func filledUnder(
	model conformance.Model, structure conformance.Structure, under string, depth int,
) map[string]json.RawMessage {
	filled := map[string]json.RawMessage{}

	if depth > fixtureDepth {
		return filled
	}

	for _, name := range structure.Names(under) {
		path := name
		if under != "" {
			path = under + "." + name
		}

		element, found := structure.Element(path)
		if !found {
			element, found = structure.Element(path + "[x]")
		}

		if !found || !element.Required() || len(element.Types) == 0 {
			continue
		}

		// A choice carries its type in the member's name, which is also how a
		// client would have to write it.
		code := element.Types[0]
		written := name

		if element.Choice {
			written = name + upperFirst(code)
		}

		value := valueOf(model, structure, path, element, code, depth)
		if value == nil {
			continue
		}

		if element.Repeats() {
			value = json.RawMessage("[" + string(value) + "]")
		}

		filled[written] = value
	}

	return filled
}

// valueOf builds one value of the shape a type declares.
func valueOf(
	model conformance.Model,
	structure conformance.Structure,
	path string,
	element conformance.Element,
	code string,
	depth int,
) json.RawMessage {
	// A coded element bound to a value set takes a code from it. Anything else
	// is a body the server refuses, which would make this suite prove the routes
	// work on something no client could send.
	if bound, ok := boundCode(element); ok {
		switch code {
		case "code":
			return json.RawMessage(`"` + bound.Code + `"`)
		case "Coding":
			return json.RawMessage(coding(bound))
		case "CodeableConcept":
			return json.RawMessage(`{"coding":[` + coding(bound) + `]}`)
		}
	}

	// A mime type cannot come from a value set. R4 binds these to BCP-13, which
	// is IANA's registry rather than a list of codes, so nothing resolves it and
	// the fixture would fall through to a plain token — leaving every fixture
	// carrying a content type of "fixture", which is not a media type and not a
	// body any client could send.
	if strings.HasPrefix(element.Binding.ValueSet, mimeTypeValueSet) {
		return json.RawMessage(`"text/plain"`)
	}

	if held, known := primitiveFixtures[code]; known {
		return json.RawMessage(held)
	}

	// A backbone element's own children are declared here, beneath it.
	if code == "BackboneElement" || code == "Element" {
		return encoded(filledUnder(model, structure, path, depth+1))
	}

	// A reference is what places a clinical resource, so it names the patient
	// this suite is confined to rather than an invented id.
	if code == "Reference" {
		return json.RawMessage(`{"reference":"Patient/` + string(conformancePatient) + `"}`)
	}

	named, defined := model.Structure(code)
	if !defined {
		return json.RawMessage(`{}`)
	}

	_ = element

	return encoded(filledAtLeastOnce(model, named, depth))
}

// filledAtLeastOnce fills a complex type that requires nothing of its own.
//
// R4 has no empty object: an element written as {} is present and says nothing,
// and the server refuses one. A datatype like CodeableConcept requires none of
// its children, so filling only what is required would produce exactly that —
// and the fixture would be proving the routes work on a body no client could
// send.
func filledAtLeastOnce(
	model conformance.Model, named conformance.Structure, depth int,
) map[string]json.RawMessage {
	filled := filledUnder(model, named, "", depth+1)
	if len(filled) > 0 {
		return filled
	}

	// Nothing is required, so one optional element carries the content. A
	// primitive, because a complex one would have the same problem one level
	// down, and valueOf so a bound code comes from its own value set.
	for _, name := range named.Names("") {
		element, found := named.Element(name)
		if !found || element.Choice || len(element.Types) == 0 {
			continue
		}

		code := element.Types[0]
		if _, known := primitiveFixtures[code]; !known {
			continue
		}

		value := valueOf(model, named, name, element, code, depth+1)
		if value == nil {
			continue
		}

		if element.Repeats() {
			value = json.RawMessage("[" + string(value) + "]")
		}

		return map[string]json.RawMessage{name: value}
	}

	return filled
}

// boundCode returns a code the element's own binding admits, and reports whether
// there is one. A binding this build did not resolve decides nothing, so the
// fixture falls back to a plain token.
func boundCode(element conformance.Element) (conformance.Coded, bool) {
	if !element.Binding.Required() {
		return conformance.Coded{}, false
	}

	model, err := conformance.Terminologies()
	if err != nil {
		panic("the fixtures need the terminology: " + err.Error())
	}

	admitted, resolved := model.Admits(element.Binding.ValueSet)
	if !resolved {
		return conformance.Coded{}, false
	}

	held := admitted.Codes()
	if len(held) == 0 {
		return conformance.Coded{}, false
	}

	// The first in order, so a fixture is the same on every run.
	return held[0], true
}

// coding writes one code as R4 writes a Coding.
func coding(held conformance.Coded) string {
	if held.System == "" {
		return `{"code":"` + held.Code + `"}`
	}

	return `{"system":"` + held.System + `","code":"` + held.Code + `"}`
}

// primitiveFixtures is one acceptable value for each primitive an element may
// require. They are the shapes the validator checks, which is what makes a
// fixture a resource rather than something that merely parses.
var primitiveFixtures = map[string]string{
	"code":         `"fixture"`,
	"string":       `"a fixture"`,
	"markdown":     `"a fixture"`,
	"boolean":      `true`,
	"integer":      `1`,
	"positiveInt":  `1`,
	"unsignedInt":  `0`,
	"decimal":      `1.5`,
	"uri":          `"http://example.test/fixture"`,
	"url":          `"http://example.test/fixture"`,
	"canonical":    `"http://example.test/fixture"`,
	"oid":          `"urn:oid:1.2.3.4"`,
	"uuid":         `"urn:uuid:c757873d-ec9a-4326-a141-556f43239520"`,
	"id":           `"fixture"`,
	"date":         `"2026-03-01"`,
	"dateTime":     `"2026-03-01"`,
	"instant":      `"2026-03-01T09:00:00Z"`,
	"time":         `"09:00:00"`,
	"base64Binary": `"aGVsbG8="`,
	"xhtml":        `"<div xmlns=\"http://www.w3.org/1999/xhtml\">a fixture</div>"`,
}

func encoded(held map[string]json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(held)
	if err != nil {
		panic("a fixture that cannot be encoded: " + err.Error())
	}

	return raw
}

// upperFirst is a type code as a choice member spells it. Arithmetic on the
// first byte would do it for a lowercase one and quietly make nonsense of
// "Reference", which is a type code too.
func upperFirst(held string) string {
	if held == "" {
		return ""
	}

	return strings.ToUpper(held[:1]) + held[1:]
}

// repeatsIn reports whether one element of one type is written as an array, so a
// fixture that sets an element itself writes it the way the definition says.
func repeatsIn(resourceType, name string) bool {
	model, err := conformance.Definitions()
	if err != nil {
		panic("the fixtures need the definitions: " + err.Error())
	}

	structure, defined := model.Structure(resourceType)
	if !defined {
		return false
	}

	element, found := structure.Element(name)

	return found && element.Repeats()
}

// asWritten wraps a value the way the element it is for is written.
func asWritten(resourceType, name string, value string) json.RawMessage {
	if repeatsIn(resourceType, name) {
		return json.RawMessage("[" + value + "]")
	}

	return json.RawMessage(value)
}

// valid builds a body of one type carrying whatever the caller states, with the
// elements its definition requires filled in around them.
//
// Every fixture goes through here rather than being written out, because a
// resource this server refuses is one no test should be proving anything with:
// a suite full of bodies that are only nearly FHIR proves the routes work on
// something nobody sends.
func valid(resourceType string, stating map[string]string) string {
	fields := map[string]json.RawMessage{}

	if err := json.Unmarshal([]byte(submission(resourceType)), &fields); err != nil {
		panic("a fixture that does not parse: " + err.Error())
	}

	for name, raw := range stating {
		if raw == "" {
			delete(fields, name)

			continue
		}

		fields[name] = json.RawMessage(raw)
	}

	body, err := json.Marshal(fields)
	if err != nil {
		panic("a fixture that cannot be encoded: " + err.Error())
	}

	return string(body)
}
