package main

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// subscriptionType is the resource whose write records who it delivers as.
const subscriptionType storage.ResourceType = "Subscription"

// errSubscriptionsUnavailable reports a deployment with nowhere to keep what a
// write owes. A Subscription accepted into it would deliver nothing and say so
// to nobody, so it is refused instead.
var errSubscriptionsUnavailable = errors.New("ilavrita: this deployment cannot hold subscriptions")

// checkSubscription refuses a Subscription this server could not honour, before
// it is written.
//
// Everything about it is checked here rather than when it would first fire. A
// subscription accepted and then silently never matching is worse than one
// refused: somebody is relying on it, and nothing will tell them.
func checkSubscription(custom search.Custom, key storage.ResourceKey, content []byte) error {
	if key.Type != subscriptionType {
		return nil
	}

	_, err := subscription.Read(custom, key.ID, content)

	return err
}

// afterWrite is everything a write owes once the row exists: a Binary's bytes,
// a Subscription's owner, and the note that something happened.
//
// All of it runs inside the transaction that wrote the row. A notification owed
// for a write that rolled back is a notification about something that never
// happened, and a Subscription whose owner was not recorded is one that would
// deliver as nobody.
func (g granted) afterWrite(
	key storage.ResourceKey, payload carried,
) func(context.Context, storage.ResourceRecord) error {
	return func(ctx context.Context, record storage.ResourceRecord) error {
		if store := g.storing(key, payload); store != nil {
			if err := store(ctx, record); err != nil {
				return err
			}
		}

		if key.Type == subscriptionType {
			if err := g.ownSubscription(ctx, key); err != nil {
				return err
			}
		}

		return g.recordWrite(ctx, record)
	}
}

// ownSubscription records the standing a Subscription delivers as.
//
// It is taken from the caller rather than from the resource, because a client
// that could state it could subscribe as somebody else — and what a subscriber
// may be told is exactly what that somebody may read.
func (g granted) ownSubscription(ctx context.Context, key storage.ResourceKey) error {
	if g.notifications == nil {
		return errSubscriptionsUnavailable
	}

	if !g.standing.Principal.Valid() || g.standing.Membership == "" {
		return subscription.ErrMissingOwner
	}

	return g.notifications.Owns(ctx, key.Project, key.ID, g.standing)
}

// recordWrite notes that something was written, for whoever is watching.
//
// One row, whatever the subscription list looks like: matching inside the write
// would make every write cost as much as that list is long, and would hold the
// transaction open while it did.
func (g granted) recordWrite(ctx context.Context, record storage.ResourceRecord) error {
	if g.notifications == nil {
		return nil
	}

	id, err := subscription.MintWriteID(rand.Reader)
	if err != nil {
		return err
	}

	return g.notifications.Record(ctx, subscription.Written{
		Project: record.Key.Project, ID: id,
		Key: record.Key, Version: record.Version, At: time.Now().UTC(),
	})
}
