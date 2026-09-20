package conformance

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Element is one element as its own definition declares it.
//
// It is the part of a StructureDefinition that says what a resource may hold:
// how many of this element there may be, and what each one is. The prose, the
// bindings and the invariants are not here, because nothing in this build reads
// them.
type Element struct {
	// Min and Max are the cardinality. Max is "1", "*" or "0", as R4 writes it.
	Min int
	Max string

	// Types are the type codes this element may hold. More than one means a
	// choice — an element written as "value[x]", present as "valueQuantity" or
	// "valueString" and never as both.
	Types []string

	// Choice reports whether the element is written with the [x] suffix, which
	// is what makes the member's name carry the type.
	Choice bool

	// Binding is the value set a coded element is bound to, and how strictly.
	// Only a required binding is a rule: an extensible or preferred one says
	// what a code should be, and R4 permits another.
	Binding Binding

	// Constraints are the invariants R4 attaches to this element: rules about
	// the resource that cardinality and datatypes cannot express, written as
	// FHIRPath and evaluated with this element as the context.
	Constraints []Constraint

	// Opaque reports an element this build cannot say what is inside.
	//
	// R4 lets an element hold its own kind — a Questionnaire.item holds items,
	// an OperationDefinition.parameter.part holds parts — which a snapshot
	// cannot write out because it would not end. Those are expanded here to a
	// bounded depth, and what is past the bound is marked so that nothing walks
	// into it: an element with no children in the model would otherwise have
	// every child it does have reported as one nobody declared.
	Opaque bool
}

// Constraint is one invariant R4 states about an element.
type Constraint struct {
	// Key is the name R4 gives it, such as "org-1". It is what a refusal cites,
	// because it is what the specification and every other implementation call
	// this rule.
	Key string

	// Severity is "error" or "warning". A warning is a best practice R4 states
	// and does not require, so it is reported and never refused.
	Severity string

	// Human is the rule in the specification's own words, which is what makes a
	// refusal actionable to somebody who does not read FHIRPath.
	Human string

	// Expression is the rule itself.
	Expression string

	// BestPractice reports a constraint R4 marks as guidance rather than a
	// rule, with an extension it puts on exactly those.
	//
	// "A resource should have narrative for robust management" is true and is
	// not something a server refuses a write over, nor something worth saying
	// about every resource that ever arrives. R4 draws that line itself, so
	// this build reads it rather than inventing one.
	BestPractice bool
}

// bestPracticeExtension is what R4 marks guidance with.
const bestPracticeExtension = "http://hl7.org/fhir/StructureDefinition/elementdefinition-bestpractice"

// Applies reports whether this build holds a resource to this constraint.
func (c Constraint) Applies() bool { return !c.BestPractice }

// Required reports whether failing this invariant makes a resource invalid,
// rather than merely unusual.
func (c Constraint) Required() bool { return c.Severity == "error" }

// Repeats reports whether R4 writes this element as a JSON array.
func (e Element) Repeats() bool { return e.Max != "1" && e.Max != "0" }

// Required reports whether a resource must carry it.
func (e Element) Required() bool { return e.Min > 0 }

// Binding is what a coded element is bound to.
//
// Strength is R4's own vocabulary — "required", "extensible", "preferred" or
// "example" — and only the first of those makes a code outside the set wrong.
// ValueSet is the canonical url, with any version suffix taken off, because that
// is how the set is named where it is defined.
type Binding struct {
	Strength string
	ValueSet string
}

// Required reports whether a code outside this binding's set is an error rather
// than a suggestion.
func (b Binding) Required() bool { return b.Strength == "required" && b.ValueSet != "" }

// Structure is one type's elements, by the path beneath its root.
//
// The paths are the snapshot's own, with the type name taken off:
// "status", "component", "component.code". A backbone element's children are in
// here beside it, because that is how a snapshot writes them; a datatype's are
// in that datatype's own Structure.
type Structure struct {
	// Type is the resource or datatype this describes.
	Type string

	// Kind is what the definition calls itself: "resource", "complex-type" or
	// "primitive-type".
	Kind string

	// Elements is every element beneath the root, by its relative path.
	Elements map[string]Element

	// Root is the definition's own element — the one whose path is the type
	// itself. It is kept apart from Elements rather than stored under the empty
	// path, because everything that walks Elements walks it by name and an
	// element with no name is not one of those.
	//
	// It carries the invariants stated about the resource as a whole, which is
	// most of them: whether an Organization has a name or an identifier is not
	// a fact about any one of its elements.
	Root Element
}

