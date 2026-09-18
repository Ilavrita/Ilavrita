package subscription

import (
	"context"
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

	// identifierBytes is how much randomness an identifier carries.
	identifierBytes = 16
)

var (
	// ErrMissingOwner reports a Subscription with nobody to deliver as. It fires
	// nothing: a notification delivered as nobody would be one authorized by
	// nothing.
	ErrMissingOwner = errors.New("subscription: no standing owns this subscription")

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

// MintDeliveryID draws an identifier for one delivery.
func MintDeliveryID(random io.Reader) (storage.LogicalID, error) {
	return mintID(random, deliveryPrefix)
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

	// Backlog returns the writes nobody has fanned out yet, oldest first.
	Backlog(ctx context.Context, limit int) ([]Written, error)

	// Settle removes one backlog entry, having enqueued whatever it owed.
	Settle(ctx context.Context, project storage.ProjectID, id storage.LogicalID) error

	// Owns records who a Subscription delivers as.
	Owns(ctx context.Context, project storage.ProjectID, subscription storage.LogicalID, owner Owner) error

	// Owner returns who a Subscription delivers as, if anybody still does.
	Owner(ctx context.Context, project storage.ProjectID, subscription storage.LogicalID) (Owner, bool, error)

	// Owe enqueues one delivery.
	Owe(ctx context.Context, delivery Delivery) error

	// Due returns the pending deliveries ready to be tried.
	Due(ctx context.Context, at time.Time, limit int) ([]Delivery, error)

	// Attempted records what became of one try.
	Attempted(ctx context.Context, delivery Delivery, state DeliveryState, at time.Time) error
}
