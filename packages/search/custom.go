package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// Reading a SearchParameter resource into a parameter this build can apply.
//
// R4 states where a parameter reads from as a FHIRPath expression, and this
// build has no FHIRPath engine. What it has is the element model, which is
// enough for the expressions that name an element and nothing else —
// "Patient.telecom.value". One that filters, unions, resolves or calls a
// function is refused rather than approximated.
//
// That refusal is the point. A parameter that quietly matched something other
// than what it says is worse than one that does not exist: a search naming it
// comes back looking answered, and nobody reads an empty page as a bug.
var (
	// ErrNotASearchParameter reports a resource that is not one.
	ErrNotASearchParameter = errors.New("search: that is not a SearchParameter")

	// ErrParameterIncomplete reports a SearchParameter missing an element R4
	// requires of one.
	ErrParameterIncomplete = errors.New("search: a SearchParameter states its code, base and type")

	// ErrParameterKindUnsupported reports a type this build does not index.
	ErrParameterKindUnsupported = errors.New(
		"search: this build indexes token, string, reference and date")

	// ErrParameterExpression reports an expression that is not a plain path.
	ErrParameterExpression = errors.New(
		"search: an expression names one element, with no function or filter")

	// ErrParameterBase reports a base this server does not serve.
	ErrParameterBase = errors.New("search: that base is not a type this server serves")

	// ErrParameterReserved reports a code a built-in parameter already answers.
	// Shadowing one would change what an existing query means.
	ErrParameterReserved = errors.New("search: that code is already answered for this type")

	// ErrParameterElement reports an expression naming an element the type does
	// not declare, or one whose datatype the stated type cannot read.
	ErrParameterElement = errors.New("search: that element cannot be read as that type")
)

// searchParameterStatus is the only status this build applies. A draft or
// retired parameter is stored like any resource and indexes nothing.
const searchParameterStatus = "active"

// kindsByType maps R4's SearchParameter.type onto what this build indexes.
var kindsByType = map[string]Kind{
	"token":     KindToken,
	"string":    KindString,
	"reference": KindReference,
	"date":      KindDate,
}

// tokenMembers names, for each datatype a token may be read from, the members
// holding the code and the system.
//
// A token has two halves and they have to come from the same element, so this
// is derived from the datatype rather than stated by whoever wrote the
// SearchParameter: a definition that paired one coding's system with another's
// code would be a definition nobody could tell was wrong.
var tokenMembers = map[string]struct{ code, system string }{
	"Coding":       {"code", "system"},
	"Identifier":   {"value", "system"},
	"ContactPoint": {"value", "system"},
}

// codeableConcept is read one level in, at the coding it holds, which is where
// the two halves of the token actually are.
const codeableConcept = "CodeableConcept"

// plainTokens are the datatypes that are themselves the code, with no system
// beside them.
var plainTokens = []string{"code", "string", "boolean", "uri", "id", "canonical"}

// datatypesByKind is what each kind other than a token may read from.
var datatypesByKind = map[Kind][]string{
	KindString:    {"string", "markdown", "HumanName", "Address"},
	KindReference: {"Reference"},
	KindDate:      {"date", "dateTime", "instant", "Period"},
}

// submittedSearchParameter is the part of one this build reads.
type submittedSearchParameter struct {
	ResourceType string   `json:"resourceType"`
	Code         string   `json:"code"`
	Status       string   `json:"status"`
	Type         string   `json:"type"`
	Base         []string `json:"base"`
	Expression   string   `json:"expression"`
}

// Defined is one parameter a Project stated, for one of the types it named.
//
// A SearchParameter may name several bases, and each is compiled on its own:
// the same expression reads a different element on a different type, and one
// base being unreadable says nothing about the others.
type Defined struct {
	Type      storage.ResourceType
	Parameter Parameter
}

