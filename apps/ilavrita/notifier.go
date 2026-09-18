package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// How much one pass of the notifier takes on. Bounded so a backlog that built
// up while this server was down is worked through in steps rather than in one
// transaction nobody can interrupt.
const (
	backlogBatch  = 50
	deliveryBatch = 50
)

// notifier turns writes into the notifications they owe.
//
// It runs outside every request. Matching inside a write would make each write
// cost as much as the subscription list is long, and would hold the transaction
// open while it did.
type notifier struct {
	queue    subscription.Queue
	searches search.Repository
	deliver  deliverer
	resolve  authz.Resolvers
	now      func() time.Time
}

// deliverer is what actually reaches a subscriber. It is an interface so the
// channels are separable and so a test can watch what would have been sent
// without anything leaving the process.
type deliverer interface {
	// Deliver tells one subscriber about one resource. A nil error is a
	// delivery the subscriber accepted; anything else is tried again.
	Deliver(ctx context.Context, held subscription.Subscription, record storage.ResourceRecord) error
}

// clock is the time the notifier reads, falling back to the real one.
func (n *notifier) clock() time.Time {
	if n.now == nil {
		return time.Now().UTC()
	}

	return n.now().UTC()
}

// fanOut works through the backlog, turning each recorded write into the
// deliveries it owes.
//
// A write owes a subscriber when that subscriber's own Scope, running their own
// criteria, finds the resource. Matching and authorization are therefore one
// question rather than two that could disagree: a subscription cannot be told
// about something its owner could not have read by asking.
func (n *notifier) fanOut(ctx context.Context) error {
	backlog, err := n.queue.Backlog(ctx, backlogBatch)
	if err != nil {
		return err
	}

	for _, written := range backlog {
		if err := n.owed(ctx, written); err != nil {
			return err
		}

		if err := n.queue.Settle(ctx, written.Project, written.ID); err != nil {
			return err
		}
	}

	return nil
}

// owed enqueues what one write owes.
func (n *notifier) owed(ctx context.Context, written subscription.Written) error {
	watchers, err := n.queue.Watching(ctx, written.Project)
	if err != nil {
		return err
	}

	for _, watcher := range watchers {
		held, err := subscription.Read(watcher.ID, watcher.Content)
		if err != nil {
			// Written through a route that checks it, so this is a row somebody
			// changed underneath us. It is reported and skipped rather than
			// failing the whole backlog on one bad subscription.
			report(fmt.Errorf("subscription %s in %s is unreadable: %w", watcher.ID, written.Project, err))

			continue
		}

		if !held.Delivers() || held.Watching() != written.Key.Type {
			continue
		}

		record, matched, err := n.matches(ctx, written, held)
		if err != nil {
			return err
		}

		if !matched {
			continue
		}

		if err := n.owe(ctx, written.Project, held, record); err != nil {
			return err
		}
	}

	return nil
}

// matches asks whether this subscriber's own criteria, under this subscriber's
// own Scope, finds the resource that was written.
//
// The record it returns is the one the search found, which is the resource as
// it stands now rather than as it stood when the write was recorded. A
// notification says "this matches your criteria", and the only version that can
// honestly be said of is the one that was looked at.
func (n *notifier) matches(
	ctx context.Context, written subscription.Written, held subscription.Subscription,
) (storage.ResourceRecord, bool, error) {
	owner, owned, err := n.queue.Owner(ctx, written.Project, held.ID())
	if err != nil {
		return storage.ResourceRecord{}, false, err
	}

	// Standing withdrawn is a subscription that stops delivering, rather than
	// one that goes on delivering as somebody who is no longer there.
	//
	// What makes that safe is that a principal nobody holds builds no Scope, so
	// the search below would find nothing anyway. This is here so the answer is
	// a clean no rather than an error nobody asked for.
	if !owned {
		return storage.ResourceRecord{}, false, nil
	}

	scope, err := authz.BuildScope(ctx, authz.Request{
		Principal: owner.Principal,
		Project:   written.Project,
		Kind:      storage.KindFHIR,
		Type:      written.Key.Type,
		Action:    storage.ActionSearch,
		Now:       n.clock(),
		Resolvers: n.resolve,
	})
	if err != nil {
		return storage.ResourceRecord{}, false, err
	}

	plan, err := narrowedToOne(held, written.Key.ID)
	if err != nil {
		return storage.ResourceRecord{}, false, err
	}

	page, err := n.searches.Search(ctx, scope, plan)
	if err != nil {
		return storage.ResourceRecord{}, false, err
	}

	if len(page.Records) == 0 {
		return storage.ResourceRecord{}, false, nil
	}

	return page.Records[0], true, nil
}

