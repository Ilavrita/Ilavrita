package main

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/dbx"
)

// jobsOnSuperusers reads what this install recorded about the table.
func jobsOnSuperusers(t *testing.T, records *sql.DB) ([]sqlite.Ran, error) {
	t.Helper()

	return sqlite.JobsOn(t.Context(), records, superuserTable, 10)
}

// runtimeWithSuperusers stands in for PocketBase's own data.db, holding as many
// superusers as the test wants to plant.
func runtimeWithSuperusers(t *testing.T, emails ...string) dbx.Builder {
	t.Helper()

	held, err := dbx.Open("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open the runtime database: %v", err)
	}

	t.Cleanup(func() { _ = held.Close() })

	if _, err := held.NewQuery(
		"CREATE TABLE {{_superusers}} (id TEXT PRIMARY KEY, email TEXT NOT NULL)").Execute(); err != nil {
		t.Fatalf("create the superusers table: %v", err)
	}

	for index, email := range emails {
		if _, err := held.NewQuery(
			"INSERT INTO {{_superusers}} (id, email) VALUES ({:id}, {:email})").Bind(dbx.Params{
			"id": string(rune('a'+index)) + "-planted", "email": email,
		}).Execute(); err != nil {
			t.Fatalf("plant %s: %v", email, err)
		}
	}

	return held
}

func superuserCount(t *testing.T, runtime dbx.Builder) int {
	t.Helper()

	var held int
	if err := runtime.NewQuery("SELECT count(*) FROM {{_superusers}}").Row(&held); err != nil {
		t.Fatalf("count the superusers: %v", err)
	}

	return held
}

// TestAPocketBaseSuperuserNeverSurvivesAStart. A superuser holds no Project, no
// Membership and no Grant, so nothing it does is narrowed by a compartment or
// written to the audit trail — and the console it unlocks can take a backup of
// the whole data directory. The table is reachable by things that are not this
// process, so the guarantee has to be re-established on every start rather than
// assumed from the command being gone.
func TestAPocketBaseSuperuserNeverSurvivesAStart(t *testing.T) {
	records := preparedDatabase(t)
	runtime := runtimeWithSuperusers(t, "one@example.com", "two@example.com")

	if err := refuseSuperusers(t.Context(), runtime, records); err != nil {
		t.Fatalf("refuse the superusers: %v", err)
	}

	if held := superuserCount(t, runtime); held != 0 {
		t.Errorf("%d superuser(s) survived the start", held)
	}

	// Removing a credential somebody made is worth a row. An operator who finds
	// their login stopped working should be able to read why.
	ran, err := jobsOnSuperusers(t, records)
	if err != nil {
		t.Fatalf("read the jobs: %v", err)
	}

	if len(ran) != 1 {
		t.Fatalf("recorded %d job(s), want 1", len(ran))
	}

	if ran[0].Outcome != "applied" {
		t.Errorf("the job is recorded as %q", ran[0].Outcome)
	}
}

// TestAnInstallWithNoSuperuserRecordsNothing. A server starts far more often
// than somebody plants a credential, and a row per start would bury the one
// start that mattered.
func TestAnInstallWithNoSuperuserRecordsNothing(t *testing.T) {
	records := preparedDatabase(t)
	runtime := runtimeWithSuperusers(t)

	for range 3 {
		if err := refuseSuperusers(t.Context(), runtime, records); err != nil {
			t.Fatalf("refuse the superusers: %v", err)
		}
	}

	ran, err := jobsOnSuperusers(t, records)
	if err != nil {
		t.Fatalf("read the jobs: %v", err)
	}

	if len(ran) != 0 {
		t.Errorf("an install that held none recorded %d job(s)", len(ran))
	}
}

// TestARuntimeThatCannotBeReadStopsTheServer. Not being able to tell whether a
// superuser exists is not a state to serve clinical data in, so it is an error
// rather than a warning nobody reads.
func TestARuntimeThatCannotBeReadStopsTheServer(t *testing.T) {
	records := preparedDatabase(t)

	held, err := dbx.Open("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open the runtime database: %v", err)
	}

	t.Cleanup(func() { _ = held.Close() })

	// No _superusers table at all, which is what a runtime this build does not
	// recognise looks like.
	if err := refuseSuperusers(t.Context(), held, records); err == nil {
		t.Error("a runtime whose superusers could not be read started anyway")
	}
}

// TestNothingRegistersASuperuserCommand. Removing the rows at start is half of
// it; the other half is that this binary never offers a way to make one. That
// is expressed by calling Execute rather than Start — a one-word difference
// somebody restoring "the normal PocketBase entry point" would undo without
// noticing, which is why it is a rule here rather than a comment.
func TestNothingRegistersASuperuserCommand(t *testing.T) {
	listed, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	sources := token.NewFileSet()
	held := map[string]*ast.File{}

	for _, entry := range listed {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		parsed, err := parser.ParseFile(sources, name, nil, 0)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		held[name] = parsed
	}

	// Start is refused only on the PocketBase app itself, so an unrelated
	// Start elsewhere in the package is not mistaken for this one.
	refused := map[string]string{"NewSuperuserCommand": "mints a superuser"}

	var registersServe bool

	for name, file := range held {
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.SelectorExpr)
			if !isCall {
				return true
			}

			if because, forbidden := refused[call.Sel.Name]; forbidden {
				t.Errorf("%s calls %s, which %s", name, call.Sel.Name, because)
			}

			if receiver, named := call.X.(*ast.Ident); named &&
				receiver.Name == "app" && call.Sel.Name == "Start" {
				t.Errorf("%s calls app.Start, which registers PocketBase's own "+
					"superuser command; call app.Execute instead", name)
			}

			if call.Sel.Name == "NewServeCommand" {
				registersServe = true
			}

			return true
		})
	}

	// And the rule means something only while this package registers serve for
	// itself. A build that went back to Start would serve nothing here and pass
	// the loop above by having no calls at all.
	if !registersServe {
		t.Error("nothing registers the serve command, so this rule guards nothing")
	}
}
