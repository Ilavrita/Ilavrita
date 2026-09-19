package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
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

	// sockets holds the subscribers connected to this process. A deployment
	// running several replicas has each subscriber on one of them, which is
	// what makes this channel best-effort and rest-hook the durable one.
	sockets *hub

	// notifications is what a write owes whoever is watching. It is the
	// interface rather than the store, so a backend wired without one holds a
	// nil the write path can actually test for: a concrete nil handed to an
	// interface field is not nil, and would be called.
	notifications subscription.Queue

	// payloads holds the bytes a Binary describes. A backend wired without one
	// serves no payload, which is what a deployment with nowhere to put them
	// can honestly offer.
	payloads files.Store

	// definitions holds the FHIR specification's own definitions. They belong to
	// no Project, so they are read beside the Project's own store rather than
	// through a Scope: what a caller may read is decided before this is reached.
	definitions *sqlite.CanonicalStore

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

	// Live answers whether a session a socket bound earlier is still one this
	// server may serve as. A bound socket holds no token to present again, so
	// this is asked of the session it named.
	Live(ctx context.Context, proj project.ID, id project.SessionID, now time.Time) (bool, error)

	// RevokeEveryUserSession signs one identity out of one Project everywhere.
	// It is what a stolen token is answered with, and what a recovered second
	// factor takes with it: a session open on the phone that was lost would
	// otherwise outlive the factor.
	RevokeEveryUserSession(
		ctx context.Context, proj project.ID, user project.UserID, at time.Time,
	) (int64, error)
}

// access is what one authorized interaction may do: the storage it reads and
// writes, the commit boundary it writes within, and the Scope and Project it
// was decided for. They travel together, so no route holds one without them.
type access struct {
	Resources     storage.ResourceRepository
	Payloads      files.Store
	Notifications subscription.Queue
	Searches      search.Repository
	Versions      storage.VersionStore
	Transactions  storage.Transactor
	Scope         storage.Scope
	Project       storage.ProjectID
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

	// Before anything is answered: this install holds no PocketBase superuser.
	// A failure here stops the server, because not being able to tell whether
	// one exists is not a state to serve clinical data in.
	if err := refuseSuperusers(context.Background(), app.DB(), db.DB()); err != nil {
		_ = db.Close()

		return err
	}

	serving = newBackend(db.DB(), app.DataDir())

	if err := seedDefinitions(context.Background(), serving.definitions); err != nil {
		_ = db.Close()

		return err
	}

	// The notifier outlives every request and stops with the process. A write
	// records what it owes and returns; this is what pays it.
	working, stop := context.WithCancel(context.Background())

	app.OnTerminate().BindFunc(func(terminate *core.TerminateEvent) error {
		stop()

		return terminate.Next()
	})

	go runNotifier(working, serving.notifier())

	return nil
}

// notifier builds the worker that turns writes into notifications.
//
// Its identity is drawn per process. Two replicas working through one queue
// have to be able to tell each other's claims apart, and what a claim has to be
// is distinct — nothing depends on which worker it was.
func (b *backend) notifier() *notifier {
	worker, err := subscription.MintWorkerID(rand.Reader)
	if err != nil {
		// A process that cannot draw one would claim rows as the empty worker,
		// which every other process would answer to as well.
		report(err)
	}

	return &notifier{
		worker:   worker,
		queue:    b.notifications,
		searches: b.resources,
		channels: map[subscription.Channel]deliverer{
			subscription.ChannelRestHook:  newRestHook(),
			subscription.ChannelWebSocket: b.sockets,
		},
		resolve: b.resolvers,
	}
}

// payloadDirectory is where a Binary's bytes live, beside the database rather
// than inside it: a row carrying megabytes makes every read of the metadata pay
// for them and every backup of the database carry them.
const payloadDirectory = "payloads"

// newBackend binds one database to every port. Built once, because a store built
// per request would open a second pool on every call.
func newBackend(db *sql.DB, dataDir string) *backend {
	return &backend{
		resources:     sqlite.NewResourceStore(db),
		users:         sqlite.NewUserStore(db),
		projects:      sqlite.NewProjectStore(db),
		sessions:      sqlite.NewSessionStore(db),
		memberships:   sqlite.NewMembershipStore(db),
		applications:  sqlite.NewClientApplicationStore(db),
		audits:        sqlite.NewAuditStore(db),
		factors:       sqlite.NewFactorStore(db, sealingKey()),
		definitions:   sqlite.NewCanonicalStore(db),
		payloads:      files.NewDisk(filepath.Join(dataDir, payloadDirectory)),
		notifications: sqlite.NewSubscriptionStore(db),
		sockets:       newHub(),
		attempts:      newAttemptLimiter(sqlite.NewAttemptStore(db), attemptKeys(), nil),
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

// seedDefinitions brings the FHIR base definitions up to what this build
// embeds, before the first request.
//
// A failure here is fatal, like every other failure at startup. A server that
// could not seed them would answer 404 for StructureDefinition/Observation,
// which is a wrong answer rather than a missing feature — and its
// CapabilityStatement would still say it serves the type.
//
// It is idempotent, and cheap when there is nothing to do: the digest of what
// this build embeds is compared with what the install last seeded, so an
// ordinary start reads one row instead of parsing the specification.
func seedDefinitions(ctx context.Context, store *sqlite.CanonicalStore) error {
	seeded, changed, err := store.Seed(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("ilavrita: seed the FHIR definitions: %w", err)
	}

	if changed {
		log.Printf("seeded %d FHIR %s definitions", seeded.Held, seeded.Release)
	}

	return nil
}
