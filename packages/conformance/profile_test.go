package conformance

import (
	"os"
	"path/filepath"
	"testing"
)

// The guide is fetched by scripts/ig/fetch.sh and never committed, so a run
// without it skips rather than fails.
func guide(t *testing.T) string {
	t.Helper()

	dir := filepath.Join("..", "..", ".ig", "ndhm", "package")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no guide fetched; run scripts/ig/fetch.sh")
	}

	return dir
}

func TestProfilesFromReadsAGuide(t *testing.T) {
	held, err := ProfilesFrom(guide(t))
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	if len(held.URLs()) == 0 {
		t.Fatal("the guide resolved no profile")
	}
}

func TestAProfileIsFoundByTheURLAResourceNames(t *testing.T) {
	held, err := ProfilesFrom(guide(t))
	if err != nil {
		t.Fatal(err)
	}

	url := held.URLs()[0]
	if _, found := held.Profile(url); !found {
		t.Errorf("a profile the guide defines was not found by its own URL %q", url)
	}
}

// A resource names a profile with a version after a pipe; the profile is the
// same one.
func TestAVersionedURLFindsTheProfile(t *testing.T) {
	held, err := ProfilesFrom(guide(t))
	if err != nil {
		t.Fatal(err)
	}

	url := held.URLs()[0]
	if _, found := held.Profile(url + "|6.5.0"); !found {
		t.Error("a versioned profile URL did not resolve")
	}
}

func TestAProfileCarriesTheNarrowingItStates(t *testing.T) {
	held, err := ProfilesFrom(guide(t))
	if err != nil {
		t.Fatal(err)
	}

	narrowed := 0

	for _, url := range held.URLs() {
		structure, _ := held.Profile(url)
		for _, element := range structure.Elements {
			if element.Required() {
				narrowed++
			}
		}
	}

	if narrowed == 0 {
		t.Error("no profile narrowed any element; the snapshot was not read")
	}
}

// A base definition must not be read as a profile, or every resource would be
// checked against somebody's narrowing of it.
func TestABaseDefinitionIsNotAProfile(t *testing.T) {
	dir := t.TempDir()
	base := `{"resourceType":"StructureDefinition","url":"http://example.org/base",
		"derivation":"specialization","kind":"resource","id":"Patient",
		"snapshot":{"element":[{"path":"Patient"},{"path":"Patient.name","min":1,"max":"1"}]}}`

	if err := os.WriteFile(filepath.Join(dir, "base.json"), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := ProfilesFrom(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, found := held.Profile("http://example.org/base"); found {
		t.Error("a base definition was loaded as a profile")
	}
}
