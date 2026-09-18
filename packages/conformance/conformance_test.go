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
