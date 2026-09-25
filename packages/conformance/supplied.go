package conformance

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// SuppliedDirectory names a directory of CodeSystem and ValueSet JSON this
// install obtained under its own licence. SNOMED CT and LOINC are not
// redistributed here; see docs/terminology.md.
const SuppliedDirectory = "ILAVRITA_TERMINOLOGY_DIR"

// Supplied reads terminology a deployment holds a licence for and merges it
// over what this build embeds. A supplied definition wins: an install that
// obtained a newer release means to use it.
func Supplied(root string) (Terminology, error) {
	systems, sets, err := readTerminology()
	if err != nil {
		return Terminology{}, err
	}

	if err := readSupplied(root, systems, sets); err != nil {
		return Terminology{}, err
	}

	resolved := map[string]Admitted{}

	for url, set := range sets {
		if admitted, ok := expand(set, systems); ok {
			resolved[url] = admitted
		}
	}

	return Terminology{sets: resolved}, nil
}

// readSupplied walks the directory, reading every CodeSystem and ValueSet it
// holds. A file that is neither is skipped rather than refused: a release ships
// its manifest and its examples alongside the definitions.
//
// os.Root confines the walk, so a symlink inside a release cannot reach a file
// outside the directory the operator named.
func readSupplied(
	dir string, systems map[string]definedCodeSystem, sets map[string]composedValueSet,
) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, dir, err)
	}
	defer func() { _ = root.Close() }()

	held := root.FS()

	return fs.WalkDir(held, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}

		content, err := fs.ReadFile(held, path)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, path, err)
		}

		var named struct {
			ResourceType string `json:"resourceType"`
			URL          string `json:"url"`
		}
		if json.Unmarshal(content, &named) != nil || named.URL == "" {
			return nil
		}

		switch named.ResourceType {
		case "CodeSystem":
			var system definedCodeSystem
			if err := json.Unmarshal(content, &system); err != nil {
				return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, path, err)
			}

			systems[system.URL] = system

		case "ValueSet":
			var set composedValueSet
			if err := json.Unmarshal(content, &set); err != nil {
				return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, path, err)
			}

			sets[set.URL] = set
		}

		return nil
	})
}
