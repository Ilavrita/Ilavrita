package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
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

// Nothing configured is the state a deployment runs in by default, and it must
// identify nobody rather than fall back to some implicit identity.
func TestAbsentDevelopmentPrincipalIdentifiesNobody(t *testing.T) {
	request, _ := readRequest(t)

	if _, err := (&backend{}).resolve(request); !errors.Is(err, errNoPrincipal) {
		t.Fatalf("resolve without a development principal: %v, want errNoPrincipal", err)
	}
}

func TestAuthorizeWithoutAPrincipalReachesNoScope(t *testing.T) {
	request, _ := readRequest(t)
	serve(t, &backend{})

	granted, err := authorize(request, readPatient())
	if !errors.Is(err, errNoPrincipal) {
		t.Fatalf("authorize without a development principal: %v, want errNoPrincipal", err)
	}

	if !granted.Scope.IsEmpty() || granted.Resources != nil || granted.Versions != nil {
		t.Fatal("a refused request must reach neither a Scope nor a store")
	}
}

// A route reaching the wiring before startup built it must deny, not panic and
// not answer from a database nobody opened.
func TestAuthorizeWithoutAWiredBackendRefuses(t *testing.T) {
	request, _ := readRequest(t)

	if _, err := authorize(request, readPatient()); !errors.Is(err, errNotServing) {
		t.Fatalf("authorize on an unwired server: %v, want errNotServing", err)
	}
}

func TestConfiguredDevelopmentPrincipalUnsetIsNotAFailure(t *testing.T) {
	t.Setenv(developmentPrincipalVariable, "")

	configured, err := configuredDevelopmentPrincipal()
	if err != nil {
		t.Fatalf("unset %s: %v", developmentPrincipalVariable, err)
	}

	if configured != nil {
		t.Fatalf("unset %s resolved to %+v, want no principal", developmentPrincipalVariable, configured)
	}
}

func TestConfiguredDevelopmentPrincipalReadsTheWholeValue(t *testing.T) {
	t.Setenv(developmentPrincipalVariable, "clinic-a:client_application:loader")

	configured, err := configuredDevelopmentPrincipal()
	if err != nil {
		t.Fatalf("parse a well-formed value: %v", err)
	}

	want := caller{
		project:   project.ID("clinic-a"),
		principal: project.PrincipalRef{Kind: project.PrincipalClientApplication, ID: "loader"},
	}

	if *configured != want {
		t.Fatalf("configured principal %+v, want %+v", *configured, want)
	}
}

// Every one of these is a typo an operator could plausibly make, and each must
// stop the process rather than degrade it to a principal nobody intended.
func TestMalformedDevelopmentPrincipalRefusesToStart(t *testing.T) {
	malformed := map[string]string{
		"no parts":           "clinic-a",
		"two parts":          "clinic-a:user",
		"four parts":         "clinic-a:user:alice:extra",
		"empty project":      ":user:alice",
		"wildcard project":   "*:user:alice",
		"unknown kind":       "clinic-a:wizard:alice",
		"empty kind":         "clinic-a::alice",
		"empty principal id": "clinic-a:user:",
		"blank principal id": "clinic-a:user:   ",
		"nothing but colons": "::",
	}

	for name, value := range malformed {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDevelopmentPrincipal(value); err == nil {
				t.Fatalf("%q was accepted, want a refusal", value)
			}
		})
	}
}

// The configured Project is the only one a by-key request can ever name, which
// is what keeps a Project-A principal from reaching Project B (REST-36).
func TestAuthorizationRequestNamesOnlyTheConfiguredProject(t *testing.T) {
	request, _ := readRequest(t)
	wired := &backend{developmentPrincipal: &caller{
		project:   project.ID("clinic-a"),
		principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "alice"},
	}}

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
