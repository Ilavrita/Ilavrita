package main

import (
	"os"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// PocketBase serves its own API and admin console alongside our routes. Those are
// not product surface (FR-029) and reach the image decoder behind CVE-2023-36308,
// so they are off unless explicitly opted into for development.
const exposeRuntimeVariable = "ILAVRITA_EXPOSE_POCKETBASE"

var runtimePrefixes = []string{"/api/", "/_/"}

func registerRuntimeBoundary(routes *router.Router[*core.RequestEvent]) {
	if runtimeSurfaceEnabled() {
		return
	}

	routes.BindFunc(func(request *core.RequestEvent) error {
		if servesRuntimeSurface(request.Request.URL.Path) {
			return request.NotFoundError("", nil)
		}

		return request.Next()
	})
}

func runtimeSurfaceEnabled() bool {
	return strings.EqualFold(os.Getenv(exposeRuntimeVariable), "true")
}

func servesRuntimeSurface(path string) bool {
	for _, prefix := range runtimePrefixes {
		if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}