// Model is every structure this build can check a resource against.
type Model struct {
	structures map[string]Structure
}

// Structure returns one type's elements, and reports whether this build has
// them. A type nobody defined is checked against nothing rather than against a
// guess.
func (m Model) Structure(name string) (Structure, bool) {
	held, found := m.structures[name]

	return held, found
}

// Types lists what the model holds, for a test that has to cover it.
func (m Model) Types() []string {
	return slices.Sorted(maps.Keys(m.structures))
}

// Element returns one element of one type by its relative path.
func (s Structure) Element(path string) (Element, bool) {
	held, found := s.Elements[path]

	return held, found
}

// Choices returns the choice elements declared directly beneath one path, by the
// name they are written under without the [x]. A member is matched against these
// when it is not a declared element itself.
func (s Structure) Choices(under string) map[string]Element {
	found := map[string]Element{}

	for path, element := range s.Elements {
		if !element.Choice || !directlyUnder(path, under) {
			continue
		}

		found[strings.TrimSuffix(lastSegment(path), "[x]")] = element
	}

	return found
}

// Names lists the elements declared directly beneath one path, which is what an
// unknown member is reported against.
func (s Structure) Names(under string) []string {
	var found []string

	for path, element := range s.Elements {
		if !directlyUnder(path, under) {
			continue
		}

		name := lastSegment(path)
		if element.Choice {
			name = strings.TrimSuffix(name, "[x]")
		}

		found = append(found, name)
	}

	slices.Sort(found)

	return found
}

// directlyUnder reports whether one path is a child of another, and not a
// grandchild: "component.code" is under "component" and not under "".
func directlyUnder(path, under string) bool {
	if under == "" {
		return !strings.Contains(path, ".")
	}

	rest, beneath := strings.CutPrefix(path, under+".")

	return beneath && !strings.Contains(rest, ".")
}

func lastSegment(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		return path[index+1:]
	}

	return path
}

// Definitions is the model built from what this build embeds, once.
//
// Building it parses the specification, which takes a fraction of a second and a
// few hundred megabytes that are released again. It is built on first use rather
// than at startup, so a process that never validates anything never pays for it.
var Definitions = sync.OnceValues(func() (Model, error) {
	structures := map[string]Structure{}

	for _, name := range embedded {
		if err := structuresIn(name, structures); err != nil {
			return Model{}, err
		}
	}

	// Resolved after everything is read, because one definition may point at
	// another that has not been read yet.
	resolveContentReferences(structures)

	return Model{structures: structures}, nil
})

// structuresIn reads one bundle's definitions straight into the model, without
// holding the whole bundle: thirty-five megabytes kept twice is a hundred
// megabytes of a server's memory to build a model that is a fraction of it.
func structuresIn(name string, into map[string]Structure) error {
	opened, closer, err := openBundle(name)
	if err != nil {
		return err
	}

	defer closer()

	decoder := json.NewDecoder(opened)

	if err := seekEntries(decoder, name); err != nil {
		return err
	}

	for decoder.More() {
		var entry struct {
			Resource json.RawMessage `json:"resource"`
		}

		if err := decoder.Decode(&entry); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, name, err)
		}

		structure, held := structureOf(entry.Resource)
		if held {
			into[structure.Type] = structure
		}
	}

	return nil
}

// snapshotElement is as much of one element as the model keeps.
type snapshotElement struct {
	Path             string `json:"path"`
	Min              int    `json:"min"`
	Max              string `json:"max"`
	ContentReference string `json:"contentReference"`
	Type             []struct {
		Code string `json:"code"`
	} `json:"type"`
	Binding struct {
		Strength string `json:"strength"`
		ValueSet string `json:"valueSet"`
	} `json:"binding"`
	Constraint []struct {
		Key        string `json:"key"`
		Severity   string `json:"severity"`
		Human      string `json:"human"`
		Expression string `json:"expression"`
		Extension  []struct {
			URL string `json:"url"`
		} `json:"extension"`
	} `json:"constraint"`
}

