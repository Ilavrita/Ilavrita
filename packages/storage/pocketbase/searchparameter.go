package pocketbase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// searchParameterType is the resource a Project defines a parameter with.
const searchParameterType storage.ResourceType = "SearchParameter"

// CustomParameters reads the parameters one Project added to the built-in
// registry.
//
// It reads the compiled shape rather than the SearchParameter resources, so a
// write does not mean walking the element model, and a definition that compiled
// once keeps working whatever the model does afterwards.
func (s *ResourceStore) CustomParameters(
	ctx context.Context, project storage.ProjectID,
) (search.Custom, error) {
	const query = "SELECT res_type, code, kind, path, code_member, system_member" +
		" FROM search_parameter WHERE project_id = ? ORDER BY res_type, code"

	rows, err := s.conn(ctx).QueryContext(ctx, query, string(project))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read the custom parameters: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var custom search.Custom

	for rows.Next() {
		var resourceType, code, kind, path, codeMember, systemMember string

		if err := rows.Scan(&resourceType, &code, &kind, &path, &codeMember, &systemMember); err != nil {
			return nil, fmt.Errorf("pocketbase: read a custom parameter: %w", err)
		}

		parameter, err := search.Rebuild(code, search.Kind(kind), path, codeMember, systemMember)
		if err != nil {
			// A row that no longer compiles is a defect in this store, not a
			// query somebody got wrong. Answering the search without it would
			// silently drop a restriction the caller stated.
			return nil, fmt.Errorf("pocketbase: %s/%s no longer compiles: %w", resourceType, code, err)
		}

		if custom == nil {
			custom = search.Custom{}
		}

		custom.Add(search.Defined{Type: storage.ResourceType(resourceType), Parameter: parameter})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read the custom parameters: %w", err)
	}

	return custom, nil
}

// writeSearchParameters replaces what one SearchParameter resource defines.
//
// It runs in the transaction that writes the resource, like every other
// projection here: a definition maintained separately is one a write can skip,
// and a registry that disagrees with the resources behind it answers queries
// nobody defined.
//
// A resource that compiles to nothing — a draft, or a deletion — leaves the
// Project with nothing from it, which is the same statement either way.
func (s *ResourceStore) writeSearchParameters(
	ctx context.Context, key storage.ResourceKey, content []byte, at time.Time,
) error {
	replaced, err := s.clearSearchParameters(ctx, key)
	if err != nil {
		return err
	}

	defined, err := compiledFrom(content)
	if err != nil {
		return err
	}

	const insert = "INSERT INTO search_parameter" +
		" (project_id, res_type, code, kind, path, code_member, system_member, source_id, defined_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"

	for _, held := range defined {
		if _, err := s.conn(ctx).ExecContext(ctx, insert,
			string(key.Project), string(held.Type), held.Parameter.Name(),
			string(held.Parameter.Kind()), strings.Join(held.Parameter.Path(), "."),
			held.Parameter.CodeMember(), held.Parameter.SystemMember(),
			string(key.ID), at.UTC().UnixMilli()); err != nil {
			return fmt.Errorf("pocketbase: define %s on %s: %w",
				held.Parameter.Name(), held.Type, err)
		}
	}

	// Whatever this source used to define is no longer defined, so the rows it
	// indexed answer a parameter nobody states. They go with it: a stale row is
	// a match for a restriction that no longer exists.
	if err := s.clearIndexedBy(ctx, key.Project, replaced); err != nil {
		return err
	}

	// What it defines now is true of resources written from here on, and of
	// nothing already stored. Every type it names is owed a walk.
	for _, held := range defined {
		if err := s.enqueueReindex(ctx, key.Project, held.Type, at); err != nil {
			return err
		}
	}

	return nil
}

// compiledFrom reads a stored SearchParameter, treating content nothing can
// read as defining nothing.
//
// A deletion writes no content, and a resource that is not a SearchParameter
// never reaches here. What is left is a resource somebody wrote that does not
// compile — and that is refused at the door by validation, so reaching this
// with one is a defect worth surfacing rather than swallowing.
func compiledFrom(content []byte) ([]search.Defined, error) {
	if len(content) == 0 {
		return nil, nil
	}

	defined, err := search.ReadSearchParameter(content)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: compile a stored SearchParameter: %w", err)
	}

	return defined, nil
}

// definedParameter is one parameter a source defined, named by where it lives.
type definedParameter struct {
	resourceType string
	code         string
}

// clearSearchParameters removes what one source defined and reports what that
// was, so the index rows behind it can go too.
func (s *ResourceStore) clearSearchParameters(
	ctx context.Context, key storage.ResourceKey,
) ([]definedParameter, error) {
	const read = "SELECT res_type, code FROM search_parameter" +
		" WHERE project_id = ? AND source_id = ?"

	rows, err := s.conn(ctx).QueryContext(ctx, read, string(key.Project), string(key.ID))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read what %s defined: %w", key.ID, err)
	}

	defer func() { _ = rows.Close() }()

	var held []definedParameter

	for rows.Next() {
		var one definedParameter
		if err := rows.Scan(&one.resourceType, &one.code); err != nil {
			return nil, fmt.Errorf("pocketbase: read what %s defined: %w", key.ID, err)
		}

		held = append(held, one)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read what %s defined: %w", key.ID, err)
	}

	const clear = "DELETE FROM search_parameter WHERE project_id = ? AND source_id = ?"

	if _, err := s.conn(ctx).ExecContext(ctx, clear, string(key.Project), string(key.ID)); err != nil {
		return nil, fmt.Errorf("pocketbase: clear what %s defined: %w", key.ID, err)
	}

	return held, nil
}

// clearIndexedBy removes the index rows a parameter that no longer exists left
// behind.
func (s *ResourceStore) clearIndexedBy(
	ctx context.Context, project storage.ProjectID, gone []definedParameter,
) error {
	const clear = "DELETE FROM fhir_search_index" +
		" WHERE project_id = ? AND res_type = ? AND param = ?"

	for _, held := range gone {
		if _, err := s.conn(ctx).ExecContext(ctx, clear,
			string(project), held.resourceType, held.code); err != nil {
			return fmt.Errorf("pocketbase: clear the index for %s on %s: %w",
				held.code, held.resourceType, err)
		}
	}

	return nil
}
