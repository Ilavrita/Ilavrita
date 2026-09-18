package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// SubscriptionStore keeps what a write owes a subscriber.
//
// Recording a write joins the transaction that made it, so a write that rolls
// back owes nobody anything. Everything else runs on its own: a worker is not
// inside anybody's request.
type SubscriptionStore struct {
	db *sql.DB
}

var _ subscription.Queue = (*SubscriptionStore)(nil)

// NewSubscriptionStore binds the queue to an open database.
func NewSubscriptionStore(db *sql.DB) *SubscriptionStore {
	return &SubscriptionStore{db: db}
}

// Record notes one write, in the transaction that made it.
func (s *SubscriptionStore) Record(ctx context.Context, written subscription.Written) error {
	const insert = "INSERT INTO subscription_backlog" +
		" (project_id, id, res_type, res_id, version_id, at) VALUES (?, ?, ?, ?, ?, ?)"

	if _, err := conn(ctx, s.db).ExecContext(ctx, insert,
		string(written.Project), string(written.ID), string(written.Key.Type),
		string(written.Key.ID), string(written.Version), written.At.UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: record a write for its subscribers: %w", err)
	}

	return nil
}

// Backlog returns the writes nobody has fanned out yet, oldest first.
func (s *SubscriptionStore) Backlog(ctx context.Context, limit int) ([]subscription.Written, error) {
	const query = "SELECT project_id, id, res_type, res_id, version_id, at" +
		" FROM subscription_backlog ORDER BY at, id LIMIT ?"

	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read the subscription backlog: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var held []subscription.Written

	for rows.Next() {
		var (
			owner, id, resourceType, resourceID, version string
			at                                           int64
		)

		if err := rows.Scan(&owner, &id, &resourceType, &resourceID, &version, &at); err != nil {
			return nil, fmt.Errorf("pocketbase: scan the subscription backlog: %w", err)
		}

		held = append(held, subscription.Written{
			Project: storage.ProjectID(owner),
			ID:      storage.LogicalID(id),
			Key: storage.ResourceKey{
				Project: storage.ProjectID(owner),
				Type:    storage.ResourceType(resourceType),
				ID:      storage.LogicalID(resourceID),
			},
			Version: storage.VersionID(version),
			At:      time.UnixMilli(at).UTC(),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read the subscription backlog: %w", err)
	}

	return held, nil
}

// Settle removes one backlog entry, having enqueued whatever it owed.
func (s *SubscriptionStore) Settle(
	ctx context.Context, owner storage.ProjectID, id storage.LogicalID,
) error {
	if _, err := conn(ctx, s.db).ExecContext(ctx,
		"DELETE FROM subscription_backlog WHERE project_id = ? AND id = ?",
		string(owner), string(id)); err != nil {
		return fmt.Errorf("pocketbase: settle a backlog entry: %w", err)
	}

	return nil
}

// Owns records who a Subscription delivers as, replacing whatever it had: a
// Subscription rewritten by somebody else delivers as them from then on.
func (s *SubscriptionStore) Owns(
	ctx context.Context,
	owner storage.ProjectID,
	held storage.LogicalID,
	standing subscription.Owner,
) error {
	const upsert = "INSERT INTO subscription_owners" +
		" (project_id, subscription_id, membership_id, principal_kind, principal_id, created_at)" +
		" VALUES (?, ?, ?, ?, ?, ?)" +
		" ON CONFLICT (project_id, subscription_id) DO UPDATE SET" +
		" membership_id = excluded.membership_id, principal_kind = excluded.principal_kind," +
		" principal_id = excluded.principal_id"

	if _, err := conn(ctx, s.db).ExecContext(ctx, upsert,
		string(owner), string(held), string(standing.Membership),
		string(standing.Principal.Kind), string(standing.Principal.ID),
		time.Now().UTC().UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: record who a subscription delivers as: %w", err)
	}

	return nil
}

// Owner returns who a Subscription delivers as, if anybody still does.
func (s *SubscriptionStore) Owner(
	ctx context.Context, owner storage.ProjectID, held storage.LogicalID,
) (subscription.Owner, bool, error) {
	const query = "SELECT membership_id, principal_kind, principal_id FROM subscription_owners" +
		" WHERE project_id = ? AND subscription_id = ?"

	var membership, kind, principal string

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, string(owner), string(held)).
		Scan(&membership, &kind, &principal); {
	case errors.Is(err, sql.ErrNoRows):
		return subscription.Owner{}, false, nil
	case err != nil:
		return subscription.Owner{}, false, fmt.Errorf("pocketbase: read a subscription's owner: %w", err)
	}

	return subscription.Owner{
		Membership: project.MembershipID(membership),
		Principal: project.PrincipalRef{
			Kind: project.PrincipalKind(kind), ID: project.PrincipalID(principal),
		},
	}, true, nil
}

// Owe enqueues one delivery.
func (s *SubscriptionStore) Owe(ctx context.Context, delivery subscription.Delivery) error {
	const insert = "INSERT INTO subscription_deliveries" +
		" (project_id, id, subscription_id, res_type, res_id, version_id," +
		" state, attempts, due_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)"

	now := time.Now().UTC().UnixMilli()

	if _, err := conn(ctx, s.db).ExecContext(ctx, insert,
		string(delivery.Project), string(delivery.ID), string(delivery.Subscription),
		string(delivery.Key.Type), string(delivery.Key.ID), string(delivery.Version),
		string(subscription.Pending), delivery.DueAt.UnixMilli(), now); err != nil {
		return fmt.Errorf("pocketbase: enqueue a notification: %w", err)
	}

	return nil
}

// Due returns the pending deliveries ready to be tried, oldest first.
func (s *SubscriptionStore) Due(
	ctx context.Context, at time.Time, limit int,
) ([]subscription.Delivery, error) {
	const query = "SELECT project_id, id, subscription_id, res_type, res_id, version_id," +
		" attempts, due_at FROM subscription_deliveries" +
		" WHERE state = ? AND due_at <= ? ORDER BY due_at, id LIMIT ?"

	rows, err := s.db.QueryContext(ctx, query, string(subscription.Pending), at.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read the deliveries that are due: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var held []subscription.Delivery

	for rows.Next() {
		var (
			owner, id, watched, resourceType, resourceID, version string
			attempts, due                                         int64
		)

		if err := rows.Scan(&owner, &id, &watched, &resourceType, &resourceID,
			&version, &attempts, &due); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a due delivery: %w", err)
		}

		held = append(held, subscription.Delivery{
			Project:      storage.ProjectID(owner),
			ID:           storage.LogicalID(id),
			Subscription: storage.LogicalID(watched),
			Key: storage.ResourceKey{
				Project: storage.ProjectID(owner),
				Type:    storage.ResourceType(resourceType),
				ID:      storage.LogicalID(resourceID),
			},
			Version:  storage.VersionID(version),
			State:    subscription.Pending,
			Attempts: int(attempts),
			DueAt:    time.UnixMilli(due).UTC(),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read the deliveries that are due: %w", err)
	}

	return held, nil
}

// Attempted records what became of one try.
//
// A settled delivery is kept rather than deleted: what was never delivered is
// the thing an operator needs to see, and a queue that tidies its own failures
// away answers no question afterwards.
func (s *SubscriptionStore) Attempted(
	ctx context.Context, delivery subscription.Delivery, state subscription.DeliveryState, at time.Time,
) error {
	const update = "UPDATE subscription_deliveries" +
		" SET state = ?, attempts = ?, due_at = ?, settled_at = ?" +
		" WHERE project_id = ? AND id = ?"

	var settled any
	if state != subscription.Pending {
		settled = at.UnixMilli()
	}

	if _, err := conn(ctx, s.db).ExecContext(ctx, update,
		string(state), delivery.Attempts, delivery.DueAt.UnixMilli(), settled,
		string(delivery.Project), string(delivery.ID)); err != nil {
		return fmt.Errorf("pocketbase: record a delivery attempt: %w", err)
	}

	return nil
}

// Watching returns the Subscriptions one Project currently holds.
//
// A tombstone is excluded: a deleted Subscription delivers nothing, which is
// how one is switched off for good.
func (s *SubscriptionStore) Watching(
	ctx context.Context, owner storage.ProjectID,
) ([]subscription.Watcher, error) {
	const query = "SELECT res_id, content FROM fhir_resource" +
		" WHERE project_id = ? AND res_type = 'Subscription' AND deleted = 0 ORDER BY res_id"

	rows, err := s.db.QueryContext(ctx, query, string(owner))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read a project's subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var held []subscription.Watcher

	for rows.Next() {
		var (
			id      string
			content []byte
		)

		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a subscription: %w", err)
		}

		held = append(held, subscription.Watcher{ID: storage.LogicalID(id), Content: content})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read a project's subscriptions: %w", err)
	}

	return held, nil
}
