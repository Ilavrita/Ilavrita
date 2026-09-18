package pocketbase

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ErrUncompilableQuery reports a plan this backend cannot compile. It is
// refused rather than compiled without the part it could not read, for the
// reason a filter is: a criterion silently dropped returns more than the caller
// asked for, and they cannot tell.
var ErrUncompilableQuery = fmt.Errorf("pocketbase: this backend cannot compile the query")

// The relation a search reads and the predicates it always carries. A tombstone
// is excluded here rather than by clearing its index, so a delete stays one
// statement and a recreate does not have to restore anything.
const (
	searchRelation  = "fhir_resource r"
	searchPredicate = "r.project_id = ? AND r.res_type = ? AND r.deleted = 0"

	// Correlated to the row rather than to a literal key, which is the only
	// difference between this and the by-key compartment predicate.
	searchCompartmentPredicate = "EXISTS (SELECT 1 FROM fhir_resource_compartment c" +
		" WHERE c.project_id = ? AND c.comp_type = ? AND c.comp_id = ?" +
		" AND c.res_type = r.res_type AND c.res_id = r.res_id)"

	// One criterion, as the values one parameter accepts.
	indexPredicate = "EXISTS (SELECT 1 FROM fhir_search_index i" +
		" WHERE i.project_id = ? AND i.res_type = r.res_type AND i.res_id = r.res_id" +
		" AND i.param = ? AND ("
)

var _ search.Repository = (*ResourceStore)(nil)

// Search returns one page of the resources a Scope authorizes and a query
// matches. The Scope is the same one a by-key read carries: the criteria are
// added to its predicate and nothing is taken out of it (SRC-3).
func (s *ResourceStore) Search(
	ctx context.Context, scope storage.Scope, query search.Query,
) (search.Page, error) {
	arms, err := searchArms(scope, query)
	if err != nil {
		return search.Page{}, err
	}

	page, err := s.readPage(ctx, scope, query, arms)
	if err != nil {
		return search.Page{}, err
	}

	if !query.CountsTotal() {
		return page, nil
	}

	total, err := s.countMatches(ctx, arms)
	if err != nil {
		return search.Page{}, err
	}

	page.Total, page.Counted = total, true

	return page, nil
}

