package pocketbase

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// Reindexing is what makes a new parameter true of resources already stored.
//
// The index is built on write, so a parameter defined today describes nothing
// written yesterday. A search by it would answer an empty page, which is what a
// correct search looks like — so the gap has to be closed by walking the type
// again rather than left for somebody to notice.
//
// It is a backlog rather than part of the write because a Project's whole
// Organization table is not work to do inside the request that defined the
// parameter: one pooled connection per process means that request would be
// every other request waiting.

// ReindexWork is one type whose index no longer matches its parameters.
type ReindexWork struct {
	Project storage.ProjectID
	Type    storage.ResourceType
}

// enqueueReindex records that one type needs walking again.
//
// Already enqueued is already correct: the work is "make this index match the
// parameters", which does not change by being asked for twice. The row keeps
// the time it was first asked, so the oldest debt is paid first.
func (s *ResourceStore) enqueueReindex(
	ctx context.Context, project storage.ProjectID, resourceType storage.ResourceType, at time.Time,
) error {
	const insert = "INSERT INTO reindex_backlog (project_id, res_type, at)" +
		" VALUES (?, ?, ?) ON CONFLICT (project_id, res_type) DO NOTHING"

	if _, err := s.conn(ctx).ExecContext(ctx, insert,
		string(project), string(resourceType), at.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: enqueue a reindex of %s: %w", resourceType, err)
	}

	return nil
}

// ClaimReindex takes up to limit types for this worker until the claim expires.
//
// A claim that expires is work returned rather than work lost: a replica that
// died mid-walk leaves an index half rebuilt, and the next claim starts it
// again. Rebuilding an index that is already right costs time and changes
// nothing, which is what makes that safe.
func (s *ResourceStore) ClaimReindex(
	ctx context.Context, worker string, until, now time.Time, limit int,
) ([]ReindexWork, error) {
	if worker == "" || limit <= 0 {
		return nil, fmt.Errorf("pocketbase: a reindex claim names a worker and a limit")
	}

	const query = "UPDATE reindex_backlog SET claimed_by = ?, claimed_until = ?" +
		" WHERE rowid IN (SELECT rowid FROM reindex_backlog" +
		" WHERE claimed_until IS NULL OR claimed_until <= ?" +
		" ORDER BY at, res_type LIMIT ?)" +
		" RETURNING project_id, res_type"

	rows, err := s.db.QueryContext(ctx, query,
		worker, until.UTC().UnixMilli(), now.UTC().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: claim a reindex: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var held []ReindexWork

	for rows.Next() {
		var work ReindexWork
		if err := rows.Scan(&work.Project, &work.Type); err != nil {
			return nil, fmt.Errorf("pocketbase: read a claimed reindex: %w", err)
		}

		held = append(held, work)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: claim a reindex: %w", err)
	}

	return held, nil
}

// Reindex rebuilds one type's index in one Project and settles the backlog row.
//
// Every resource is read before any is written. One pooled connection per
// process means a write issued while a cursor is open deadlocks, and this walks
// a whole type.
func (s *ResourceStore) Reindex(ctx context.Context, work ReindexWork) error {
	return s.WithinTransaction(ctx, func(ctx context.Context) error {
		held, err := s.resourcesOfType(ctx, work)
		if err != nil {
			return err
		}

		for _, one := range held {
			if err := s.writeSearchIndex(ctx, one.key, one.content); err != nil {
				return err
			}
		}

		const settle = "DELETE FROM reindex_backlog WHERE project_id = ? AND res_type = ?"

		if _, err := s.conn(ctx).ExecContext(ctx, settle,
			string(work.Project), string(work.Type)); err != nil {
			return fmt.Errorf("pocketbase: settle the reindex of %s: %w", work.Type, err)
		}

		return nil
	})
}

// resourcesOfType reads every current resource of one type in one Project, whole,
// so nothing is written while the cursor is open.
func (s *ResourceStore) resourcesOfType(
	ctx context.Context, work ReindexWork,
) ([]indexable, error) {
	const query = "SELECT project_id, res_type, res_id, content FROM fhir_resource" +
		" WHERE project_id = ? AND res_type = ? AND deleted = 0 AND content IS NOT NULL"

	rows, err := s.conn(ctx).QueryContext(ctx, query, string(work.Project), string(work.Type))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read %s to reindex: %w", work.Type, err)
	}

	defer func() { _ = rows.Close() }()

	var held []indexable

	for rows.Next() {
		var (
			one     indexable
			content []byte
		)

		if err := rows.Scan(&one.key.Project, &one.key.Type, &one.key.ID, &content); err != nil {
			return nil, fmt.Errorf("pocketbase: read a resource to reindex: %w", err)
		}

		one.content = content
		held = append(held, one)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read %s to reindex: %w", work.Type, err)
	}

	return held, nil
}

// PendingReindexes reports how many types are waiting, which is what an
// operator asks when a search is answering less than they expect.
func (s *ResourceStore) PendingReindexes(ctx context.Context) (int, error) {
	var held int

	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM reindex_backlog").Scan(&held); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}

		return 0, fmt.Errorf("pocketbase: count the reindex backlog: %w", err)
	}

	return held, nil
}