// structureOf reads one definition into the elements it declares.
func structureOf(content json.RawMessage) (Structure, bool) {
	var held struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		Abstract     bool   `json:"abstract"`
		Derivation   string `json:"derivation"`
		Snapshot     struct {
			Element []snapshotElement `json:"element"`
		} `json:"snapshot"`
	}

	if err := json.Unmarshal(content, &held); err != nil {
		return Structure{}, false
	}

	// Specializations only. A profile constrains something this build does not
	// check against profiles, and reading one as if it were the base definition
	// would check every resource against somebody's narrowing of it.
	if held.ResourceType != "StructureDefinition" || held.Derivation != "specialization" {
		return Structure{}, false
	}

	if len(held.Snapshot.Element) == 0 {
		return Structure{}, false
	}

	structure := Structure{
		Type:     held.ID,
		Kind:     held.Kind,
		Elements: map[string]Element{},
		Root:     elementOf(held.Snapshot.Element[0]),
	}
	root := held.Snapshot.Element[0].Path

	for _, element := range held.Snapshot.Element[1:] {
		path, beneath := strings.CutPrefix(element.Path, root+".")
		if !beneath {
			continue
		}

		structure.Elements[path] = elementOf(element)
	}

	return structure, true
}

// elementOf reads one snapshot element.
func elementOf(held snapshotElement) Element {
	element := Element{
		Min:    held.Min,
		Max:    held.Max,
		Choice: strings.HasSuffix(held.Path, "[x]"),
		Binding: Binding{
			Strength: held.Binding.Strength,
			// The version suffix is taken off: a binding names "…|4.0.1" and the
			// set is defined under the url without it.
			ValueSet: strings.SplitN(held.Binding.ValueSet, "|", 2)[0],
		},
		Constraints: constraintsOf(held),
	}

	for _, named := range held.Type {
		if named.Code != "" {
			element.Types = append(element.Types, named.Code)
		}
	}

	// An element that points at another's children carries that path in place
	// of a type, and is resolved once every definition has been read.
	if held.ContentReference != "" {
		element.Types = []string{contentReferenceType + strings.TrimPrefix(held.ContentReference, "#")}
	}

	return element
}

// contentReferenceType marks a type that is really a pointer at another
// element's children, until it is resolved.
const contentReferenceType = "#"

// nestingDepth is how deep a path may be before an element that holds its own
// kind stops being expanded, counted in segments.
//
// A Questionnaire item inside an item inside an item is a real questionnaire,
// and the expansion has to stop somewhere because the definition does not. It is
// a bound on the path rather than on how many times the expansion runs: each
// round copies children under every reference it resolved, including the ones it
// just created, so a bound on rounds bounds nothing — four of them leave a model
// with four million elements in it.
//
// What is past the bound is marked opaque rather than left with no children, so
// a resource that nests further is unchecked there instead of being told every
// element it wrote is one nobody declared.
const nestingDepth = 6

// resolveContentReferences copies the children of the element a contentReference
// names to where it points, round by round until nothing is left to resolve or
// the bound is reached.
//
// Each round reads the elements as they were and writes the additions
// separately, because a copy made in one round is itself something to resolve in
// the next: expanding in place would make the result depend on the order a map
// was walked, which is an answer that changes between runs.
func resolveContentReferences(structures map[string]Structure) {
	for name, structure := range structures {
		// Each round reaches one segment deeper, so the bound on the path is
		// also the bound on the rounds.
		for range nestingDepth {
			if !expandOnce(structure) {
				break
			}
		}

		// Whatever is still a reference is past the bound.
		for path, element := range structure.Elements {
			if referenced(element) != "" {
				element.Types = nil
				element.Opaque = true
				structure.Elements[path] = element
			}
		}

		structures[name] = structure
	}
}

