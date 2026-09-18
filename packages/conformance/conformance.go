// Package conformance carries the FHIR R4 base definitions this build serves.
//
// They are the specification's own, embedded rather than fetched: a healthcare
// server that reached the network at startup to find out what a resource is
// would be one whose behaviour depends on somebody else's uptime.
package conformance

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

//go:embed definitions/profiles-resources.json.gz definitions/profiles-types.json.gz
var bundles embed.FS

// The bundles this build embeds, in the order they are read. Resources first,
// so a digest of the pair is stable rather than depending on a map's ordering.
var embedded = []string{
	"definitions/profiles-resources.json.gz",
	"definitions/profiles-types.json.gz",
}

// Release is the FHIR version these definitions are from. It is stated here as
// well as in the resources themselves, so what an install seeded is readable
// without parsing thirty-five megabytes of JSON to find out.
const Release = "4.0.1"

// ErrUnreadableDefinitions reports embedded definitions this build cannot read,
// which is a broken build rather than a broken install.
var ErrUnreadableDefinitions = errors.New("conformance: the embedded definitions cannot be read")

// Definition is one canonical resource as it will be stored.
//
// Content is the resource exactly as the specification publishes it. Nothing is
// trimmed: a client reading StructureDefinition/Observation is reading what HL7
// wrote, and a reduced copy would be something else wearing its name.
type Definition struct {
	Type    string
	ID      string
	URL     string
	Version string
	Content []byte
}

// Digest identifies what this build would seed.
//
// It is over the compressed bytes as embedded, so it changes when and only when
// the vendored files do. An install compares it with what it last seeded and, if
// they match, reads nothing else — which is what keeps a start that would change
// nothing from parsing the whole specification to find that out.
var Digest = sync.OnceValue(func() string {
	held := sha256.New()

	for _, name := range embedded {
		raw, err := bundles.ReadFile(name)
		if err != nil {
			// Embedded at build time; a build that cannot read its own files
			// is one that will fail the first test that asks for them.
			return ""
		}

		_, _ = held.Write(raw)
	}

	return hex.EncodeToString(held.Sum(nil))
})

// StructureDefinitions returns every base resource and datatype definition,
// ordered by type and id so two builds of the same bundles seed the same thing
// in the same order.
//
// It parses thirty-five megabytes to do it. That is why the caller asks for the
// digest first: this runs on a first start and after an upgrade, and on no other
// start.
func StructureDefinitions() ([]Definition, error) {
	var held []Definition

	for _, name := range embedded {
		found, err := definitionsIn(name)
		if err != nil {
			return nil, err
		}

		held = append(held, found...)
	}

	slices.SortFunc(held, func(a, b Definition) int {
		if named := strings.Compare(a.Type, b.Type); named != 0 {
			return named
		}

		return strings.Compare(a.ID, b.ID)
	})

	return held, nil
}

// definitionsIn reads one bundle.
func definitionsIn(name string) ([]Definition, error) {
	raw, err := bundles.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, name, err)
	}

	defer func() { _ = raw.Close() }()

	opened, err := gzip.NewReader(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, name, err)
	}

	defer func() { _ = opened.Close() }()

	return definitionsFrom(opened, name)
}

// bundleEntry is as much of a Bundle as this reads: the resource, kept raw so
// what is stored is the bytes the specification published rather than a
// re-encoding of them.
type bundleEntry struct {
	Resource json.RawMessage `json:"resource"`
}

// definitionsFrom reads the StructureDefinitions out of one bundle's entries.
//
// The entries are decoded one at a time rather than into one slice, because a
// bundle this size held twice — once as JSON and once as values — is a hundred
// megabytes of a server's memory at startup.
func definitionsFrom(held io.Reader, name string) ([]Definition, error) {
	decoder := json.NewDecoder(held)

	if err := seekEntries(decoder, name); err != nil {
		return nil, err
	}

	var found []Definition

	for decoder.More() {
		var entry bundleEntry
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, name, err)
		}

		definition, held := definitionOf(entry.Resource)
		if held {
			found = append(found, definition)
		}
	}

	return found, nil
}

// definitionOf reads what one entry is, and reports whether it is a definition
// this seeds. A bundle carries operations and compartments beside the
// definitions; what is seeded is what this build says it seeds.
func definitionOf(content json.RawMessage) (Definition, bool) {
	var held struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		URL          string `json:"url"`
		Version      string `json:"version"`
	}

	if err := json.Unmarshal(content, &held); err != nil {
		return Definition{}, false
	}

	if held.ResourceType != "StructureDefinition" || held.ID == "" || held.URL == "" {
		return Definition{}, false
	}

	return Definition{
		Type:    held.ResourceType,
		ID:      held.ID,
		URL:     held.URL,
		Version: held.Version,
		Content: slices.Clone(content),
	}, true
}

// seekEntries reads forward to the start of the bundle's entry array, leaving
// the decoder positioned on the first one.
func seekEntries(decoder *json.Decoder, name string) error {
	unreadable := func(err error) error {
		return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, name, err)
	}

	if _, err := decoder.Token(); err != nil {
		return unreadable(err)
	}

	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return unreadable(err)
		}

		if name, isKey := key.(string); !isKey || name != "entry" {
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				return unreadable(err)
			}

			continue
		}

		// The opening bracket of the entry array; the caller reads its members.
		if _, err := decoder.Token(); err != nil {
			return unreadable(err)
		}

		return nil
	}

	return fmt.Errorf("%w: %s holds no entries", ErrUnreadableDefinitions, name)
}
