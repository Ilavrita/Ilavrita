package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// everyTypeLevelInteraction is R4's own type-restful-interaction value set. The
// statement may advertise the part of it the routes register, and none of the
// rest, so this list is what the sweeps below are complete against.
var everyTypeLevelInteraction = []fhir.Interaction{
	"create", "read", "vread", "update", "patch", "delete",
	"history-instance", "history-type", "search-type",
}

// The URL shapes the FHIR base path can carry, and the methods a client may
// reach them with. Every pair is swept, so nothing is served unadvertised.
var (
	everyPathShape = []string{typePath, typeHistoryPath, instancePath, instanceHistoryPath, instanceVersionPath}
	everyMethod    = []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	}
)

// request fills the route's own template, so a test names an interaction rather
// than retyping the path the router matched it on.
func (s servedInteraction) request(resourceType, id string) call {
	path := strings.NewReplacer(
		"{"+resourceTypeParameter+"}", resourceType,
		"{"+idParameter+"}", id,
		"{"+versionParameter+"}", "1",
	).Replace(s.path)

	sent := call{method: s.method, path: fhir.BasePath + path}

	switch {
	case s.path == typeSearchPath:
		// A posted search states a query, not a resource.
		sent.body, sent.contentType = "_id="+id, formContentType
	case s.method == http.MethodPost:
		sent.body = submission(resourceType)
	case s.method == http.MethodPut:
		sent.body = replacement(resourceType, id)
	}

	return sent
}

// advertised reads the statement the metadata route actually serves, so these
// tests assert what a client receives rather than what a constructor returns.
func advertised(t *testing.T) []fhir.ResourceCapability {
	t.Helper()

	answer := call{method: http.MethodGet, path: fhir.BasePath + metadataPath}.send(t, fhirRoutes(t))
	assertStatus(t, answer, http.StatusOK)

	var statement fhir.CapabilityStatement
	if err := json.Unmarshal(answer.Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode the CapabilityStatement: %v; body %s", err, answer.Body.String())
	}

	if len(statement.Rest) != 1 {
		t.Fatalf("the statement declares %d rest endpoint(s), want 1", len(statement.Rest))
	}

	return statement.Rest[0].Resource
}

// registeredRoute finds the route an advertised interaction claims to have.
func registeredRoute(code fhir.Interaction) (servedInteraction, bool) {
	for _, served := range servedInteractions {
		if served.code == code {
			return served, true
		}
	}

	return servedInteraction{}, false
}

func isRegistered(method, path string) bool {
	return slices.ContainsFunc(servedInteractions, func(served servedInteraction) bool {
		return served.method == method && served.path == path
	})
}

// The statement is built from the table the routes were registered from, so
// every advertised code names a registered route and a real R4 interaction.
func TestEveryAdvertisedInteractionNamesARegisteredRoute(t *testing.T) {
	for _, resource := range advertised(t) {
		// The distinct codes, not the rows: one interaction may be served by
		// more than one route, and a search is.
		if len(resource.Interaction) != len(advertisedInteractions()) {
			t.Fatalf("%s advertises %d interaction(s), want %d",
				resource.Type, len(resource.Interaction), len(advertisedInteractions()))
		}

		for _, interaction := range resource.Interaction {
			if _, found := registeredRoute(interaction.Code); !found {
				t.Errorf("%s advertises %q, which no route is registered for", resource.Type, interaction.Code)
			}

			if !slices.Contains(everyTypeLevelInteraction, interaction.Code) {
				t.Errorf("%s advertises %q, which R4 does not define", resource.Type, interaction.Code)
			}
		}
	}
}

// And the advertisement is not a claim: every advertised interaction reaches a
// handler for every advertised type. Nothing identifies the caller here, so a
// handler answers 401, where the unimplemented wildcard would answer 501.
func TestEveryAdvertisedInteractionReachesAHandler(t *testing.T) {
	serve(t, &backend{})

	routes := fhirRoutes(t)

	for _, resource := range advertised(t) {
		for _, interaction := range resource.Interaction {
			served, found := registeredRoute(interaction.Code)
			if !found {
				t.Fatalf("%s advertises %q, which no route is registered for", resource.Type, interaction.Code)
			}

			answer := served.request(resource.Type, "example").send(t, routes)
			assertIssue(t, answer, http.StatusUnauthorized, fhir.CodeLogin)
		}
	}
}

