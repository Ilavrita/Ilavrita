package conformance_test

import (
	"encoding/json"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// TestEveryServedTypeHasItsDefinition. The point of embedding the specification
// is that this server can say what a resource is; a type it serves and has no
// definition for is one it cannot.
func TestEveryServedTypeHasItsDefinition(t *testing.T) {
	held, err := conformance.StructureDefinitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	byID := map[string]conformance.Definition{}
	for _, definition := range held {
		byID[definition.ID] = definition
	}

	for _, name := range fhir.ServedResourceTypes() {
		if _, found := byID[name]; !found {
			t.Errorf("%s is served and has no definition", name)
		}
	}

	// And the datatypes, because an element's type is one of them.
	for _, name := range []string{"HumanName", "CodeableConcept", "Reference", "Period", "Quantity"} {
		if _, found := byID[name]; !found {
			t.Errorf("the %s datatype has no definition", name)
		}
	}
}

// TestADefinitionIsWhatTheSpecificationPublished, whole. A client reading
// StructureDefinition/Observation is reading what HL7 wrote; a reduced copy
// would be something else wearing its name.
func TestADefinitionIsWhatTheSpecificationPublished(t *testing.T) {
	held, err := conformance.StructureDefinitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	for _, definition := range held {
		if definition.ID != "Observation" {
			continue
		}

		var read struct {
			ResourceType string `json:"resourceType"`
			URL          string `json:"url"`
			Version      string `json:"version"`
			Snapshot     struct {
				Element []struct {
					Path string `json:"path"`
					Min  int    `json:"min"`
					Max  string `json:"max"`
				} `json:"element"`
			} `json:"snapshot"`
		}

		if err := json.Unmarshal(definition.Content, &read); err != nil {
			t.Fatalf("decode the stored definition: %v", err)
		}

		if read.ResourceType != "StructureDefinition" || read.Version != conformance.Release {
			t.Errorf("it reads as %s %s", read.ResourceType, read.Version)
		}

		if read.URL != definition.URL || definition.URL == "" {
			t.Errorf("the url is %q and the row says %q", read.URL, definition.URL)
		}

		// The snapshot is the flattened element list, which is the half of a
		// definition anything checking a resource needs.
		if len(read.Snapshot.Element) < 30 {
			t.Fatalf("Observation's snapshot holds %d elements", len(read.Snapshot.Element))
		}

		found := false

		for _, element := range read.Snapshot.Element {
			if element.Path == "Observation.status" {
				found = true

				if element.Min != 1 || element.Max != "1" {
					t.Errorf("Observation.status is %d..%s", element.Min, element.Max)
				}
			}
		}

		if !found {
			t.Error("the snapshot names no Observation.status")
		}

		return
	}

	t.Fatal("no Observation definition was read")
}

// TestTheDigestIsOfWhatIsEmbedded, and is stable. An install compares it with
// what it last seeded, so one that wandered would reseed the whole
// specification on every start.
func TestTheDigestIsOfWhatIsEmbedded(t *testing.T) {
	first, second := conformance.Digest(), conformance.Digest()

	if first == "" || first != second {
		t.Errorf("the digest reads %q then %q", first, second)
	}

	if len(first) != 64 {
		t.Errorf("it is %d characters", len(first))
	}
}

// TestTheDefinitionsAreOrdered, so two builds of the same bundles seed the same
// thing in the same order and a diff of what changed is readable.
func TestTheDefinitionsAreOrdered(t *testing.T) {
	held, err := conformance.StructureDefinitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	if len(held) < 200 {
		t.Fatalf("read %d definitions, want the resources and the datatypes", len(held))
	}

	for index := 1; index < len(held); index++ {
		before, after := held[index-1], held[index]

		if before.Type > after.Type || (before.Type == after.Type && before.ID >= after.ID) {
			t.Fatalf("%s/%s is followed by %s/%s", before.Type, before.ID, after.Type, after.ID)
		}
	}
}

// TestAnElementThatHoldsItsOwnKindIsExpanded. R4 lets an element hold its own
// kind — a Questionnaire item holds items, an OperationDefinition parameter
// holds parts — which a snapshot cannot write out because it would not end. The
// model expands it, and what it expands has to be there or every element a
// nested resource writes reads as one nobody declared.
func TestAnElementThatHoldsItsOwnKindIsExpanded(t *testing.T) {
	model, err := conformance.Definitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	for _, held := range []struct {
		named string
		path  string
	}{
		{"Questionnaire", "item.item.text"},
		{"Questionnaire", "item.item.item.linkId"},
		{"OperationDefinition", "parameter.part.name"},
		{"OperationDefinition", "parameter.part.part.min"},
	} {
		structure, defined := model.Structure(held.named)
		if !defined {
			t.Fatalf("no structure for %s", held.named)
		}

		if _, found := structure.Element(held.path); !found {
			t.Errorf("%s.%s was not expanded", held.named, held.path)
		}
	}
}

// TestNoElementIsLeftWithNothingInsideIt. This is the invariant that stops the
// expansion producing refusals for resources that are right.
//
// An element that names no type is one whose children are declared beneath it —
// that is what resolving a reference leaves behind. If the expansion stopped
// before it got there, the element would name no type and have no children, and
// a walker would then report every element the resource wrote inside it as one
// nobody declared. What is past the bound is marked opaque so nothing walks into
// it at all.
func TestNoElementIsLeftWithNothingInsideIt(t *testing.T) {
	model, err := conformance.Definitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	stranded := 0

	for _, name := range model.Types() {
		structure, _ := model.Structure(name)

		for path, element := range structure.Elements {
			if len(element.Types) != 0 || element.Opaque {
				continue
			}

			if len(structure.Names(path)) != 0 {
				continue
			}

			stranded++

			if stranded <= 5 {
				t.Errorf("%s.%s names no type, holds nothing and is not opaque", name, path)
			}
		}
	}

	if stranded != 0 {
		t.Errorf("%d elements are stranded", stranded)
	}
}

// TestTheModelIsTheSameEveryTime. It is built by walking maps, and a model that
// depended on the order they were walked would be a validator that refused a
// resource on one run and accepted it on the next.
func TestTheModelIsTheSameEveryTime(t *testing.T) {
	model, err := conformance.Definitions()
	if err != nil {
		t.Fatalf("read the definitions: %v", err)
	}

	first := map[string]int{}
	for _, name := range model.Types() {
		structure, _ := model.Structure(name)
		first[name] = len(structure.Elements)
	}

	// The same process holds one model, so what this can compare is the model
	// against itself; TestTheSpecificationValidatesAgainstItself run repeatedly
	// is what covers two processes disagreeing.
	for _, name := range model.Types() {
		structure, _ := model.Structure(name)
		if len(structure.Elements) != first[name] {
			t.Errorf("%s holds %d elements and held %d", name, len(structure.Elements), first[name])
		}
	}

	if len(first) < 200 {
		t.Errorf("the model holds %d types", len(first))
	}
}
