package conformance

import (
	"os"
	"path/filepath"
	"testing"
)

// A licensed release is not in this repository, so the fixture stands in for
// one: the shape is what matters, not the content.
func writeRelease(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("codesystem.json", `{"resourceType":"CodeSystem","url":"http://snomed.info/sct",
		"content":"complete","concept":[{"code":"38341003"},{"code":"73211009"}]}`)
	write("valueset.json", `{"resourceType":"ValueSet","url":"http://example.org/vs/conditions",
		"compose":{"include":[{"system":"http://snomed.info/sct"}]}}`)
	write("manifest.json", `{"resourceType":"ImplementationGuide","url":"http://example.org/ig"}`)
	write("notes.txt", "not json")
	write(filepath.Join("nested", "extra.json"), `{"resourceType":"ValueSet","url":"http://example.org/vs/nested",
		"compose":{"include":[{"system":"http://snomed.info/sct","concept":[{"code":"38341003"}]}]}}`)

	return root
}

func TestSuppliedResolvesASetFromALicensedRelease(t *testing.T) {
	held, err := Supplied(writeRelease(t))
	if err != nil {
		t.Fatalf("reading the release: %v", err)
	}

	admitted, resolved := held.Admits("http://example.org/vs/conditions")
	if !resolved {
		t.Fatal("a set the release defines was not resolved")
	}

	if !admitted.Holds(Coded{System: "http://snomed.info/sct", Code: "38341003"}) {
		t.Error("a code the release defines was not admitted")
	}
}

func TestSuppliedWalksBeneathTheRoot(t *testing.T) {
	held, err := Supplied(writeRelease(t))
	if err != nil {
		t.Fatal(err)
	}

	if _, resolved := held.Admits("http://example.org/vs/nested"); !resolved {
		t.Error("a definition in a subdirectory was not read")
	}
}

// A release ships its manifest and its notes alongside the definitions; neither
// is terminology and neither may stop the load.
func TestSuppliedIgnoresWhatIsNotTerminology(t *testing.T) {
	if _, err := Supplied(writeRelease(t)); err != nil {
		t.Errorf("a non-terminology file stopped the load: %v", err)
	}
}

// Without a licence an install still validates; it just decides less.
func TestEmbeddedSetsSurviveASuppliedRelease(t *testing.T) {
	embedded, err := Terminologies()
	if err != nil {
		t.Skip("this build embeds no terminology")
	}

	sets := embedded.Sets()
	if len(sets) == 0 {
		t.Skip("this build embeds no sets")
	}

	held, err := Supplied(writeRelease(t))
	if err != nil {
		t.Fatal(err)
	}

	if _, resolved := held.Admits(sets[0]); !resolved {
		t.Errorf("supplying a release dropped the embedded set %q", sets[0])
	}
}

func TestSuppliedReportsAMissingDirectory(t *testing.T) {
	if _, err := Supplied(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing directory was accepted silently")
	}
}

// A release is unpacked by whoever obtained it, and an archive can carry a
// symlink. The walk is confined so one cannot read outside the directory named.
func TestSuppliedDoesNotFollowASymlinkOutOfTheDirectory(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"resourceType":"ValueSet","url":"http://example.org/vs/secret",
		"compose":{"include":[{"system":"http://snomed.info/sct"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	root := writeRelease(t)
	if err := os.Symlink(secret, filepath.Join(root, "escape.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	held, err := Supplied(root)
	if err != nil {
		t.Fatalf("a symlink stopped the load: %v", err)
	}

	if _, resolved := held.Admits("http://example.org/vs/secret"); resolved {
		t.Error("the walk followed a symlink outside the directory it was given")
	}
}
