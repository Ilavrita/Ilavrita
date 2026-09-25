package conformance

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
)

// ProfileDirectory names a directory holding an implementation guide. No guide
// is vendored here: one is somebody else's artefact under somebody else's terms.
const ProfileDirectory = "ILAVRITA_PROFILE_DIR"

// Profiles is every profile this build can check a resource against, by the URL
// a resource names in meta.profile.
type Profiles struct {
	held map[string]Structure
}

// Profile returns one profile, and reports whether this build resolved it. One
// it does not hold decides nothing, the same as an unresolved value set.
func (p Profiles) Profile(url string) (Structure, bool) {
	structure, found := p.held[strings.SplitN(url, "|", 2)[0]]

	return structure, found
}

// URLs lists what was resolved, for a test that has to cover it.
func (p Profiles) URLs() []string { return slices.Sorted(maps.Keys(p.held)) }

// ProfilesFrom reads the profiles in a guide. Only a snapshot is read: a guide
// that ships differentials alone states its narrowing relative to a base this
// build would have to expand, and that expansion is not done here.
func ProfilesFrom(dir string) (Profiles, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Profiles{}, fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, dir, err)
	}
	defer func() { _ = root.Close() }()

	held := root.FS()
	found := map[string]Structure{}

	walk := func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		content, err := fs.ReadFile(held, path)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnreadableDefinitions, path, err)
		}

		var named struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(content, &named) != nil || named.URL == "" {
			return nil
		}

		if structure, ok := structureOf(content, "constraint"); ok {
			found[named.URL] = structure
		}

		return nil
	}

	if err := fs.WalkDir(held, ".", walk); err != nil {
		return Profiles{}, err
	}

	return Profiles{held: found}, nil
}
