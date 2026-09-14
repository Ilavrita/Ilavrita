package main

import "testing"

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
