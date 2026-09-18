package pocketbase

import (
	"context"
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

	entries, err := search.Extract(key.Type, content)
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
		return nil, nil, entry.Folded, nil, nil
	case search.KindDate:
		return nil, nil, nil, entry.Lower, entry.Upper
	default:
		return nil, nil, nil, nil, nil
	}
}