// narrowedToOne is the subscription's criteria asked of one resource. It is
// built by re-reading the criteria rather than by editing a parsed one, so what
// the match runs is what a search of the same string would run.
func narrowedToOne(held subscription.Subscription, id storage.LogicalID) (search.Query, error) {
	_, stated, _ := cutCriteria(held.Stated())

	values, err := url.ParseQuery(stated)
	if err != nil {
		return search.Query{}, fmt.Errorf("%w: %w", subscription.ErrMalformedCriteria, err)
	}

	values.Set("_id", string(id))

	return search.Parse(held.Watching(), values)
}

// cutCriteria splits "Type?query" the way the subscription itself reads it.
func cutCriteria(stated string) (string, string, bool) {
	for index := range stated {
		if stated[index] == '?' {
			return stated[:index], stated[index+1:], true
		}
	}

	return stated, "", false
}

// owe enqueues one delivery.
func (n *notifier) owe(
	ctx context.Context,
	owner storage.ProjectID,
	held subscription.Subscription,
	record storage.ResourceRecord,
) error {
	id, err := subscription.MintDeliveryID(rand.Reader)
	if err != nil {
		return err
	}

	return n.queue.Owe(ctx, subscription.Delivery{
		Project: owner, ID: id, Subscription: held.ID(),
		Key: record.Key, Version: record.Version, DueAt: n.clock(),
	})
}

// send works through the deliveries that are due.
//
// The resource is read again here, under the subscriber's own Scope: access
// withdrawn between the write and the notification is access the notification
// does not have. A queue that carried the body would deliver what the
// subscriber could no longer read.
func (n *notifier) send(ctx context.Context) error {
	due, err := n.queue.Due(ctx, n.clock(), deliveryBatch)
	if err != nil {
		return err
	}

	for _, delivery := range due {
		if err := n.attempt(ctx, delivery); err != nil {
			return err
		}
	}

	return nil
}

// attempt makes one delivery and records what became of it.
func (n *notifier) attempt(ctx context.Context, delivery subscription.Delivery) error {
	at := n.clock()
	delivery.Attempts++

	held, record, deliverable, err := n.deliverable(ctx, delivery)
	if err != nil {
		return err
	}

	// A subscription that has gone, been switched off, or whose owner can no
	// longer read the resource owes nothing. It is abandoned rather than
	// retried: nothing about it will change by trying again.
	if !deliverable {
		return n.queue.Attempted(ctx, delivery, subscription.Abandoned, at)
	}

	if err := n.deliver.Deliver(ctx, held, record); err == nil {
		return n.queue.Attempted(ctx, delivery, subscription.Delivered, at)
	} else {
		report(fmt.Errorf("delivery %s to subscription %s failed: %w",
			delivery.ID, delivery.Subscription, err))
	}

	next, again := subscription.NextAttempt(delivery.Attempts, at)
	if !again {
		return n.queue.Attempted(ctx, delivery, subscription.Abandoned, at)
	}

	delivery.DueAt = next

	return n.queue.Attempted(ctx, delivery, subscription.Pending, at)
}

// deliverable re-reads everything a delivery rests on: that the subscription is
// still there and still on, and that its owner can still read the resource.
func (n *notifier) deliverable(
	ctx context.Context, delivery subscription.Delivery,
) (subscription.Subscription, storage.ResourceRecord, bool, error) {
	watchers, err := n.queue.Watching(ctx, delivery.Project)
	if err != nil {
		return subscription.Subscription{}, storage.ResourceRecord{}, false, err
	}

	var held subscription.Subscription

	for _, watcher := range watchers {
		if watcher.ID != delivery.Subscription {
			continue
		}

		held, err = subscription.Read(watcher.ID, watcher.Content)
		if err != nil {
			return subscription.Subscription{}, storage.ResourceRecord{}, false, nil
		}
	}

	if held.ID() == "" || !held.Delivers() {
		return subscription.Subscription{}, storage.ResourceRecord{}, false, nil
	}

	record, matched, err := n.matches(ctx, subscription.Written{
		Project: delivery.Project, Key: delivery.Key, Version: delivery.Version,
	}, held)
	if err != nil {
		return subscription.Subscription{}, storage.ResourceRecord{}, false, err
	}

	return held, record, matched, nil
}
