package conformance

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Coded is one code in one system.
type Coded struct {
	System string
	Code   string
}

// Admitted is the set of codes one value set allows.
//
// It is a set rather than a list because the only question asked of it is
// whether a code is in it. A value set this build could not work out holds
// nothing and reports so: the difference between "not in the set" and "I could
// not tell" decides whether a resource is refused, and conflating them would
// refuse resources for a set nobody here understands.
type Admitted struct {
	codes map[Coded]bool

	// anySystem holds the systems a value set admits every code from, which is
	// how R4 writes "this element holds a MIME type" — an include naming a
	// system and no concepts, where the system's own definition is complete.
	// Those are expanded, so this stays empty for the sets this build resolves.
	anySystem []string
}

// Holds reports whether one code is in the set.
//
// The system is compared when the code carries one. A bare code — which is what
// R4's `code` datatype is, a token with no system of its own — matches on the
// code alone, because the element's binding is what says which system it is in.
func (a Admitted) Holds(held Coded) bool {
	if a.codes[held] {
		return true
	}

	if held.System != "" {
		return slices.Contains(a.anySystem, held.System)
	}

	for known := range a.codes {
		if known.Code == held.Code {
			return true
		}
	}

	return false
}

// Size returns how many codes the set holds, for a test that has to know the
// expansion did something.
func (a Admitted) Size() int { return len(a.codes) }

// Codes returns what the set holds, ordered, so something choosing a code from
// it chooses the same one every time.
func (a Admitted) Codes() []Coded {
	held := slices.Collect(maps.Keys(a.codes))

	slices.SortFunc(held, func(x, y Coded) int {
		if named := strings.Compare(x.System, y.System); named != 0 {
			return named
		}

		return strings.Compare(x.Code, y.Code)
	})

	return held
}

// Terminology is the value sets this build can decide a code against.
type Terminology struct {
	sets map[string]Admitted
}

// Admits returns one value set, and reports whether this build resolved it.
//
// A set it did not resolve is one no code is judged against. R4 binds elements
// to MIME types, to UCUM units and to sets published elsewhere, and none of
// those is content this build holds — so a resource using them is unchecked
// there rather than refused.
func (t Terminology) Admits(url string) (Admitted, bool) {
	held, found := t.sets[url]

	return held, found
}

// Sets lists what was resolved, for a test that has to cover it.
func (t Terminology) Sets() []string { return slices.Sorted(maps.Keys(t.sets)) }

// Terminologies is the terminology built from what this build embeds, once.
var Terminologies = sync.OnceValues(func() (Terminology, error) {
	systems, sets, err := readTerminology()
	if err != nil {
		return Terminology{}, err
	}

	resolved := map[string]Admitted{}

	for url, set := range sets {
		admitted, ok := expand(set, systems)
		if ok {
			resolved[url] = admitted
		}
	}

	return Terminology{sets: resolved}, nil
})

// composedValueSet is as much of a ValueSet as an expansion needs.
type composedValueSet struct {
	URL     string `json:"url"`
	Compose struct {
		Include []composeInclude `json:"include"`
		Exclude []composeInclude `json:"exclude"`
	} `json:"compose"`
}

type composeInclude struct {
	System   string            `json:"system"`
	ValueSet []string          `json:"valueSet"`
	Filter   []json.RawMessage `json:"filter"`
	Concept  []struct {
		Code string `json:"code"`
	} `json:"concept"`
}

// definedCodeSystem is as much of a CodeSystem as an expansion needs.
type definedCodeSystem struct {
	URL     string          `json:"url"`
	Content string          `json:"content"`
	Concept []nestedConcept `json:"concept"`
}

// nestedConcept is one code, which may hold others beneath it.
type nestedConcept struct {
	Code    string          `json:"code"`
	Concept []nestedConcept `json:"concept"`
}

// readTerminology reads every code system and value set the bundles carry.
func readTerminology() (map[string]definedCodeSystem, map[string]composedValueSet, error) {
	systems := map[string]definedCodeSystem{}
	sets := map[string]composedValueSet{}

	held, err := bundled(func(definition Definition) bool {
		return definition.Type == "ValueSet" || definition.Type == "CodeSystem"
	})
	if err != nil {
		return nil, nil, err
	}

	for _, definition := range held {
		switch definition.Type {
		case "CodeSystem":
			var system definedCodeSystem
			if err := json.Unmarshal(definition.Content, &system); err != nil {
				return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, definition.ID, err)
			}

			systems[system.URL] = system

		case "ValueSet":
			var set composedValueSet
			if err := json.Unmarshal(definition.Content, &set); err != nil {
				return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, definition.ID, err)
			}

			sets[set.URL] = set
		}
	}

	return systems, sets, nil
}

// expand works out which codes one value set admits, and reports whether it
// could work it out at all.
//
// It handles the two shapes R4's required bindings are written in: an include
// listing concepts, and an include naming a code system whose own definition is
// complete. Anything else — a filter, a nested value set, an exclusion, a system
// defined somewhere this build does not hold — is refused rather than guessed
// at, because a set that was half worked out would refuse codes that are in it.
func expand(
	set composedValueSet, systems map[string]definedCodeSystem,
) (Admitted, bool) {
	if len(set.Compose.Exclude) != 0 || len(set.Compose.Include) == 0 {
		return Admitted{}, false
	}

	codes := map[Coded]bool{}

	for _, include := range set.Compose.Include {
		if len(include.Filter) != 0 || len(include.ValueSet) != 0 || include.System == "" {
			return Admitted{}, false
		}

		if len(include.Concept) != 0 {
			for _, concept := range include.Concept {
				codes[Coded{System: include.System, Code: concept.Code}] = true
			}

			continue
		}

		system, defined := systems[include.System]
		if !defined || system.Content != "complete" {
			return Admitted{}, false
		}

		for _, code := range flattenConcepts(system.Concept) {
			codes[Coded{System: include.System, Code: code}] = true
		}
	}

	if len(codes) == 0 {
		return Admitted{}, false
	}

	return Admitted{codes: codes}, true
}

// flattenConcepts reads every code a system defines, including the ones nested
// beneath another. A code that is a child of another is still a code.
func flattenConcepts(held []nestedConcept) []string {
	var codes []string

	for _, concept := range held {
		if concept.Code != "" {
			codes = append(codes, concept.Code)
		}

		codes = append(codes, flattenConcepts(concept.Concept)...)
	}

	return codes
}