// expandOnce resolves every reference the structure holds right now, and reports
// whether it resolved any.
func expandOnce(structure Structure) bool {
	added := map[string]Element{}
	resolved := map[string]Element{}

	for path, element := range structure.Elements {
		named := referenced(element)
		if named == "" {
			continue
		}

		// "Questionnaire.item" in Questionnaire's own elements is "item".
		relative, beneath := strings.CutPrefix(named, structure.Type+".")
		if !beneath {
			continue
		}

		// Past the bound nothing is copied, and the element is left as a
		// reference for the pass that marks what is left opaque.
		if strings.Count(path, ".")+1 >= nestingDepth {
			continue
		}

		for child, held := range childrenOf(structure, relative) {
			landing := path + "." + child
			if _, taken := structure.Elements[landing]; !taken {
				added[landing] = held
			}
		}

		element.Types = nil
		resolved[path] = element
	}

	if len(resolved) == 0 {
		return false
	}

	maps.Copy(structure.Elements, added)
	maps.Copy(structure.Elements, resolved)

	return true
}

// childrenOf returns what is declared beneath one path, by the rest of the path.
func childrenOf(structure Structure, under string) map[string]Element {
	held := map[string]Element{}

	for path, element := range structure.Elements {
		if rest, beneath := strings.CutPrefix(path, under+"."); beneath {
			held[rest] = element
		}
	}

	return held
}

// referenced returns the path an element points at, and empty for one that
// points at nothing.
func referenced(element Element) string {
	if len(element.Types) != 1 || !strings.HasPrefix(element.Types[0], contentReferenceType) {
		return ""
	}

	return strings.TrimPrefix(element.Types[0], contentReferenceType)
}

// constraintsOf reads the invariants one element declares.
//
// An invariant with no expression is one R4 states in prose alone. It is left
// out rather than kept as an empty rule: a constraint nothing can evaluate
// would otherwise be counted as one that passed.
func constraintsOf(held snapshotElement) []Constraint {
	var kept []Constraint

	for _, one := range held.Constraint {
		if one.Key == "" || one.Expression == "" {
			continue
		}

		held := Constraint{
			Key:        one.Key,
			Severity:   one.Severity,
			Human:      one.Human,
			Expression: one.Expression,
		}

		for _, extension := range one.Extension {
			if extension.URL == bestPracticeExtension {
				held.BestPractice = true
			}
		}

		kept = append(kept, held)
	}

	return kept
}

// ConstraintsIn reads the invariants one StructureDefinition states about the
// resource as a whole, and the type it constrains.
//
// Only the ones attached to its own root. An invariant sits on an element and
// is evaluated with that element as the context — "must have either extensions
// or value, not both" is about an extension, and asking it of the resource
// answers about a resource's own extensions, which is a different question with
// a different answer.
//
// Applying a profile's deeper invariants means resolving each one's path
// through the resource, which this build does not do; docs/known-limitations.md
// says so rather than this pretending otherwise.
//
// A profile is written as a differential and a base definition as a snapshot.
// Both are read, because a profile may be published either way.
func ConstraintsIn(content []byte) ([]Constraint, string, error) {
	var held struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Snapshot     struct {
			Element []snapshotElement `json:"element"`
		} `json:"snapshot"`
		Differential struct {
			Element []snapshotElement `json:"element"`
		} `json:"differential"`
	}

	if err := json.Unmarshal(content, &held); err != nil {
		return nil, "", fmt.Errorf("conformance: read a StructureDefinition: %w", err)
	}

	if held.ResourceType != "StructureDefinition" {
		return nil, "", fmt.Errorf("conformance: that is a %s, not a StructureDefinition",
			held.ResourceType)
	}

	var found []Constraint

	for _, element := range append(
		append([]snapshotElement{}, held.Differential.Element...),
		held.Snapshot.Element...,
	) {
		// The root is the element whose path is the definition's own name, with
		// nothing beneath it.
		if strings.Contains(element.Path, ".") {
			continue
		}

		for _, one := range constraintsOf(element) {
			found = appendConstraintOnce(found, one)
		}
	}

	return found, held.Type, nil
}

// appendConstraintOnce keeps a rule once when a definition states it in both a
// differential and a snapshot.
func appendConstraintOnce(held []Constraint, one Constraint) []Constraint {
	for _, kept := range held {
		if kept.Key == one.Key {
			return held
		}
	}

	return append(held, one)
}
