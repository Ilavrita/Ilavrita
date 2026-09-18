package search

import (
	"fmt"
	"slices"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// declaration is one row of the table below, before it is validated. Writing
// the registry as data keeps what this build implements in one readable place;
// a parameter is only real once Supported has built it.
type declaration struct {
	name string
	kind Kind
	path string

	// code and system are the members within a token's element. Both are empty
	// for an element that is itself the code, such as a plain status.
	code   string
	system string
}

// universal are the parameters every resource type answers, read from the row
// rather than from anything projected out of its content.
var universal = []declaration{
	{name: "_id", kind: KindToken, path: "res_id"},
	{name: "_lastUpdated", kind: KindDate, path: "last_updated"},
}

// implemented names the parameters each resource type adds to the universal
// ones. A type absent from here answers the universal parameters and nothing
// else, and a parameter absent from a type's row is refused rather than
// ignored (SRC-4).
//
// It is deliberately a short list. Every entry here is a column a write has to
// maintain and a predicate a read has to compile, so a parameter is added when
// something needs it rather than because R4 defines it.
var implemented = map[storage.ResourceType][]declaration{
	"Patient": {
		{name: "identifier", kind: KindToken, path: "identifier", code: "value", system: "system"},
		{name: "family", kind: KindString, path: "name.family"},
		{name: "given", kind: KindString, path: "name.given"},
		{name: "gender", kind: KindToken, path: "gender"},
		{name: "active", kind: KindToken, path: "active"},
		{name: "birthdate", kind: KindDate, path: "birthDate"},
	},
	"Observation": {
		{name: "status", kind: KindToken, path: "status"},
		{name: "category", kind: KindToken, path: "category.coding", code: "code", system: "system"},
		{name: "code", kind: KindToken, path: "code.coding", code: "code", system: "system"},
		{name: "subject", kind: KindReference, path: "subject.reference"},
		{name: "patient", kind: KindReference, path: "subject.reference"},
		{name: "encounter", kind: KindReference, path: "encounter.reference"},
		{name: "performer", kind: KindReference, path: "performer.reference"},
		{name: "date", kind: KindDate, path: "effectiveDateTime"},
	},
	"Encounter": {
		{name: "status", kind: KindToken, path: "status"},
		{name: "class", kind: KindToken, path: "class", code: "code", system: "system"},
		{name: "subject", kind: KindReference, path: "subject.reference"},
		{name: "patient", kind: KindReference, path: "subject.reference"},
		{name: "date", kind: KindDate, path: "period.start"},
	},
	"Condition": {
		{name: "subject", kind: KindReference, path: "subject.reference"},
		{name: "patient", kind: KindReference, path: "subject.reference"},
		{name: "encounter", kind: KindReference, path: "encounter.reference"},
		{name: "code", kind: KindToken, path: "code.coding", code: "code", system: "system"},
		{name: "category", kind: KindToken, path: "category.coding", code: "code", system: "system"},
	},
	"Organization": {
		{name: "identifier", kind: KindToken, path: "identifier", code: "value", system: "system"},
		{name: "name", kind: KindString, path: "name"},
		{name: "active", kind: KindToken, path: "active"},
	},
	"Practitioner": {
		{name: "identifier", kind: KindToken, path: "identifier", code: "value", system: "system"},
		{name: "family", kind: KindString, path: "name.family"},
		{name: "given", kind: KindString, path: "name.given"},
		{name: "active", kind: KindToken, path: "active"},
	},
	"MedicationRequest": {
		{name: "status", kind: KindToken, path: "status"},
		{name: "intent", kind: KindToken, path: "intent"},
		{name: "subject", kind: KindReference, path: "subject.reference"},
		{name: "patient", kind: KindReference, path: "subject.reference"},
		{name: "encounter", kind: KindReference, path: "encounter.reference"},
	},
	"DiagnosticReport": {
		{name: "status", kind: KindToken, path: "status"},
		{name: "code", kind: KindToken, path: "code.coding", code: "code", system: "system"},
		{name: "subject", kind: KindReference, path: "subject.reference"},
		{name: "patient", kind: KindReference, path: "subject.reference"},
		{name: "encounter", kind: KindReference, path: "encounter.reference"},
	},
}

// Supported returns the parameters one resource type answers, universal ones
// first, in a stable order so a CapabilityStatement built from it does not
// change between processes.
//
// It panics on a malformed declaration, which is a programming error in the
// table above rather than anything a request can reach: a registry that built
// half its parameters would refuse queries this build means to serve.
func Supported(resourceType storage.ResourceType) []Parameter {
	declared := append(slices.Clone(universal), implemented[resourceType]...)
	built := make([]Parameter, 0, len(declared))

	for _, entry := range declared {
		parameter, err := build(entry)
		if err != nil {
			panic(fmt.Sprintf("search: the registry declares %s/%s badly: %v",
				resourceType, entry.name, err))
		}

		built = append(built, parameter)
	}

	return built
}

// build turns one declaration into the parameter it describes.
func build(entry declaration) (Parameter, error) {
	if slices.ContainsFunc(universal, func(known declaration) bool { return known.name == entry.name }) {
		return Stored(entry.name, entry.kind, entry.path)
	}

	switch entry.kind {
	case KindToken:
		return Token(entry.name, entry.path, entry.code, entry.system)
	case KindString:
		return Text(entry.name, entry.path)
	case KindReference:
		return Reference(entry.name, entry.path)
	case KindDate:
		return Date(entry.name, entry.path)
	default:
		return Parameter{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(entry.kind))
	}
}

// Find returns the parameter one query names, if this build implements it for
// this type.
func Find(resourceType storage.ResourceType, name string) (Parameter, bool) {
	for _, parameter := range Supported(resourceType) {
		if parameter.name == name {
			return parameter, true
		}
	}

	return Parameter{}, false
}

// Indexed returns the parameters a write has to project for one resource type,
// which is every supported parameter the store does not already hold a column
// for.
func Indexed(resourceType storage.ResourceType) []Parameter {
	supported := Supported(resourceType)
	projected := make([]Parameter, 0, len(supported))

	for _, parameter := range supported {
		if parameter.Projects() {
			projected = append(projected, parameter)
		}
	}

	return projected
}
