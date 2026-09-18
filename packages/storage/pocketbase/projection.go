package pocketbase

import (
	"context"
	"fmt"
	"slices"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ErrPartialView reports a whole-resource write from a caller whose own policy
// returns only part of that resource. It wraps ErrDenied, so a route answers it
// the way it answers any refusal.
var ErrPartialView = fmt.Errorf(
	"%w: a caller that reads part of a resource may not replace all of it", storage.ErrDenied)

// The placements a projection is decided against. A current row is placed by
// what it says now; a version by where it landed when it was written, which is
// the same distinction the compartment predicates already make.
const (
	currentPlacementQuery = "SELECT comp_type, comp_id FROM fhir_resource_compartment" +
		" WHERE project_id = ? AND res_type = ? AND res_id = ?"

	versionPlacementQuery = "SELECT hc.comp_type, hc.comp_id FROM fhir_resource_history_compartment hc" +
		" JOIN fhir_resource_history h ON h.project_id = hc.project_id AND h.res_type = hc.res_type" +
		" AND h.res_id = hc.res_id AND h.version_seq = hc.version_seq" +
		" WHERE hc.project_id = ? AND hc.res_type = ? AND hc.res_id = ? AND h.version_id = ?"
)

// placement is how a record's compartments are read, which differs between the
// current row and one version of it.
type placement func(context.Context, storage.ResourceRecord) ([]storage.Compartment, error)

// narrowed returns the record holding only what the Grants covering it return.
//
// When no Grant carries a projection the record is handed back untouched, which
// is both the common case and the cheapest: nothing is read and nothing is
// decoded. Otherwise the Grants that actually reach this row are worked out and
// their projections combined, because a Grant that does not reach the row has
// no say in how much of it is returned.
func (s *ResourceStore) narrowed(
	ctx context.Context,
	scope storage.Scope,
	record storage.ResourceRecord,
	action storage.Action,
	placed placement,
) (storage.ResourceRecord, error) {
	grants := authorizedGrants(scope, record.Key, action)

	if !slices.ContainsFunc(grants, func(grant storage.Grant) bool { return grant.Projection != nil }) {
		return record, nil
	}

	compartments, err := placed(ctx, record)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	reaching := make([]*storage.Projection, 0, len(grants))

	for _, grant := range grants {
		reaches, err := s.reaches(ctx, grant, record, compartments)
		if err != nil {
			return storage.ResourceRecord{}, err
		}

		if reaches {
			reaching = append(reaching, grant.Projection)
		}
	}

	// The query returned a row no Grant is now found to reach, so the compiled
	// predicate and this check disagree. Answering either way would be a guess:
	// returning the row discloses under a Grant nothing proved, and narrowing it
	// to nothing hides a row the caller may be owed.
	if len(reaching) == 0 {
		return storage.ResourceRecord{}, fmt.Errorf("%w: %s/%s in %s",
			ErrScopeEscape, record.Key.Type, record.Key.ID, record.Key.Project)
	}

	widest := storage.WidestProjection(reaching)
	if widest == nil {
		return record, nil
	}

	content, err := widest.Apply(record.Content)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	record.Content = content

	return record, nil
}

// reaches reports whether one Grant covers this row. It asks the two questions
// the compiled predicate asks, in the same order and against the same facts:
// where the resource is placed, and whether its content satisfies the filter.
func (s *ResourceStore) reaches(
	ctx context.Context,
	grant storage.Grant,
	record storage.ResourceRecord,
	compartments []storage.Compartment,
) (bool, error) {
	if grant.Compartment != nil && !slices.Contains(compartments, *grant.Compartment) {
		return false, nil
	}

	return s.admits(ctx, grant, record.Content)
}

// currentPlacement reads where the row is placed now.
func (s *ResourceStore) currentPlacement(
	ctx context.Context, record storage.ResourceRecord,
) ([]storage.Compartment, error) {
	key := record.Key

	return s.readPlacement(ctx, currentPlacementQuery,
		string(key.Project), string(key.Type), string(key.ID))
}

// versionPlacement reads where the row was placed when this version was
// written, never where it has since moved to.
func (s *ResourceStore) versionPlacement(
	ctx context.Context, record storage.ResourceRecord,
) ([]storage.Compartment, error) {
	key := record.Key

	return s.readPlacement(ctx, versionPlacementQuery,
		string(key.Project), string(key.Type), string(key.ID), string(record.Version))
}

func (s *ResourceStore) readPlacement(
	ctx context.Context, query string, args ...any,
) ([]storage.Compartment, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read a resource's placement: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var compartments []storage.Compartment

	for rows.Next() {
		var compartment storage.Compartment
		if err := rows.Scan(&compartment.Type, &compartment.ID); err != nil {
			return nil, fmt.Errorf("pocketbase: scan a resource's placement: %w", err)
		}

		compartments = append(compartments, compartment)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read a resource's placement: %w", err)
	}

	return compartments, nil
}

// refuseBlindReplace refuses a whole-resource write from a caller who cannot
// see the whole resource. An update replaces content wholesale, so a caller
// holding only projected read Grants would send back what they were shown and
// silently drop every element their own policy withheld — data loss produced by
// an authorization rule rather than by anyone's intent.
//
// A caller with no read Grant at all is not blind in this sense: nothing showed
// them a partial resource to send back, and the read-back that renders the
// write already answers for them.
func refuseBlindReplace(scope storage.Scope, key storage.ResourceKey) error {
	if !scope.Withholds(key.Project, storeKind, key.Type, storage.ActionRead) {
		return nil
	}

	return ErrPartialView
}