// ReadSearchParameter compiles one stored SearchParameter into the parameters
// it defines, one per base it names.
//
// A parameter that is not active compiles to nothing and is not an error: a
// draft is a thing somebody is still writing, and refusing to store it would
// make a Project unable to draft one.
func ReadSearchParameter(content []byte) ([]Defined, error) {
	var held submittedSearchParameter
	if err := json.Unmarshal(content, &held); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotASearchParameter, err)
	}

	if held.ResourceType != "SearchParameter" {
		return nil, fmt.Errorf("%w: it is a %s", ErrNotASearchParameter, held.ResourceType)
	}

	if held.Code == "" || held.Type == "" || held.Status == "" || len(held.Base) == 0 {
		return nil, ErrParameterIncomplete
	}

	if held.Status != searchParameterStatus {
		return nil, nil
	}

	kind, indexed := kindsByType[held.Type]
	if !indexed {
		return nil, fmt.Errorf("%w: %q", ErrParameterKindUnsupported, held.Type)
	}

	model, err := conformance.Definitions()
	if err != nil {
		return nil, fmt.Errorf("search: read the definitions: %w", err)
	}

	defined := make([]Defined, 0, len(held.Base))

	for _, base := range held.Base {
		compiled, err := compileFor(model, held, base, kind)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", base, err)
		}

		defined = append(defined, compiled)
	}

	return defined, nil
}

// compileFor builds the parameter one base gets.
func compileFor(
	model conformance.Model, held submittedSearchParameter, base string, kind Kind,
) (Defined, error) {
	resourceType := storage.ResourceType(base)

	if !fhir.ServesResourceType(base) {
		return Defined{}, fmt.Errorf("%w: %s", ErrParameterBase, base)
	}

	if _, reserved := Find(resourceType, held.Code); reserved {
		return Defined{}, fmt.Errorf("%w: %s", ErrParameterReserved, held.Code)
	}

	element, err := elementPath(held.Expression, base)
	if err != nil {
		return Defined{}, err
	}

	structure, defined := model.Structure(base)
	if !defined {
		return Defined{}, fmt.Errorf("%w: %s is not defined", ErrParameterBase, base)
	}

	datatype, err := datatypeAt(structure, base, element)
	if err != nil {
		return Defined{}, err
	}

	parameter, err := parameterFor(kind, held.Code, element, datatype)
	if err != nil {
		return Defined{}, err
	}

	return Defined{Type: resourceType, Parameter: parameter}, nil
}

// elementPath reads the element an expression names, with the leading type
// stripped, and refuses anything that is not a plain path.
func elementPath(expression, base string) (string, error) {
	held := strings.TrimSpace(expression)
	if held == "" {
		return "", fmt.Errorf("%w: it states none", ErrParameterExpression)
	}

	// Everything FHIRPath can do beyond naming an element. A parameter reading
	// "Patient.name.where(use='official')" is not one this build can project.
	if strings.ContainsAny(held, "()|[]'\" ") {
		return "", fmt.Errorf("%w: %q", ErrParameterExpression, expression)
	}

	prefix := base + "."
	if !strings.HasPrefix(held, prefix) {
		return "", fmt.Errorf("%w: %q does not read from %s", ErrParameterExpression, expression, base)
	}

	return strings.TrimPrefix(held, prefix), nil
}

// datatypeAt returns the datatype of the element a path names.
func datatypeAt(structure conformance.Structure, base, element string) (string, error) {
	held, found := structure.Element(element)
	if !found {
		// A choice is declared under the name R4 writes it with, which is not
		// the name a value is written under.
		held, found = structure.Element(element + "[x]")
	}

	if !found {
		return "", fmt.Errorf("%w: %s declares no %s", ErrParameterElement, base, element)
	}

	if len(held.Types) != 1 {
		// A choice element is written under a different member name for each
		// type it may hold, so one path does not name one datatype.
		return "", fmt.Errorf("%w: %s.%s holds %d types",
			ErrParameterElement, base, element, len(held.Types))
	}

	return held.Types[0], nil
}

// parameterFor builds the parameter a kind and a datatype agree on, and refuses
// the pairs that do not.
func parameterFor(kind Kind, code, element, datatype string) (Parameter, error) {
	if kind == KindToken {
		if members, coded := tokenMembers[datatype]; coded {
			return Token(code, element, members.code, members.system)
		}

		if datatype == codeableConcept {
			members := tokenMembers["Coding"]

			return Token(code, element+".coding", members.code, members.system)
		}

		if slices.Contains(plainTokens, datatype) {
			return Token(code, element, "", "")
		}

		return Parameter{}, fmt.Errorf("%w: a token cannot read a %s", ErrParameterElement, datatype)
	}

	if !slices.Contains(datatypesByKind[kind], datatype) {
		return Parameter{}, fmt.Errorf("%w: a %s cannot read a %s", ErrParameterElement, kind, datatype)
	}

	switch kind {
	case KindString:
		return Text(code, element)
	case KindReference:
		return Reference(code, element)
	case KindDate:
		return Date(code, element)
	default:
		return Parameter{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(kind))
	}
}
