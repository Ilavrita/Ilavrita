// Command ilavrita runs the Ilavrita healthcare server. PocketBase supplies the
// runtime; the healthcare API belongs to Ilavrita and is mounted separately.
package main

import (
	"log"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Stamped by the release pipeline through -ldflags; see the Makefile.
var (
	version  = "dev"
	revision = "unknown"
)

// startedAt dates the CapabilityStatement, which R4 requires. It is fixed for
// the life of the process so the document does not change on every request.
var startedAt = time.Now()

func main() {
	app := pocketbase.New()

	app.OnServe().BindFunc(func(serve *core.ServeEvent) error {
		// PocketBase's installer prints a live 30-minute superuser token to stdout
		// and opens a browser. A credential in the logs is not acceptable here.
		serve.InstallerFunc = nil

		if err := startServing(serve.App); err != nil {
			return err
		}

		registerRuntimeBoundary(serve.Router)
		registerOperationalRoutes(serve.Router)
		registerAuthRoutes(serve.Router)
		registerControlRoutes(serve.Router)
		registerFHIRRoutes(serve.Router)

		return serve.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
