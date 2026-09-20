// Command ilavrita runs the Ilavrita healthcare server. PocketBase supplies the
// runtime; the healthcare API belongs to Ilavrita and is mounted separately.
package main

import (
	"log"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/cmd"
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

	registerBackupCommands(app)

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
		registerOAuthRoutes(serve.Router)
		registerFHIRRoutes(serve.Router)

		return serve.Next()
	})

	// Start would register PocketBase's own superuser command beside serve.
	// Execute is the same thing without the system commands, so serve is added
	// here and the command that mints a superuser is never registered at all.
	// See superuser.go for why this server has no such account.
	app.RootCmd.AddCommand(cmd.NewServeCommand(app, true))

	if err := app.Execute(); err != nil {
		log.Fatal(err)
	}
}