// The other direction, swept over the whole URL grammar: a method and shape the
// table does not register reaches no handler at all, so nothing this server
// serves can go unadvertised.
func TestNothingOutsideTheTableReachesAHandler(t *testing.T) {
	serve(t, &backend{})

	routes := fhirRoutes(t)

	for _, shape := range everyPathShape {
		for _, method := range everyMethod {
			answer := servedInteraction{method: method, path: shape}.request("Organization", "example").send(t, routes)

			if isRegistered(method, shape) {
				assertIssue(t, answer, http.StatusUnauthorized, fhir.CodeLogin)

				continue
			}

			assertIssue(t, answer, http.StatusNotImplemented, fhir.CodeNotSupported)
		}
	}
}

// R4's type-level value set minus the registered routes is what this build does
// not do: search, type-level history and patch. None of it may be advertised,
// and the sweep above is what proves none of it is served either.
func TestNoUnregisteredInteractionIsAdvertised(t *testing.T) {
	resources := advertised(t)

	for _, code := range everyTypeLevelInteraction {
		if _, found := registeredRoute(code); found {
			continue
		}

		for _, resource := range resources {
			if slices.ContainsFunc(resource.Interaction, func(declared fhir.ResourceInteraction) bool {
				return declared.Code == code
			}) {
				t.Errorf("%s advertises %q, which no route dispatches", resource.Type, code)
			}
		}
	}
}

// A type is advertised exactly when it is an endpoint: one list gates the routes
// and fills the statement, so a name in one is a name in the other.
func TestTheAdvertisedTypesAreExactlyTheEndpoints(t *testing.T) {
	var advertisedTypes []string

	for _, resource := range advertised(t) {
		if !fhir.ServesResourceType(resource.Type) {
			t.Errorf("%s is advertised but no route would serve it", resource.Type)
		}

		advertisedTypes = append(advertisedTypes, resource.Type)
	}

	if !slices.Equal(advertisedTypes, fhir.ServedResourceTypes()) {
		t.Fatalf("advertised %v, want %v", advertisedTypes, fhir.ServedResourceTypes())
	}
}

// A type carrying patient data is reachable only through a compartment-restricted
// grant, and a create is checked against the compartments the submitted resource
// declares. A clinical type this build cannot place in one would advertise a
// create nobody can perform, so the two lists have to agree.
func TestEveryAdvertisedClinicalTypeCanBePlaced(t *testing.T) {
	var clinical int

	for _, resource := range advertised(t) {
		if !authz.CarriesClinicalData(storage.ResourceType(resource.Type)) {
			continue
		}

		clinical++

		if !fhir.DerivesCompartments(resource.Type) {
			t.Errorf("%s is advertised, but no confined grant could ever reach one", resource.Type)
		}
	}

	if clinical == 0 {
		t.Error("no clinical type is advertised, so this proves nothing")
	}
}

// Every advertised type carries the versioning and update-as-create facts the
// handlers actually implement: a version on every record, and an update that
// creates the id a client names.
func TestEveryAdvertisedTypeDeclaresHowItBehaves(t *testing.T) {
	for _, resource := range advertised(t) {
		if resource.Versioning != "versioned" {
			t.Errorf("%s declares versioning %q, want versioned", resource.Type, resource.Versioning)
		}

		if !resource.UpdateCreate {
			t.Errorf("%s does not declare updateCreate, but update creates the id a client names", resource.Type)
		}
	}
}

// TestAnInteractionIsDeclaredOnce. One interaction is served by more than one
// route — a search answers a GET on the type and a POST to _search — and a
// statement naming it twice would be declaring two things a client can only do
// once.
func TestAnInteractionIsDeclaredOnce(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{method: http.MethodGet, path: fhir.BasePath + "/metadata"}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	var statement fhir.CapabilityStatement
	if err := json.Unmarshal(answer.Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode the statement: %v", err)
	}

	for _, resource := range statement.Rest[0].Resource {
		seen := map[fhir.Interaction]bool{}

		for _, interaction := range resource.Interaction {
			if seen[interaction.Code] {
				t.Errorf("%s declares %s twice", resource.Type, interaction.Code)
			}

			seen[interaction.Code] = true
		}

		// And the ones that are served are all there, so deduplicating did not
		// drop one.
		if len(seen) != len(advertisedInteractions()) {
			t.Errorf("%s declares %d interactions, and %d are served",
				resource.Type, len(seen), len(advertisedInteractions()))
		}
	}
}
