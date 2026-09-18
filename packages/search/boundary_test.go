package search_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestThisPackageHoldsNoSQL. A plan is backend-independent or it is not a plan:
// the moment a statement is written here, this package knows how one backend
// stores things and every other backend has to match it (SRC-6).
//
// The depguard rule keeps the runtime out; this keeps its dialect out too,
// which no import can express.
func TestThisPackageHoldsNoSQL(t *testing.T) {
	// Fragments that only appear in a statement. "FROM" and "WHERE" alone are
	// too common in prose to name here, so each carries the punctuation or the
	// neighbour that makes it SQL.
	statements := []string{
		"SELECT ", "INSERT INTO", "UPDATE ", "DELETE FROM", " JOIN ",
		"CREATE TABLE", "CREATE INDEX", " FROM ", " WHERE ", "json_extract", "json_each",
	}

	sources, err := fs.Glob(os.DirFS("."), "*.go")
	if err != nil {
		t.Fatalf("list the package: %v", err)
	}

	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		body, err := fs.ReadFile(os.DirFS("."), name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		for _, fragment := range statements {
			if strings.Contains(string(body), fragment) {
				t.Errorf("%s carries %q, which is a backend's dialect rather than a plan", name, fragment)
			}
		}
	}
}
