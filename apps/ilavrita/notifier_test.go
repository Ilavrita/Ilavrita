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

	// deliveries is what each notification named itself, so a test can ask
	// whether a repeat is recognisable as one.
	deliveries []storage.LogicalID
}

func (r *told) Deliver(
	_ context.Context,
	delivery subscription.Delivery,
	_ subscription.Subscription,
	record storage.ResourceRecord,
) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.err != nil {
		return r.err
	}

	r.sent = append(r.sent, record.Key)
	r.deliveries = append(r.deliveries, delivery.ID)

	return nil
}

// named returns the identifier each notification carried, which is what a
// subscriber recognises a repeat by.
func (r *told) named() []storage.LogicalID {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]storage.LogicalID(nil), r.deliveries...)
}

func (r *told) delivered() []storage.ResourceKey {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]storage.ResourceKey(nil), r.sent...)
}

// testNotifierWorker is who these tests claim queue rows as. A claim names one
// worker, so a notifier built without one claims nothing.
const testNotifierWorker = subscription.WorkerID("wkr_tests")

// watchingServer wires a server whose notifier a test drives by hand, so what
// is asserted is one pass rather than a race with a ticker.
func watchingServer(t *testing.T) (http.Handler, *sql.DB, *told, *notifier) {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, everyAction)

	deliveries := &told{}

	return fhirRoutes(t), db, deliveries, &notifier{
		worker:   testNotifierWorker,
		queue:    sqlite.NewSubscriptionStore(db),
		searches: sqlite.NewResourceStore(db),
		resolve:  serving.resolvers,
		channels: map[subscription.Channel]deliverer{subscription.ChannelRestHook: deliveries},
	}
}

