package main

import (
	"net/http"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func TestServesRuntimeSurfaceMatchesPocketBasePaths(t *testing.T) {
	pocketBasePaths := []string{
		"/_/",
		"/_/index.html",
		"/api/health",
		"/api/collections",
		"/api/files/collection/record/file.png",
	}

	for _, path := range pocketBasePaths {
		if !servesRuntimeSurface(path) {
			t.Errorf("%q should be recognised as PocketBase surface", path)
		}
	}
}

func TestServesRuntimeSurfaceLeavesIlavritaRoutesAlone(t *testing.T) {
	ilavritaPaths := []string{
		"/healthz",
		"/version",
		"/fhir/R4/metadata",
		"/fhir/R4/Patient/123",
	}

	for _, path := range ilavritaPaths {
		if servesRuntimeSurface(path) {
			t.Errorf("%q is an Ilavrita route and must stay reachable", path)
		}
	}
}

// A prefix match that ignored the separator would also block a future Ilavrita
// route whose name merely starts with the same letters.
func TestServesRuntimeSurfaceRequiresAPathSeparator(t *testing.T) {
	neighbours := []string{"/apidocs", "/api-reference", "/_internal"}

	for _, path := range neighbours {
		if servesRuntimeSurface(path) {
			t.Errorf("%q is not PocketBase surface", path)
		}
	}
}

// originField is what a browser sends and what a CORS policy answers on.
const originField = "Origin"

const allowOriginField = "Access-Control-Allow-Origin"

// runtimeRoutes builds what the process actually serves: the runtime's own CORS
// policy bound at the root, with the Ilavrita routes registered beneath it.
func runtimeRoutes(t *testing.T) http.Handler {
	t.Helper()

	publishAt(t, testHost)

	routes := router.NewRouter(
		func(response http.ResponseWriter, request *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
			return &core.RequestEvent{Event: router.Event{Response: response, Request: request}}, nil
		})

	routes.Bind(apis.CORS(apis.CORSConfig{}))
	registerOperationalRoutes(routes)
	registerFHIRRoutes(routes)

	mux, err := routes.BuildMux()
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}

	return mux
}

// No script on another origin may read patient data. The runtime allows every
// origin by default, so the FHIR surface has to withdraw that policy rather
// than inherit it, preflight included.
func TestTheFHIRSurfaceCarriesNoCrossOriginHeaders(t *testing.T) {
	serve(t, &backend{})

	routes := runtimeRoutes(t)
	const forged = "https://evil.example"

	for _, attempt := range []call{
		{method: http.MethodGet, path: fhir.BasePath + metadataPath, origin: forged},
		{method: http.MethodOptions, path: resourcePath("Organization", "example"), origin: forged},
		{method: http.MethodGet, path: resourcePath("Organization", "example"), origin: forged},
	} {
		answer := attempt.send(t, routes)
		if got := answer.Header().Get(allowOriginField); got != "" {
			t.Errorf("%s %s answered %s: %q", attempt.method, attempt.path, allowOriginField, got)
		}
	}
}

// And the withdrawal is confined to the FHIR base path: monitoring a deployment
// must not start depending on what the healthcare surface decided.
func TestTheRuntimeKeepsItsOwnPolicyOutsideTheFHIRSurface(t *testing.T) {
	routes := runtimeRoutes(t)

	answer := call{method: http.MethodGet, path: healthPath, origin: "https://evil.example"}.send(t, routes)
	if answer.Header().Get(allowOriginField) == "" {
		t.Error("the runtime's own CORS policy was withdrawn outside the FHIR surface")
	}
}
