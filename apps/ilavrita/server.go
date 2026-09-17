package main

import (
	"context"
	"database/sql"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// serving is the wiring every FHIR route reads. Startup builds it before the
// router answers anything, and one that was never built authorizes nothing
// rather than reaching a database nobody opened.
var serving *backend

// backend is everything a FHIR route needs: the stores, the ports one
// authorization decision reads, and whatever names the caller.
type backend struct {
	resources            *sqlite.ResourceStore
	users                *sqlite.UserStore
	resolvers            authz.Resolvers
	developmentPrincipal *caller
}

// access is what one authorized interaction may do: the storage it reads and
// writes, the commit boundary it writes within, and the Scope and Project it
// was decided for. They travel together, so no route holds one without them.
type access struct {
	Resources    storage.ResourceRepository
	Versions     storage.VersionStore
	Transactions storage.Transactor
	Scope        storage.Scope
	Project      storage.ProjectID
}

// decision names the one triple a Scope answers. A Scope built to read Patient
// authorizes nothing else, which is what keeps one from widening downstream.
type decision struct {
	Kind   storage.Kind
	Type   storage.ResourceType
	Action storage.Action
}

// startServing builds the wiring once, before the first request. Every failure
// here is fatal: a server that cannot reach its own database must not answer
// requests it would answer wrongly.
func startServing(app core.App) error {
	developmentPrincipal, err := configuredDevelopmentPrincipal()
	if err != nil {
		return err
	}

	publishing, err = configuredAddress()
	if err != nil {
		return err
	}

	db, err := openDatabase(app.DataDir())
	if err != nil {
		return err
	}

	if err := prepareDatabase(context.Background(), db.DB()); err != nil {
		_ = db.Close()

		return err
	}

	app.OnTerminate().BindFunc(func(terminate *core.TerminateEvent) error {
		_ = db.Close()

		return terminate.Next()
	})

	serving = newBackend(db.DB(), developmentPrincipal)
	warnAboutDevelopmentPrincipal(developmentPrincipal)

	return nil
}

// newBackend binds one database to every port. Built once, because a store built
// per request would open a second pool on every call.
func newBackend(db *sql.DB, developmentPrincipal *caller) *backend {
	return &backend{
		resources: sqlite.NewResourceStore(db),
		users:     sqlite.NewUserStore(db),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       noProjectLinks{},
		},
		developmentPrincipal: developmentPrincipal,
	}
}

// noProjectLinks reaches no other Project. A by-key interaction carries no way
// to name a grantor Project, so a link could only widen a Scope here; the real
// resolver belongs to the search route that can name one.
type noProjectLinks struct{}

var _ project.LinkResolver = noProjectLinks{}

// Inbound answers with no link at all.
func (noProjectLinks) Inbound(context.Context, project.ID) ([]project.Link, error) {
	return nil, nil
}