// readPage reads one page and one row beyond it, which is how a next link is
// offered only when something actually follows.
func (s *ResourceStore) readPage(
	ctx context.Context,
	scope storage.Scope,
	query search.Query,
	arms []arm,
) (search.Page, error) {
	paged := make([]arm, 0, len(arms))

	for _, compiled := range arms {
		if cursor := query.Cursor(); cursor != "" {
			compiled.filter("r.res_id > ?", string(cursor))
		}

		paged = append(paged, compiled)
	}

	text, args := union(currentColumns, searchRelation, paged)
	text = "SELECT " + recordColumns + " FROM (" + text + ") ORDER BY res_id LIMIT " +
		strconv.Itoa(query.Count()+1)

	rows, err := s.conn(ctx).QueryContext(ctx, text, args...)
	if err != nil {
		return search.Page{}, fmt.Errorf("pocketbase: search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []storage.ResourceRecord

	for rows.Next() {
		record, _, err := scanRecord(rows)
		if err != nil {
			return search.Page{}, err
		}

		if err := assertInScope(scope, record, storage.ActionSearch); err != nil {
			return search.Page{}, err
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return search.Page{}, fmt.Errorf("pocketbase: search: %w", err)
	}

	// Narrowing reads each row's placement, which is a query of its own; this
	// pool holds one connection, so the cursor is closed before any of them.
	if err := rows.Close(); err != nil {
		return search.Page{}, fmt.Errorf("pocketbase: close the search cursor: %w", err)
	}

	page := search.Page{}

	if len(records) > query.Count() {
		records, page.More = records[:query.Count()], true
	}

	for index, record := range records {
		narrowed, err := s.narrowed(ctx, scope, record, storage.ActionSearch, s.currentPlacement)
		if err != nil {
			return search.Page{}, err
		}

		records[index] = narrowed
	}

	page.Records = records

	return page, nil
}

// countMatches counts every match, not just this page's. It runs over the same
// arms, so a total can never describe a wider set than the page came from.
func (s *ResourceStore) countMatches(ctx context.Context, arms []arm) (int, error) {
	text, args := union("r.res_id AS res_id", searchRelation, arms)

	var total int

	if err := s.conn(ctx).QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ("+text+")", args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("pocketbase: count search matches: %w", err)
	}

	return total, nil
}

// searchArms compiles one arm per authorizing Grant, each carrying that Grant's
// own restrictions and every criterion the query states.
func searchArms(scope storage.Scope, query search.Query) ([]arm, error) {
	grants := searchGrants(scope, query.Type())
	arms := make([]arm, 0, len(grants))

	for _, grant := range grants {
		compiled, err := searchArm(grant, query)
		if err != nil {
			return nil, err
		}

		arms = append(arms, compiled)
	}

	return arms, nil
}

// searchGrants keeps the Grants that authorize a search of this type. It is
// authorizedGrants without a key, because a search names no resource: the same
// checks, asked of the type alone.
func searchGrants(scope storage.Scope, resourceType storage.ResourceType) []storage.Grant {
	kept := make([]storage.Grant, 0, len(scope.Grants()))

	for _, grant := range scope.Grants() {
		if grant.Kind != storeKind || grant.Type != resourceType || grant.Action != storage.ActionSearch {
			continue
		}

		if !scope.Allows(grant.Project, storeKind, resourceType, storage.ActionSearch) {
			continue
		}

		kept = append(kept, grant)
	}

	return kept
}

// searchArm compiles one Grant and the whole query against the current-state
// table.
func searchArm(grant storage.Grant, query search.Query) (arm, error) {
	var compiled arm

	compiled.relation(grant.Project, searchPredicate, string(query.Type()))

	if grant.Compartment != nil {
		compiled.relation(grant.Project, searchCompartmentPredicate,
			string(grant.Compartment.Type), string(grant.Compartment.ID))
	}

	predicate, args, err := filterPredicate("r.content", grant.Filter)
	if err != nil {
		return arm{}, err
	}

	if predicate != "" {
		compiled.filter(predicate, args...)
	}

	for _, criterion := range query.Criteria() {
		text, criterionArgs, err := criterionPredicate(grant.Project, criterion)
		if err != nil {
			return arm{}, err
		}

		compiled.filter(text, criterionArgs...)
	}

	return compiled, nil
}

// criterionPredicate compiles one criterion. A stored parameter reads the row
// it is already on; every other reads the projection a write left behind.
func criterionPredicate(
	owner storage.ProjectID, criterion search.Criterion,
) (string, []any, error) {
	parameter := criterion.Parameter()

	if !parameter.Projects() {
		return storedPredicate(parameter, criterion.Values())
	}

	alternatives := make([]string, 0, len(criterion.Values()))
	args := []any{string(owner), parameter.Name()}

	for _, value := range criterion.Values() {
		text, valueArgs, err := indexedValue(parameter, value)
		if err != nil {
			return "", nil, err
		}

		alternatives = append(alternatives, text)
		args = append(args, valueArgs...)
	}

	return indexPredicate + strings.Join(alternatives, " OR ") + "))", args, nil
}

// indexedValue compiles one alternative against the index row.
func indexedValue(parameter search.Parameter, value search.Value) (string, []any, error) {
	switch parameter.Kind() {
	case search.KindToken:
		if system, qualified := value.System(); qualified {
			return "(i.code = ? AND i.system = ?)", []any{value.Text(), system}, nil
		}

		return "i.code = ?", []any{value.Text()}, nil
	case search.KindReference:
		return "i.code = ?", []any{value.Text()}, nil
	case search.KindString:
		// Folded on both sides, so the match does not depend on how either was
		// capitalised, and anchored, so a search never scans every value.
		return `i.folded LIKE ? ESCAPE '\'`, []any{likePrefix(value.Text())}, nil
	case search.KindDate:
		return spanPredicate("i.lower", "i.upper", value)
	default:
		return "", nil, fmt.Errorf("%w: parameter kind %q", ErrUncompilableQuery, string(parameter.Kind()))
	}
}

// storedPredicate compiles a parameter the row already answers.
func storedPredicate(parameter search.Parameter, values []search.Value) (string, []any, error) {
	column := "r." + parameter.Column()

	alternatives := make([]string, 0, len(values))

	var args []any

	for _, value := range values {
		switch parameter.Kind() {
		case search.KindToken, search.KindReference, search.KindString:
			alternatives = append(alternatives, column+" = ?")
			args = append(args, value.Text())
		case search.KindDate:
			text, spanArgs, err := spanPredicate(column, column, value)
			if err != nil {
				return "", nil, err
			}

			alternatives = append(alternatives, text)
			args = append(args, spanArgs...)
		default:
			return "", nil, fmt.Errorf("%w: parameter kind %q", ErrUncompilableQuery, string(parameter.Kind()))
		}
	}

	return "(" + strings.Join(alternatives, " OR ") + ")", args, nil
}

// spanPredicate compares a stored span against the span a value names. A stored
// instant is a span of one, so a column holding an instant is passed as both
// ends and the comparisons read the same.
func spanPredicate(lower, upper string, value search.Value) (string, []any, error) {
	from, to := value.Range()

	switch value.Compare() {
	case search.CompareEqual:
		// Overlap: a search for a day finds what happened during it, whether the
		// resource recorded a day or one moment inside it.
		return "(" + lower + " <= ? AND " + upper + " >= ?)", []any{to, from}, nil
	case search.CompareGreater:
		return lower + " > ?", []any{to}, nil
	case search.CompareLess:
		return upper + " < ?", []any{from}, nil
	case search.CompareGreaterEqual:
		return upper + " >= ?", []any{from}, nil
	case search.CompareLessEqual:
		return lower + " <= ?", []any{to}, nil
	default:
		return "", nil, fmt.Errorf("%w: comparison %q", ErrUncompilableQuery, string(value.Compare()))
	}
}

// likePrefix turns a value into an anchored LIKE pattern, escaping the
// wildcards so nothing a caller types is read as one.
func likePrefix(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(value))

	return escaped + "%"
}
