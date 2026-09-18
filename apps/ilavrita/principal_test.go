package main

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func readRequest(t *testing.T) (*core.RequestEvent, *httptest.ResponseRecorder) {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fhir.BasePath+"/Patient/example", nil)

	return &core.RequestEvent{Event: router.Event{Response: recorder, Request: request}}, recorder
}

// serve stands in for startup, which no test runs: it wires one backend for the
// duration of a test and leaves the package unwired again afterwards.
func serve(t *testing.T, wired *backend) {
	t.Helper()

	serving = wired
	t.Cleanup(func() { serving = nil })
}

func readPatient() decision {
	return decision{Kind: storage.KindFHIR, Type: "Patient", Action: storage.ActionRead}
}

// TestNoEnvironmentVariableNamesAPrincipal. The development principal was the
// one path that proved nothing, and it is gone: a request authenticates through
// a session or it reaches nothing at all.
func TestNoEnvironmentVariableNamesAPrincipal(t *testing.T) {
	for _, name := range []string{"principal.go", "server.go", "auth.go"} {
		source := readSource(t, name)

		if strings.Contains(source, "os.Getenv") && strings.Contains(source, "caller{") {
			t.Errorf("%s builds a caller from the environment", name)
		}

		if strings.Contains(source, "ILAVRITA_DEV_PRINCIPAL") {
			t.Errorf("%s still names the development principal", name)
		}
	}
}

// readSource reads one file in this package, through the package directory as a
// file system so a source-reading test cannot be handed a path that leaves it.
func readSource(t *testing.T, name string) string {
	t.Helper()

	source, err := fs.ReadFile(os.DirFS("."), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(source)
}

// TestARequestCarryingNoSessionIdentifiesNobody. Deny by default is the state a
// deployment runs in, and nothing falls back to an implicit identity.
func TestARequestCarryingNoSessionIdentifiesNobody(t *testing.T) {
	request, _ := readRequest(t)
	wired := &backend{sessions: fixedSession{proj: "clinic-a", principal: conformancePrincipal}}

	if _, err := wired.resolve(request); !errors.Is(err, errNoPrincipal) {
		t.Fatalf("a request with no session resolved: %v", err)
	}
}

// TestASessionNamesTheProjectItWasIssuedFor, which is the only Project a by-key
// request can ever reach (REST-36).
func TestASessionNamesTheProjectItWasIssuedFor(t *testing.T) {
	request, _ := readRequest(t)
	request.Request.Header.Set(authorizationField, bearerPrefix+"a-token")

	wired := &backend{sessions: fixedSession{proj: "clinic-a", principal: conformancePrincipal}}

	decided, err := wired.authorizationRequest(request, readPatient())
	if err != nil {
		t.Fatalf("build an authorization request: %v", err)
	}

	if decided.Project != project.ID("clinic-a") || decided.LinkedProjects != nil {
		t.Fatalf("request reached %q plus %v, want clinic-a alone", decided.Project, decided.LinkedProjects)
	}

	if decided.Now.IsZero() {
		t.Fatal("request carries no decision instant, which authz refuses")
	}

	if decided.Kind != storage.KindFHIR || decided.Type != "Patient" || decided.Action != storage.ActionRead {
		t.Fatalf("request decides %s/%s/%s, want the triple it was asked for",
			decided.Kind, decided.Type, decided.Action)
	}
}

// RFC 9110 15.5.2 makes a challenge mandatory on 401, and a client that follows
// the specification waits for one before it tries to authenticate at all.
func TestAnUnauthenticatedAnswerCarriesAChallenge(t *testing.T) {
	request, recorder := readRequest(t)

	if err := refuse(request, errNoPrincipal); err != nil {
		t.Fatalf("write the unauthenticated response: %v", err)
	}

	assertIssue(t, recorder, http.StatusUnauthorized, fhir.CodeLogin)

	if got := recorder.Header().Get(authenticateField); got != authenticateChallenge {
		t.Fatalf("%s %q, want %q", authenticateField, got, authenticateChallenge)
	}
}
