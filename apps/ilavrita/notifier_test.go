package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// told is a deliverer that only remembers, so a test can watch what would have
// been sent without anything leaving the process.
type told struct {
	mutex sync.Mutex
	sent  []storage.ResourceKey
	err   error
}

func (r *told) Deliver(
	_ context.Context, _ subscription.Subscription, record storage.ResourceRecord,
) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.err != nil {
		return r.err
	}

	r.sent = append(r.sent, record.Key)

	return nil
}

func (r *told) delivered() []storage.ResourceKey {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]storage.ResourceKey(nil), r.sent...)
}

// watchingServer wires a server whose notifier a test drives by hand, so what
// is asserted is one pass rather than a race with a ticker.
func watchingServer(t *testing.T) (http.Handler, *sql.DB, *told, *notifier) {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	deliveries := &told{}

	return fhirRoutes(t), db, deliveries, &notifier{
		queue:    sqlite.NewSubscriptionStore(db),
		searches: sqlite.NewResourceStore(db),
		deliver:  deliveries,
		resolve:  serving.resolvers,
	}
}

// subscribe creates one Subscription and returns its id.
func subscribe(t *testing.T, routes http.Handler, criteria string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription",
		body: `{"resourceType":"Subscription","criteria":"` + criteria +
			`","status":"active","channel":{"type":"rest-hook","endpoint":"https://example.test/hook"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// observe writes one Observation and returns its id.
func observe(t *testing.T, routes http.Handler, status string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: `{"resourceType":"Observation","status":"` + status +
			`","subject":{"reference":"Patient/` + string(conformancePatient) + `"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// TestAMatchingWriteIsDelivered.
func TestAMatchingWriteIsDelivered(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	subscribe(t, routes, "Observation?status=final")

	id := observe(t, routes, "final")

	worker.pass(context.Background())

	sent := deliveries.delivered()
	if len(sent) != 1 || string(sent[0].ID) != id {
		t.Fatalf("delivered %v, want the observation that matched", sent)
	}
}

// TestAWriteThatDoesNotMatchIsNotDelivered.
func TestAWriteThatDoesNotMatchIsNotDelivered(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	subscribe(t, routes, "Observation?status=final")

	observe(t, routes, "preliminary")

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("delivered %v for a write that does not match", sent)
	}
}

// TestASubscriptionIsToldNothingItsOwnerCouldNotRead. This is the whole of the
// authorization: matching and reading are one question, asked under the
// standing that created the subscription, so a subscription cannot be a way
// around a Scope.
func TestASubscriptionIsToldNothingItsOwnerCouldNotRead(t *testing.T) {
	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	routes := fhirRoutes(t)

	subscribe(t, routes, "Observation?status=final")

	// A resource written for another patient, which the conformance policy
	// confines this standing away from. It is written under a Scope that reaches
	// it, which is the only way one gets there.
	elsewhere := storage.ResourceKey{Project: homeProject, Type: "Observation", ID: "obs-elsewhere"}

	wide := storage.NewScope(
		storage.Grant{
			Project: homeProject, Kind: storage.KindFHIR, Type: "Observation",
			Action: storage.ActionWrite, Source: storage.SourceMembership,
		},
		storage.Grant{
			Project: homeProject, Kind: storage.KindFHIR, Type: "Observation",
			Action: storage.ActionRead, Source: storage.SourceMembership,
		},
	)

	err := sqlite.NewResourceStore(db).Create(context.Background(), wide, storage.ResourceRecord{
		Key: elsewhere,
		Content: []byte(`{"resourceType":"Observation","status":"final",` +
			`"subject":{"reference":"Patient/someone-else"}}`),
		Compartments: []storage.Compartment{{Type: "Patient", ID: "someone-else"}},
	})
	if err != nil {
		t.Fatalf("write another patient's observation: %v", err)
	}

	// And the write is recorded, as the route would have.
	queue := sqlite.NewSubscriptionStore(db)

	id, err := subscription.MintWriteID(randomReader())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := queue.Record(context.Background(), subscription.Written{
		Project: homeProject, ID: id, Key: elsewhere, Version: "1", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record the write: %v", err)
	}

	deliveries := &told{}
	worker := &notifier{
		queue: queue, searches: sqlite.NewResourceStore(db),
		deliver: deliveries, resolve: serving.resolvers,
	}

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a subscriber was told about %v, which its own Scope cannot read", sent)
	}
}

// TestASubscriptionNobodyTurnedOnDeliversNothing.
func TestASubscriptionNobodyTurnedOnDeliversNothing(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription",
		body: `{"resourceType":"Subscription","criteria":"Observation?status=final",` +
			`"status":"requested","channel":{"type":"rest-hook","endpoint":"https://example.test/hook"}}`,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	observe(t, routes, "final")
	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a requested subscription delivered %v", sent)
	}
}

// TestADeletedSubscriptionDeliversNothing, which is how one is switched off for
// good.
func TestADeletedSubscriptionDeliversNothing(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	id := subscribe(t, routes, "Observation?status=final")

	assertStatus(t, call{
		method: http.MethodDelete, path: resourcePath("Subscription", id),
	}.send(t, routes), http.StatusNoContent)

	observe(t, routes, "final")
	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a deleted subscription delivered %v", sent)
	}
}

// TestTheBacklogIsWorkedThroughOnce, so a pass does not deliver the same write
// again on the next one.
func TestTheBacklogIsWorkedThroughOnce(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	worker.pass(context.Background())
	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 1 {
		t.Errorf("delivered %d times, want once", len(sent))
	}

	var backlog int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM subscription_backlog").Scan(&backlog); err != nil {
		t.Fatalf("count the backlog: %v", err)
	}

	if backlog != 0 {
		t.Errorf("%d backlog entries were left behind", backlog)
	}
}

// TestAFailedDeliveryIsTriedAgainAndThenGivenUpOn. Retrying forever is a queue
// that never drains; giving up at once is a notification lost to one bad
// moment.
func TestAFailedDeliveryIsTriedAgainAndThenGivenUpOn(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}
	worker.now = clock.now

	deliveries.err = errDeliveryRefused

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	for range subscription.MaxAttempts {
		worker.pass(context.Background())
		clock.advance(time.Hour)
	}

	var state string

	var attempts int

	if err := db.QueryRowContext(context.Background(),
		"SELECT state, attempts FROM subscription_deliveries").Scan(&state, &attempts); err != nil {
		t.Fatalf("read the delivery: %v", err)
	}

	if state != string(subscription.Abandoned) {
		t.Errorf("after %d attempts the delivery is %q", attempts, state)
	}

	if attempts != subscription.MaxAttempts {
		t.Errorf("it was tried %d times, want %d", attempts, subscription.MaxAttempts)
	}
}

// TestADeliveryThatSucceedsIsNotTriedAgain.
func TestADeliveryThatSucceedsIsNotTriedAgain(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	worker.pass(context.Background())
	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 1 {
		t.Errorf("delivered %d times", len(sent))
	}

	var state string
	if err := db.QueryRowContext(context.Background(),
		"SELECT state FROM subscription_deliveries").Scan(&state); err != nil {
		t.Fatalf("read the delivery: %v", err)
	}

	if state != string(subscription.Delivered) {
		t.Errorf("the delivery is %q", state)
	}
}

// errDeliveryRefused is what a subscriber that will not accept looks like.
var errDeliveryRefused = errors.New("the subscriber refused the notification")

// randomReader is the source identifiers are drawn from.
func randomReader() io.Reader { return rand.Reader }

// deliveryStates lists what the queue currently holds, so a test can assert
// that nothing was even enqueued rather than only that nothing was sent.
func deliveryStates(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		"SELECT state FROM subscription_deliveries ORDER BY id")
	if err != nil {
		t.Fatalf("read the deliveries: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var held []string

	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan: %v", err)
		}

		held = append(held, state)
	}

	return held
}

// TestNothingIsEnqueuedForASubscriptionNobodyTurnedOn. Abandoning it later
// would answer the same to a caller and leave a queue full of work that was
// never going anywhere.
func TestNothingIsEnqueuedForASubscriptionNobodyTurnedOn(t *testing.T) {
	routes, db, _, worker := watchingServer(t)

	assertStatus(t, call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription",
		body: `{"resourceType":"Subscription","criteria":"Observation?status=final",` +
			`"status":"off","channel":{"type":"rest-hook","endpoint":"https://example.test/hook"}}`,
	}.send(t, routes), http.StatusCreated)

	observe(t, routes, "final")
	worker.pass(context.Background())

	if held := deliveryStates(t, db); len(held) != 0 {
		t.Errorf("a subscription nobody turned on enqueued %v", held)
	}
}

// TestASubscriptionWhoseStandingWentDeliversNothing. Standing withdrawn is a
// subscription that stops, rather than one that goes on delivering as somebody
// who is no longer there.
func TestASubscriptionWhoseStandingWentDeliversNothing(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	// The membership is withdrawn, which takes the owner row with it.
	if _, err := db.ExecContext(context.Background(),
		"DELETE FROM project_memberships WHERE id = 'pm_conformance'"); err != nil {
		t.Fatalf("withdraw the standing: %v", err)
	}

	var owners int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM subscription_owners").Scan(&owners); err != nil {
		t.Fatalf("count the owners: %v", err)
	}

	if owners != 0 {
		t.Fatalf("%d owner rows outlived the standing they named", owners)
	}

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a subscription whose standing went delivered %v", sent)
	}
}

// TestOnlyTheResourceThatWasWrittenIsDelivered. The criteria is asked of one
// resource, so a Project full of matching resources does not turn one write
// into a notification about somebody else's.
func TestOnlyTheResourceThatWasWrittenIsDelivered(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	// Two resources that both match, written before anybody is watching.
	first := observe(t, routes, "final")
	second := observe(t, routes, "final")

	subscribe(t, routes, "Observation?status=final")

	// The writes above are already in the backlog, so they are cleared first.
	worker.pass(context.Background())
	deliveries.mutex.Lock()
	deliveries.sent = nil
	deliveries.mutex.Unlock()

	third := observe(t, routes, "final")

	worker.pass(context.Background())

	sent := deliveries.delivered()
	if len(sent) != 1 || string(sent[0].ID) != third {
		t.Fatalf("one write delivered %v, want only %s (of %s, %s, %s)",
			sent, third, first, second, third)
	}
}

// TestADeliveryIsCheckedAgainBeforeItIsMade. A subscription switched off
// between the write and the notification is one that must not be told: the
// queue is a plan, not a promise.
func TestADeliveryIsCheckedAgainBeforeItIsMade(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	id := subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	// Fan out, so the delivery is enqueued and not yet made.
	if err := worker.fanOut(context.Background()); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	if held := deliveryStates(t, db); len(held) != 1 {
		t.Fatalf("the fan-out enqueued %v, want one delivery", held)
	}

	assertStatus(t, call{
		method: http.MethodDelete, path: resourcePath("Subscription", id),
	}.send(t, routes), http.StatusNoContent)

	if err := worker.send(context.Background()); err != nil {
		t.Fatalf("send: %v", err)
	}

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a subscription deleted before the notification was told %v", sent)
	}

	if held := deliveryStates(t, db); len(held) != 1 || held[0] != string(subscription.Abandoned) {
		t.Errorf("the delivery is %v, want it abandoned", held)
	}
}