// subscribe creates one Subscription and returns its id.
func subscribe(t *testing.T, routes http.Handler, criteria string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription",
		body: valid("Subscription", map[string]string{
			"criteria": `"` + criteria + `"`,
			"status":   `"active"`,
			"channel":  `{"type":"rest-hook","endpoint":"https://example.test/hook"}`,
		}),
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// observe writes one Observation and returns its id.
func observe(t *testing.T, routes http.Handler, status string) string {
	t.Helper()

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Observation",
		body: valid("Observation", map[string]string{
			"status":  `"` + status + `"`,
			"subject": `{"reference":"Patient/` + string(conformancePatient) + `"}`,
		}),
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
		worker: testNotifierWorker,
		queue:  queue, searches: sqlite.NewResourceStore(db),
		resolve:  serving.resolvers,
		channels: map[subscription.Channel]deliverer{subscription.ChannelRestHook: deliveries},
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
		body: valid("Subscription", map[string]string{
			"criteria": `"Observation?status=final"`, "status": `"requested"`,
			"channel": `{"type":"rest-hook","endpoint":"https://example.test/hook"}`,
		}),
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
		body: valid("Subscription", map[string]string{
			"criteria": `"Observation?status=final"`, "status": `"off"`,
			"channel": `{"type":"rest-hook","endpoint":"https://example.test/hook"}`,
		}),
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

// aSecondWorker is another replica reading the same queue.
func aSecondWorker(t *testing.T, first *notifier) *notifier {
	t.Helper()

	second := *first
	second.worker = subscription.WorkerID("wkr_the_other_replica")

	return &second
}

// TestTwoWorkersDoNotFanOutTheSameWrite. Every replica reads one queue, so
// without a claim each of them would work through every entry — and a write
// fanned out twice is every subscriber owed twice for it.
func TestTwoWorkersDoNotFanOutTheSameWrite(t *testing.T) {
	routes, db, _, worker := watchingServer(t)

	subscribeOver(t, routes, "Observation?status=final", "rest-hook", "")
	observe(t, routes, "final")

	at := worker.clock()

	claim := func(held *notifier) int {
		taken, err := held.queue.Backlog(context.Background(), subscription.Claim{
			Worker: held.worker, At: at, Until: at.Add(backlogLease), Limit: backlogBatch,
		})
		if err != nil {
			t.Fatalf("claim the backlog: %v", err)
		}

		return len(taken)
	}

	// Two writes: the Subscription is a resource like any other, and the
	// Observation is what it watches.
	if first, second := claim(worker), claim(aSecondWorker(t, worker)); first != 2 || second != 0 {
		t.Errorf("one replica claimed %d entries and the other %d, want both and none", first, second)
	}

	// And they are still there to be worked through: claiming is holding, not
	// taking away.
	assertBacklogCount(t, db, 2)
}

// TestAClaimIsGivenBackWhenItsWorkerDoesNot. The lease is the whole answer to a
// replica that died mid-pass: without it the rows it claimed would be claimed
// forever and the queue would stop draining.
func TestAClaimIsGivenBackWhenItsWorkerDoesNot(t *testing.T) {
	routes, _, _, worker := watchingServer(t)

	subscribeOver(t, routes, "Observation?status=final", "rest-hook", "")
	observe(t, routes, "final")

	at := worker.clock()

	taken, err := worker.queue.Backlog(context.Background(), subscription.Claim{
		Worker: worker.worker, At: at, Until: at.Add(backlogLease), Limit: backlogBatch,
	})
	if err != nil || len(taken) == 0 {
		t.Fatalf("claimed %d entries: %v", len(taken), err)
	}

	// That worker is gone. Nobody else may have the row until its lease runs
	// out, and then anybody may.
	other := aSecondWorker(t, worker)

	for _, held := range []struct {
		described string
		at        time.Time
		want      int
	}{
		{"while the lease holds", at.Add(backlogLease - time.Second), 0},
		{"once it has run out", at.Add(backlogLease + time.Second), len(taken)},
	} {
		again, err := other.queue.Backlog(context.Background(), subscription.Claim{
			Worker: other.worker, At: held.at, Until: held.at.Add(backlogLease), Limit: backlogBatch,
		})
		if err != nil {
			t.Fatalf("re-claim %s: %v", held.described, err)
		}

		if len(again) != held.want {
			t.Errorf("%s another worker claimed %d entries, want %d",
				held.described, len(again), held.want)
		}
	}
}

// TestFanningTheSameWriteOutTwiceOwesNobodyTwice. A claim keeps two live
// workers apart; this is what covers the one that died having enqueued its
// deliveries and not yet settled the write. The delivery's identifier is
// derived from the write and the subscription, so the second fan-out finds them
// already there.
func TestFanningTheSameWriteOutTwiceOwesNobodyTwice(t *testing.T) {
	routes, db, _, worker := watchingServer(t)

	subscribeOver(t, routes, "Observation?status=final", "rest-hook", "")
	observe(t, routes, "final")

	backlog := backlogEntryFor(t, db, "Observation")

	for attempt := range 3 {
		if err := worker.owed(context.Background(), backlog); err != nil {
			t.Fatalf("fan out %d: %v", attempt, err)
		}
	}

	if held := deliveryStates(t, db); len(held) != 1 {
		t.Errorf("three fan-outs of one write owe %d deliveries, want one", len(held))
	}
}

// TestTwoWorkersDoNotMakeTheSameDelivery, which is the sharper half: a
// subscriber posted to twice for one write is the thing an endpoint sees.
//
// Both claim before either records an outcome, because that is the race. A
// replica that waited for the first to finish would be kept away by the state
// alone, which is not what a claim is for.
func TestTwoWorkersDoNotMakeTheSameDelivery(t *testing.T) {
	routes, _, _, worker := watchingServer(t)

	subscribeOver(t, routes, "Observation?status=final", "rest-hook", "")
	observe(t, routes, "final")

	if err := worker.fanOut(t.Context()); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	at := worker.clock()

	claim := func(held *notifier) int {
		taken, err := held.queue.Due(t.Context(), subscription.Claim{
			Worker: held.worker, At: at, Until: at.Add(deliveryLease), Limit: deliveryBatch,
		})
		if err != nil {
			t.Fatalf("claim what is due: %v", err)
		}

		return len(taken)
	}

	if first, second := claim(worker), claim(aSecondWorker(t, worker)); first != 1 || second != 0 {
		t.Errorf("one replica claimed %d deliveries and the other %d, want one and none",
			first, second)
	}
}

// TestAFailedDeliveryIsDueAgainWhenItIsDue, and not when its claim runs out.
// The lease outlasts the first backoff by design — it has to cover a whole batch
// of subscribers timing out — so a claim left in place would hold a retry back
// for the difference.
func TestAFailedDeliveryIsDueAgainWhenItIsDue(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}
	worker.now = clock.now
	deliveries.err = errDeliveryRefused

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Fatalf("a refused delivery counted as %d sent", len(sent))
	}

	// Due again one backoff later, which is well inside the lease the first
	// attempt took out.
	deliveries.err = nil
	clock.advance(2 * time.Minute)

	if 2*time.Minute >= deliveryLease {
		t.Fatal("the lease no longer outlasts the first backoff, so this proves nothing")
	}

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 1 {
		t.Errorf("the retry was made %d times, want once", len(sent))
	}
}

