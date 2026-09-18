package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// The identifier namespaces. A backlog entry and a delivery are different
// things and are never mistaken for each other by a query.
const (
	writePrefix    = "wrt_"
	deliveryPrefix = "dlv_"
	workerPrefix   = "wkr_"

	// identifierBytes is how much randomness an identifier carries.
	identifierBytes = 16
)

var (
	// ErrMissingOwner reports a Subscription with nobody to deliver as. It fires
	// nothing: a notification delivered as nobody would be one authorized by
	// nothing.
	ErrMissingOwner = errors.New("subscription: no standing owns this subscription")

	// ErrUnclaimable reports a claim naming no worker, or no batch to take.
	// Claiming as nobody is not claiming: every other process would answer to
	// that name too, which is the thing a claim exists to stop.
	ErrUnclaimable = errors.New("subscription: a claim names one worker and a batch to take")

	// ErrNobodyListening reports a notification with nobody there to take it.
	//
	// It is not a failure to retry. R4's websocket channel tells whoever is
	// connected now: a socket is not a queue, and holding a notification for a
	// subscriber who may simply be offline would retry at them for half an hour
	// and still not reach them. It is recorded as never delivered rather than
	// as delivered, because that is what happened.
	ErrNobodyListening = errors.New("subscription: nobody is listening for this notification")
)

// Written is one resource write, recorded before anybody has worked out who
// should hear about it.
type Written struct {
	Project storage.ProjectID
	ID      storage.LogicalID
	Key     storage.ResourceKey
	Version storage.VersionID
	At      time.Time
}

// Owner is the standing a Subscription delivers as.
type Owner struct {
	Membership project.MembershipID
	Principal  project.PrincipalRef
}

// DeliveryState is where one notification got to.
type DeliveryState string

// The states a delivery passes through.
const (
	// Pending is owed and not yet made.
	Pending DeliveryState = "pending"

	// Delivered was accepted by the subscriber.
	Delivered DeliveryState = "delivered"

	// Abandoned was tried until this server stopped trying. It is kept rather
	// than deleted: what was never delivered is the thing an operator needs to
	// see.
	Abandoned DeliveryState = "abandoned"
)

// Delivery is one thing one subscriber is owed.
//
// It carries the resource's key and not its content. The resource is read at
// delivery time under the owner's own Scope, so access withdrawn between the
// write and the notification is access the notification does not have.
type Delivery struct {
	Project      storage.ProjectID
	ID           storage.LogicalID
	Subscription storage.LogicalID
	Key          storage.ResourceKey
	Version      storage.VersionID
	State        DeliveryState
	Attempts     int
	DueAt        time.Time
}

// Attempts is how many times one delivery is tried before this server stops,
// and how long it waits between tries.
//
// The backoff is bounded rather than exponential without end: a subscriber that
// has been down for a day is one somebody has to look at, and retrying it
// forever is a queue that never drains.
const (
	MaxAttempts = 6

	// firstBackoff is the wait after the first failure; each one after doubles
	// it, so six attempts span about half an hour.
	firstBackoff = time.Minute
)

// NextAttempt returns when a delivery that has just failed should be tried
// again, and whether it should be tried at all.
func NextAttempt(attempts int, at time.Time) (time.Time, bool) {
	if attempts >= MaxAttempts {
		return time.Time{}, false
	}

	wait := firstBackoff
	for range attempts - 1 {
		wait *= 2
	}

	return at.Add(wait), true
}

// MintWriteID draws an identifier for one recorded write.
func MintWriteID(random io.Reader) (storage.LogicalID, error) {
	return mintID(random, writePrefix)
}

// MintWorkerID draws an identifier for one worker, so the rows it claims are
// claimed by something nameable. It is drawn per process rather than configured:
// what a claim has to be is distinct, and nothing else depends on which one it
// was.
func MintWorkerID(random io.Reader) (WorkerID, error) {
	id, err := mintID(random, workerPrefix)

	return WorkerID(id), err
}

// WorkerID names one worker working through a queue.
type WorkerID string

// DeliveryIDFor names the delivery one write owes one subscription.
//
// It is derived rather than drawn, which is what makes fanning a write out
// twice enqueue nothing the second time. A worker that died between enqueueing
// a delivery and settling the write leaves that write to be fanned out again,
// and a drawn identifier would make every subscriber owed twice for it.
//
// The parts are separated, so no two triples can be spelled into one name.
func DeliveryIDFor(
	owner storage.ProjectID, written storage.LogicalID, watched storage.LogicalID,
) storage.LogicalID {
	digest := sha256.Sum256([]byte(
		string(owner) + "\x00" + string(written) + "\x00" + string(watched)))

	return storage.LogicalID(deliveryPrefix +
		base64.RawURLEncoding.EncodeToString(digest[:identifierBytes]))
}

func mintID(random io.Reader, prefix string) (storage.LogicalID, error) {
	raw := make([]byte, identifierBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("subscription: cannot mint a %q identifier: %w", prefix, err)
	}

	return storage.LogicalID(prefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

// Watcher is one stored Subscription, as the content it was written with.
type Watcher struct {
	ID      storage.LogicalID
	Content []byte
}

// Claim is one worker taking a batch of rows for itself.
//
// Several replicas read the same queue, so without a claim each one would fan
// the same write out and post the same notification. Until is the lease: a
// worker that died leaves its rows claimed, and the lease is what gives them
// back rather than leaving a queue that stops draining because a process
// somewhere is gone.
type Claim struct {
	Worker WorkerID
	At     time.Time
	Until  time.Time
	Limit  int
}

// Queue is what a write records and a worker works through.
type Queue interface {
	// Watching returns the Subscriptions one Project currently holds. They are
	// read as the server's own configuration rather than under anybody's Scope:
	// what a subscription may be told is decided when it is delivered, against
	// the standing that created it.
	Watching(ctx context.Context, project storage.ProjectID) ([]Watcher, error)

	// Record notes one write. It joins whatever transaction the context
	// carries, so a write that rolls back owes nobody anything.
	Record(ctx context.Context, written Written) error

	// Backlog claims the writes nobody is fanning out, oldest first, and
	// returns what it claimed.
	Backlog(ctx context.Context, claim Claim) ([]Written, error)

	// Settle removes one backlog entry, having enqueued whatever it owed.
	Settle(ctx context.Context, project storage.ProjectID, id storage.LogicalID) error

	// Owns records who a Subscription delivers as.
	Owns(ctx context.Context, project storage.ProjectID, subscription storage.LogicalID, owner Owner) error

	// Owner returns who a Subscription delivers as, if anybody still does.
	Owner(ctx context.Context, project storage.ProjectID, subscription storage.LogicalID) (Owner, bool, error)

	// Owe enqueues one delivery, and does nothing for one already enqueued: the
	// identifier is derived from the write and the subscription, so fanning the
	// same write out twice owes nobody twice.
	Owe(ctx context.Context, delivery Delivery) error

	// Due claims the pending deliveries ready to be tried, and returns what it
	// claimed.
	Due(ctx context.Context, claim Claim) ([]Delivery, error)

	// Attempted records what became of one try, and releases the claim on it: a
	// delivery due again is due for whichever worker reaches it.
	Attempted(ctx context.Context, delivery Delivery, state DeliveryState, at time.Time) error
}
