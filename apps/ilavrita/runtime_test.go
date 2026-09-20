package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// A browser app reaches this API cross-origin, and what makes that safe is the
// absence of one header rather than the presence of the others.
//
// Access-Control-Allow-Origin: * lets a page send a request. Without
// Access-Control-Allow-Credentials the browser attaches nothing ambient to it —
// no cookie, no stored authorization — so a hostile page has to already hold the
// bearer token, and a page holding the token never needed the browser's help.
// The two together would be every session reachable from every page, which is
// the pairing this asserts can never appear.
func TestTheFHIRSurfaceIsReachableCrossOriginAndCarriesNoAmbientCredential(t *testing.T) {
	serve(t, &backend{})

	routes := runtimeRoutes(t)
	const elsewhere = "https://app.example"

	for _, attempt := range []call{
		{method: http.MethodOptions, path: resourcePath("Organization", "example"), origin: elsewhere},
		{method: http.MethodGet, path: resourcePath("Organization", "example"), origin: elsewhere},
		{method: http.MethodGet, path: fhir.BasePath + "/Organization", origin: elsewhere},
		{method: http.MethodPost, path: fhir.BasePath, origin: elsewhere},
		{method: http.MethodGet, path: fhir.BasePath + "/_history", origin: elsewhere},
	} {
		answer := attempt.send(t, routes)

		if got := answer.Header().Get(allowOriginField); got != "*" && got != elsewhere {
			t.Errorf("%s %s answered %s: %q, and a browser app cannot reach it",
				attempt.method, attempt.path, allowOriginField, got)
		}

		// The one that must never be there.
		if got := answer.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("%s %s allows credentials alongside a wildcard origin: %q",
				attempt.method, attempt.path, got)
		}
	}
}

// TestABrowserAppCanSendWhatItNeedsAndReadWhatComesBack.
//
// A preflight that does not allow Authorization is a FHIR API no bearer token
// can reach, and a response whose ETag and Location are not exposed is one a
// browser app cannot do a conditional update against. Both are invisible from
// the server's side: the request succeeds and the browser discards it.
func TestABrowserAppCanSendWhatItNeedsAndReadWhatComesBack(t *testing.T) {
	serve(t, &backend{})

	routes := runtimeRoutes(t)

	sent := httptest.NewRequest(
		http.MethodOptions, resourcePath("Organization", "example"), nil)
	sent.Host = testHost
	sent.Header.Set("Origin", "https://app.example")
	sent.Header.Set("Access-Control-Request-Method", http.MethodPut)
	sent.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	answer := httptest.NewRecorder()
	routes.ServeHTTP(answer, sent)

	allowed := answer.Header().Get("Access-Control-Allow-Headers")
	for _, header := range []string{"Authorization", "Content-Type", "If-Match", "Prefer"} {
		if !strings.Contains(strings.ToLower(allowed), strings.ToLower(header)) {
			t.Errorf("the preflight does not allow %s, so a browser app cannot send it: %q",
				header, allowed)
		}
	}

	exposed := answer.Header().Get("Access-Control-Expose-Headers")
	if exposed == "" {
		// A preflight need not carry it; the simple request below is where a
		// browser actually reads it from.
		exposed = simpleOriginRequest(t, routes).Header().Get("Access-Control-Expose-Headers")
	}

	for _, header := range []string{"ETag", "Location"} {
		if !strings.Contains(exposed, header) {
			t.Errorf("%s is not exposed, so a browser app cannot read it back: %q", header, exposed)
		}
	}
}

// simpleOriginRequest sends one ordinary cross-origin read.
func simpleOriginRequest(t *testing.T, routes http.Handler) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(http.MethodGet, resourcePath("Organization", "example"), nil)
	sent.Host = testHost
	sent.Header.Set("Origin", "https://app.example")

	answer := httptest.NewRecorder()
	routes.ServeHTTP(answer, sent)

	return answer
}

// And what the FHIR surface decided stays there: monitoring a deployment must
// not start depending on what the healthcare surface chose.
func TestTheRuntimeKeepsItsOwnPolicyOutsideTheFHIRSurface(t *testing.T) {
	routes := runtimeRoutes(t)

	answer := call{method: http.MethodGet, path: healthPath, origin: "https://evil.example"}.send(t, routes)
	if answer.Header().Get(allowOriginField) == "" {
		t.Error("the runtime's own CORS policy was withdrawn outside the FHIR surface")
	}
}
