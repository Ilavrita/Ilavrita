package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
)

func guideDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join("..", "..", ".ig", "ndhm", "package")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no guide fetched; run scripts/ig/fetch.sh")
	}

	return dir
}

// profiles is read once per process, so a test supplying one must be the first
// to read it. Each case builds its own Report instead.
func reportAgainst(t *testing.T, dir string, body string) Report {
	t.Helper()

	var held map[string]any
	if err := json.Unmarshal([]byte(body), &held); err != nil {
		t.Fatal(err)
	}

	model, err := conformance.Definitions()
	if err != nil {
		t.Skip("this build embeds no definitions")
	}

	resolved, err := conformance.ProfilesFrom(dir)
	if err != nil {
		t.Fatal(err)
	}

	var report Report
	for _, url := range declaredProfiles(held) {
		structure, found := resolved.Profile(url)
		if !found {
			report.note(SeverityWarning, "Patient.meta.profile",
				"This install does not hold "+url+", so the resource was not checked against it.")

			continue
		}

		report.checkObject(model, structure, "", held, "Patient", 0)
	}

	return report
}

func TestAnUnheldProfileIsReportedNotPassed(t *testing.T) {
	report := reportAgainst(t, t.TempDir(), `{"resourceType":"Patient",
		"meta":{"profile":["http://example.org/nobody-holds-this"]}}`)

	if len(report.Issues()) == 0 {
		t.Fatal("a profile this install does not hold passed silently")
	}

	if !strings.Contains(report.Issues()[0].Detail, "does not hold") {
		t.Errorf("unexpected issue: %s", report.Issues()[0].Detail)
	}
}

func TestAResourceMissingWhatItsProfileRequiresIsRefused(t *testing.T) {
	dir := guideDir(t)

	resolved, err := conformance.ProfilesFrom(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Find a Patient profile that narrows something to min=1.
	var chosen string
	for _, url := range resolved.URLs() {
		structure, _ := resolved.Profile(url)
		if structure.Type != "Patient" {
			continue
		}

		for _, element := range structure.Elements {
			if element.Required() {
				chosen = url

				break
			}
		}

		if chosen != "" {
			break
		}
	}

	if chosen == "" {
		t.Skip("the guide narrows no Patient element to required")
	}

	report := reportAgainst(t, dir, `{"resourceType":"Patient",
		"meta":{"profile":["`+chosen+`"]}}`)

	if len(report.Issues()) == 0 {
		t.Errorf("a Patient missing what %s requires was accepted", chosen)
	}
}

func TestAResourceDeclaringNoProfileIsUnaffected(t *testing.T) {
	report := reportAgainst(t, guideDir(t), `{"resourceType":"Patient","id":"x"}`)

	if len(report.Issues()) != 0 {
		t.Errorf("a resource declaring no profile gained issues: %v", report.Issues())
	}
}