// assertBacklogCount reports how many writes are still waiting to be fanned out.
func assertBacklogCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()

	var held int
	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM subscription_backlog").Scan(&held); err != nil {
		t.Fatalf("count the backlog: %v", err)
	}

	if held != want {
		t.Errorf("%d backlog entries, want %d", held, want)
	}
}

// backlogEntryFor reads the write of one type waiting to be fanned out.
func backlogEntryFor(t *testing.T, db *sql.DB, of string) subscription.Written {
	t.Helper()

	var owner, id, resourceType, resourceID, version string

	if err := db.QueryRowContext(context.Background(),
		"SELECT project_id, id, res_type, res_id, version_id FROM subscription_backlog"+
			" WHERE res_type = ?", of).
		Scan(&owner, &id, &resourceType, &resourceID, &version); err != nil {
		t.Fatalf("read the backlog: %v", err)
	}

	return subscription.Written{
		Project: storage.ProjectID(owner),
		ID:      storage.LogicalID(id),
		Key: storage.ResourceKey{
			Project: storage.ProjectID(owner),
			Type:    storage.ResourceType(resourceType),
			ID:      storage.LogicalID(resourceID),
		},
		Version: storage.VersionID(version),
	}
}

// TestARepeatedNotificationNamesItselfTheSameWay. A claim keeps two live
// workers apart and a derived identifier keeps a re-run fan-out from owing
// anybody twice, but delivery itself stays at-least-once: a subscriber that
// acted and then failed to answer is posted to again. What makes that
// survivable is that the second notification is recognisable as the same one.
func TestARepeatedNotificationNamesItselfTheSameWay(t *testing.T) {
	routes, db, deliveries, worker := watchingServer(t)

	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}
	worker.now = clock.now

	// The endpoint takes the notification and then answers badly, which is the
	// case a repeat exists for: it acted, and this server cannot know that.
	deliveries.err = errDeliveryRefused

	subscribe(t, routes, "Observation?status=final")
	observe(t, routes, "final")

	worker.pass(context.Background())

	deliveries.err = nil
	clock.advance(2 * time.Minute)
	worker.pass(context.Background())

	named := deliveries.named()
	if len(named) != 1 {
		t.Fatalf("the endpoint was posted to %d times after one refusal", len(named))
	}

	// One row, so the retry was the same delivery rather than a second one —
	// and the identifier it carried is that row's.
	if held := deliveryStates(t, db); len(held) != 1 {
		t.Fatalf("%d deliveries, want one", len(held))
	}

	if named[0] != deliveryIDIn(t, db) {
		t.Errorf("the notification named itself %q and the row is %q", named[0], deliveryIDIn(t, db))
	}
}

// deliveryIDIn reads the one delivery's identifier.
func deliveryIDIn(t *testing.T, db *sql.DB) storage.LogicalID {
	t.Helper()

	var id string
	if err := db.QueryRowContext(context.Background(),
		"SELECT id FROM subscription_deliveries").Scan(&id); err != nil {
		t.Fatalf("read the delivery: %v", err)
	}

	return storage.LogicalID(id)
}
