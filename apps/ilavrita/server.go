package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/search"
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
	resources    *sqlite.ResourceStore
	users        *sqlite.UserStore
	projects     *sqlite.ProjectStore
	sessions     sessionResolver
	memberships  *sqlite.MembershipStore
	applications *sqlite.ClientApplicationStore
	resolvers    authz.Resolvers

	// payloads holds the bytes a Binary describes. A backend wired without one
	// serves no payload, which is what a deployment with nowhere to put them
	// can honestly offer.
	payloads files.Store

	// factors holds the second factor an identity proved. A backend wired
	// without one requires none, which is what a deployment that configured no
	// sealing key can honestly offer.
	factors *sqlite.FactorStore

	// now is what this server reads the time from. It is a field so a test can
	// move it: a second factor turns on a thirty-second step, and a suite that
	// had to wait one out would either be slow or be testing something else.
	now func() time.Time

	// audits records what happened. A backend wired without one records
	// nothing, which is a wiring mistake rather than a decision — so the
	// decorator asks before it opens a transaction, and a process serving
	// without it is one that cannot answer an incident.
	audits audit.Recorder

	// attempts throttles the login route. It is per process, so it holds only
	// what this instance has seen.
	attempts *attemptLimiter
}

// sessionResolver is the whole of what this server does with sessions: issue one
// when a credential is proved, turn a presented token back into it, and destroy
// it. It is an interface so a test can decide what a token means without the
// process holding any other way to name a principal.
type sessionResolver interface {
	Issue(ctx context.Context, session project.Session) error
	Resolve(ctx context.Context, token project.SessionToken, now time.Time) (project.Session, bool, error)
	Revoke(ctx context.Context, proj project.ID, id project.SessionID, at time.Time) error
}

// access is what one authorized interaction may do: the storage it reads and
// writes, the commit boundary it writes within, and the Scope and Project it
// was decided for. They travel together, so no route holds one without them.
type access struct {
	Resources    storage.ResourceRepository
	Payloads     files.Store
	Searches     search.Repository
	Versions     storage.VersionStore
	Transactions storage.Transactor
	Scope        storage.Scope
	Project      storage.ProjectID
}

// clock is the time this server reads, falling back to the real one so weak
// wiring cannot substitute a clock that never advances.
func (b *backend) clock() time.Time {
	if b == nil || b.now == nil {
		return time.Now().UTC()
	}

	return b.now().UTC()
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
	published, err := configuredAddress()
	if err != nil {
		return err
	}

	publishing = published

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

	serving = newBackend(db.DB(), app.DataDir())

	return nil
}

// payloadDirectory is where a Binary's bytes live, beside the database rather
// than inside it: a row carrying megabytes makes every read of the metadata pay
// for them and every backup of the database carry them.
const payloadDirectory = "payloads"

// newBackend binds one database to every port. Built once, because a store built
// per request would open a second pool on every call.
func newBackend(db *sql.DB, dataDir string) *backend {
	return &backend{
		resources:    sqlite.NewResourceStore(db),
		users:        sqlite.NewUserStore(db),
		projects:     sqlite.NewProjectStore(db),
		sessions:     sqlite.NewSessionStore(db),
		memberships:  sqlite.NewMembershipStore(db),
		applications: sqlite.NewClientApplicationStore(db),
		audits:       sqlite.NewAuditStore(db),
		factors:      sqlite.NewFactorStore(db, sealingKey()),
		payloads:     files.NewDisk(filepath.Join(dataDir, payloadDirectory)),
		attempts:     newAttemptLimiter(sqlite.NewAttemptStore(db), nil),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	}
}

// A by-key interaction still reaches no other Project: authorizationRequest
// names no grantor, and BuildScope consults no link nobody opted into. The real
// resolver is wired here so the capability exists for the routes that will.
