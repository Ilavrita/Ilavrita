package pocketbase

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// writeSearchIndex replaces the row's index projection with the one its current
// content states.
//
// It is derived here rather than handed in, because the content being written
// is the only thing that decides it and this is the transaction writing it. A
// projection maintained anywhere else is one that can be skipped, and an index
// nothing maintains answers searches from whatever the resource used to say.
func (s *ResourceStore) writeSearchIndex(
	ctx context.Context, key storage.ResourceKey, content []byte,
) error {
	const clear = "DELETE FROM fhir_search_index" +
		" WHERE project_id = ? AND res_type = ? AND res_id = ?"

	if _, err := s.conn(ctx).ExecContext(ctx, clear,
		string(key.Project), string(key.Type), string(key.ID)); err != nil {
		return fmt.Errorf("pocketbase: clear the search index: %w", err)
	}

	custom, err := s.CustomParameters(ctx, key.Project)
	if err != nil {
		return err
	}

	entries, err := search.Extract(custom, key.Type, content)
	if err != nil {
		return fmt.Errorf("pocketbase: index %s/%s: %w", key.Type, key.ID, err)
	}

	const insert = "INSERT INTO fhir_search_index" +
		" (project_id, res_type, res_id, param, kind, code, system, folded, lower, upper)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

	for _, entry := range entries {
		code, system, folded, lower, upper := indexColumns(entry)

		if _, err := s.conn(ctx).ExecContext(ctx, insert,
			string(key.Project), string(key.Type), string(key.ID),
			entry.Parameter, string(entry.Kind), code, system, folded, lower, upper); err != nil {
			return fmt.Errorf("pocketbase: index %s on %s/%s: %w",
				entry.Parameter, key.Type, key.ID, err)
		}
	}

	return nil
}

// indexColumns places one entry's value in the columns its kind uses, leaving
// every other column NULL. The table's own CHECK refuses anything else, so a
// kind added here without its columns fails loudly rather than storing a row no
// predicate can read.
func indexColumns(entry search.Entry) (code, system, folded, lower, upper any) {
	switch entry.Kind {
	case search.KindToken:
		if entry.System != "" {
			system = entry.System
		}

		return entry.Code, system, nil, nil, nil
	case search.KindReference:
		return entry.Code, nil, nil, nil, nil
	case search.KindString:
		// The value as written and the value folded: one is what :exact reads,
		// the other what a prefix match reads.
		return entry.Code, nil, entry.Folded, nil, nil
	case search.KindDate:
		return nil, nil, nil, entry.Lower, entry.Upper
	default:
		return nil, nil, nil, nil, nil
	}
}

// backfillSearchIndex projects every resource that already exists.
//
// The index table is created by the schema like any other, so an install that
// predates it comes up with the table empty and every resource in it
// unsearchable — a predicate with nothing behind it, answering "no matches" for
// data that is plainly there. The projection is derived from content the rows
// already hold, so it can be rebuilt rather than migrated.
//
// It runs in one transaction: a partially rebuilt index answers some searches
// and not others, which is harder to notice than one that was never built.
func backfillSearchIndex(ctx context.Context, db *sql.DB) error {
	store := NewResourceStore(db)

	return store.WithinTransaction(ctx, func(ctx context.Context) error {
		keys, err := currentResources(ctx, db)
		if err != nil {
			return err
		}

		for _, held := range keys {
			if err := store.writeSearchIndex(ctx, held.key, held.content); err != nil {
				return err
			}
		}

		return nil
	})
}

// indexable is one current resource and the content its index is derived from.
type indexable struct {
	key     storage.ResourceKey
	content []byte
}

// currentResources reads every live resource.
//
// A tombstone would derive nothing anyway, because it holds no content; the
// clause skips reading and clearing for rows that could only produce an empty
// projection. What keeps a deleted resource out of a search is the row the
// index joins, not its absence from here.
func currentResources(ctx context.Context, db *sql.DB) ([]indexable, error) {
	const query = "SELECT project_id, res_type, res_id, content FROM fhir_resource" +
		" WHERE deleted = 0 AND content IS NOT NULL"

	rows, err := conn(ctx, db).QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read resources to index: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var held []indexable

	for rows.Next() {
		var (
			project, resourceType, id string
			content                   []byte
		)

		if err := rows.Scan(&project, &resourceType, &id, &content); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a resource to index: %w", err)
		}

		held = append(held, indexable{
			key: storage.ResourceKey{
				Project: storage.ProjectID(project),
				Type:    storage.ResourceType(resourceType),
				ID:      storage.LogicalID(id),
			},
			content: content,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read resources to index: %w", err)
	}

	return held, nil
}
