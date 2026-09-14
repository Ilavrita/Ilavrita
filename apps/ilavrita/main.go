// Command ilavrita runs the Ilavrita healthcare server.
//
// The process is the Ilavrita PocketBase fork carrying the Ilavrita HTTP
// surface. PocketBase supplies the runtime; the healthcare API belongs to
// Ilavrita and is mounted separately from anything PocketBase exposes.
package main

import (
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Stamped by the release pipeline through -ldflags; see the Makefile.
var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	app := pocketbase.New()

	app.OnServe().BindFunc(func(serve *core.ServeEvent) error {
		registerOperationalRoutes(serve.Router)
		registerFHIRRoutes(serve.Router)

		return serve.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
