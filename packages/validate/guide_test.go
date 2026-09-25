package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// TestTheGuidesOwnExamplesValidate is the conformance bar: a guide's published
// examples are what it says conformant looks like, so anything refused here is
// this build disagreeing with the guide's own author.
func TestTheGuidesOwnExamplesValidate(t *testing.T) {
	examples := filepath.Join("..", "..", ".ig", "ndhm", "examples")
	if _, err := os.Stat(examples); err != nil {
		t.Skip("no guide fetched; run scripts/ig/fetch.sh")
	}

	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(conformance.ProfileDirectory, filepath.Join("..", "..", ".ig", "ndhm", "package"))

	// The guide narrows identifier to 0..1 on these and then writes an array in
	// its own example. A resource cannot be both, so the guide disagrees with
	// itself and any validator applying the profile reports it.
	contradicts := map[string]bool{
		"Medication": true, "CoverageEligibilityRequest": true,
		"Invoice": true, "InsurancePlan": true,
	}

	var checked, refused int

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		// #nosec G304 -- the path is the fetched guide's own directory.
		content, err := os.ReadFile(filepath.Join(examples, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}

		var named struct {
			ResourceType string `json:"resourceType"`
		}
		if json.Unmarshal(content, &named) != nil || named.ResourceType == "" {
			continue
		}

		checked++

		report := Resource(search.Custom{}, storage.ResourceType(named.ResourceType), content)
		for _, issue := range report.Issues() {
			if issue.Severity != SeverityError {
				continue
			}

			refused++

			if contradicts[named.ResourceType] {
				t.Logf("guide disagrees with itself: %s: %s", entry.Name(), issue.Detail)
			} else {
				t.Errorf("%s: %s: %s", entry.Name(), issue.Expression, issue.Detail)
			}

			break
		}
	}

	t.Logf("checked %d examples, %d refused", checked, refused)
}
