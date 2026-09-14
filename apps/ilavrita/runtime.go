package main

import (
	"os"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// PocketBase serves its own REST API and admin console alongside whatever the
// host application registers. Ilavrita does not publish those as product
// surface (FR-029), and leaving them reachable also exposes the file and
// thumbnail endpoints, which is the only path by which CVE-2023-36308 in the
// image decoder can be reached.
//
// They stay available for development behind an explicit opt-in, because
// working on the storage backend needs them.
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
