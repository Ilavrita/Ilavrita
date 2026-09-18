package pocketbase

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// The columns a link rebuilds from, in the order inboundRow reads them.
const linkColumns = "grantor_project, kind, status, all_resource_types," +
	" COALESCE(grantor_access_policy_id, ''), COALESCE(history_from, 0), COALESCE(expires_at, 0)," +
	" COALESCE(activated_at, 0), COALESCE(grantor_approved_by, ''), COALESCE(grantor_approved_at, 0)," +
	" COALESCE(grantee_approved_by, ''), COALESCE(grantee_approved_at, 0)"

const (
	// Only the grantee is bound, because a link reaches in one direction and this
	// resolver answers the question "what reaches into this Project".
	inboundLinks = "SELECT " + linkColumns + " FROM project_links" +
		" WHERE grantee_project = ? ORDER BY grantor_project, kind"

	linkResourceTypes = "SELECT res_type FROM project_link_types" +
		" WHERE grantee_project = ? AND grantor_project = ? AND kind = ? ORDER BY res_type"

	linkCapabilities = "SELECT capability, principal_membership_id FROM project_link_capabilities" +
		" WHERE grantee_project = ? AND grantor_project = ? AND kind = ?" +
		" ORDER BY capability, principal_membership_id"
)

// LinkStore reads the links that let one Project reach into another. It resolves
// inbound links only: a grantor's own inbound links are never consulted, which is
// what stops A reaching C through B.
type LinkStore struct {
	db *sql.DB
}

var _ project.LinkResolver = (*LinkStore)(nil)

// NewLinkStore binds a resolver to an open database. The caller owns the pool and
// is responsible for opening it with foreign keys enforced.
func NewLinkStore(db *sql.DB) *LinkStore {
	return &LinkStore{db: db}
}

// inboundRow is one project_links row as SQLite hands it over.
type inboundRow struct {
	grantor     string
	kind        string
	status      string
	allTypes    int
	policy      string
	historyFrom int64
	expiresAt   int64
	activatedAt int64
	grantorBy   string
	grantorAt   int64
	granteeBy   string
	granteeAt   int64
}

func (r *inboundRow) dest() []any {
	return []any{
		&r.grantor, &r.kind, &r.status, &r.allTypes, &r.policy, &r.historyFrom, &r.expiresAt,
		&r.activatedAt, &r.grantorBy, &r.grantorAt, &r.granteeBy, &r.granteeAt,
	}
}

// Inbound returns every link reaching into one Project, in whatever lifecycle
// state it holds. Link.Effective is what decides whether one authorizes anything,
// so this must not pre-filter: a suspended link a decision can see is a different
// fact from one that does not exist.
func (s *LinkStore) Inbound(ctx context.Context, grantee project.ID) ([]project.Link, error) {
	if err := project.ValidateID(grantee); err != nil {
		return nil, err
	}

	scanned, err := s.inbound(ctx, grantee)
	if err != nil {
		return nil, err
	}

	// The child reads run after the parent cursor is drained, because the pool is
	// one connection and a query inside the loop would wait on itself.
	links := make([]project.Link, 0, len(scanned))

	for _, row := range scanned {
		link, confers, err := s.rebuild(ctx, grantee, row)
		if err != nil {
			return nil, err
		}

		if confers {
			links = append(links, link)
		}
	}

	return links, nil
}

// inbound reads the rows themselves, leaving every child query until the cursor
// is closed.
func (s *LinkStore) inbound(ctx context.Context, grantee project.ID) ([]inboundRow, error) {
	rows, err := conn(ctx, s.db).QueryContext(ctx, inboundLinks, string(grantee))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read links into %s: %w", grantee, err)
	}
	defer func() { _ = rows.Close() }()

	var scanned []inboundRow

	for rows.Next() {
		var row inboundRow

		if err := rows.Scan(row.dest()...); err != nil {
			return nil, fmt.Errorf("pocketbase: scan link into %s: %w", grantee, err)
		}

		scanned = append(scanned, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read links into %s: %w", grantee, err)
	}

	return scanned, nil
}

// rebuild turns one row into a Link through the constructor matching its kind, so
// a row written around them fails the read rather than reaching a decision.
// rebuild turns one row into a Link through the constructor matching its kind. A
// link naming nothing to share or confer is reported as conferring nothing, not
// as an error: the schema permits the child rows to be absent, and one such row
// must not deny every decision the Project makes.
func (s *LinkStore) rebuild(
	ctx context.Context, grantee project.ID, row inboundRow,
) (project.Link, bool, error) {
	grantor := project.ID(row.grantor)
	kind := project.LinkKind(row.kind)

	if !project.LinkStatus(row.status).Valid() {
		return project.Link{}, false, fmt.Errorf("%w: %s into %s holds %q",
			ErrUnknownLinkStatus, grantor, grantee, row.status)
	}

	id := linkIdentifier(grantee, grantor, kind)

	link, confers, err := s.construct(ctx, id, grantee, grantor, kind, row)
	if err != nil || !confers {
		return project.Link{}, false, err
	}

	applied, err := s.apply(link, row)
	if err != nil {
		return project.Link{}, false, fmt.Errorf("pocketbase: rebuild link %s: %w", id, err)
	}

	return applied, true, nil
}

// construct builds the link its kind calls for, reporting false when the row
// names nothing to share or confer. A kind outside the enum is schema drift and
// fails rather than resolving to the narrower of the two.
func (s *LinkStore) construct(
	ctx context.Context, id project.LinkID, grantee, grantor project.ID,
	kind project.LinkKind, row inboundRow,
) (project.Link, bool, error) {
	switch kind {
	case project.LinkKindData:
		types, err := s.resourceTypes(ctx, grantee, grantor, kind)
		if err != nil || len(types) == 0 {
			return project.Link{}, false, err
		}

		link, err := project.NewDataLink(id, grantor, grantee, storage.LogicalID(row.policy), types...)

		return link, err == nil, wrapRebuild(id, err)
	case project.LinkKindAdministrative:
		principals, capabilities, err := s.capabilities(ctx, grantee, grantor, kind)
		if err != nil || len(capabilities) == 0 || len(principals) == 0 {
			return project.Link{}, false, err
		}

		link, err := project.NewAdminLink(id, grantor, grantee, principals, capabilities...)

		return link, err == nil, wrapRebuild(id, err)
	default:
		return project.Link{}, false, fmt.Errorf("%w: %q", project.ErrUnknownKind, string(kind))
	}
}

// wrapRebuild names which link a constructor refused, so schema drift is
// traceable to the row that carries it.
func wrapRebuild(id project.LinkID, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("pocketbase: rebuild link %s: %w", id, err)
}

// apply replays the lifecycle the row records: the approvals, then the status,
// then the expiry and history floor. The status moves through TransitionTo, so a
// row claiming active without both approvals fails rather than authorizing.
func (s *LinkStore) apply(link project.Link, row inboundRow) (project.Link, error) {
	var err error

	if row.grantorBy != "" {
		if link, err = link.ApproveGrantor(
			project.MembershipID(row.grantorBy), instant(row.grantorAt)); err != nil {
			return project.Link{}, err
		}
	}

	if row.granteeBy != "" {
		if link, err = link.ApproveGrantee(
			project.MembershipID(row.granteeBy), instant(row.granteeAt)); err != nil {
			return project.Link{}, err
		}
	}

	if link, err = link.RestoreStatus(project.LinkStatus(row.status), instant(row.activatedAt)); err != nil {
		return project.Link{}, err
	}

	if row.expiresAt != 0 {
		link = link.WithExpiry(instant(row.expiresAt))
	}

	if row.historyFrom != 0 {
		link = link.WithHistoryFrom(instant(row.historyFrom))
	}

	return link, nil
}

// resourceTypes reads what a data link shares. A link naming none shares nothing,
// which the constructor refuses rather than reading as everything.
func (s *LinkStore) resourceTypes(
	ctx context.Context, grantee, grantor project.ID, kind project.LinkKind,
) ([]storage.ResourceType, error) {
	rows, err := conn(ctx, s.db).QueryContext(ctx, linkResourceTypes,
		string(grantee), string(grantor), string(kind))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read shared types of %s into %s: %w", grantor, grantee, err)
	}
	defer func() { _ = rows.Close() }()

	var types []storage.ResourceType

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pocketbase: scan shared type: %w", err)
		}

		types = append(types, storage.ResourceType(name))
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read shared types of %s into %s: %w", grantor, grantee, err)
	}

	return types, nil
}

// capabilities reads what an administrative link confers and on whom. The holders
// are enumerated memberships, never a role whose roster the grantee could write.
func (s *LinkStore) capabilities(
	ctx context.Context, grantee, grantor project.ID, kind project.LinkKind,
) ([]project.PrincipalRef, []project.AdminCapability, error) {
	rows, err := conn(ctx, s.db).QueryContext(ctx, linkCapabilities,
		string(grantee), string(grantor), string(kind))
	if err != nil {
		return nil, nil, fmt.Errorf("pocketbase: read capabilities of %s into %s: %w", grantor, grantee, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		capabilities []project.AdminCapability
		principals   []project.PrincipalRef
		seenCap      = map[string]bool{}
		seenHolder   = map[string]bool{}
	)

	for rows.Next() {
		var capability, holder string
		if err := rows.Scan(&capability, &holder); err != nil {
			return nil, nil, fmt.Errorf("pocketbase: scan link capability: %w", err)
		}

		if !seenCap[capability] {
			seenCap[capability] = true

			capabilities = append(capabilities, project.AdminCapability(capability))
		}

		if !seenHolder[holder] {
			seenHolder[holder] = true

			principals = append(principals,
				project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(holder)})
		}
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("pocketbase: read capabilities of %s into %s: %w", grantor, grantee, err)
	}

	return principals, capabilities, nil
}

// instant turns a stored epoch into a time, reading zero as no instant rather
// than as the epoch itself.
func instant(millis int64) time.Time {
	if millis == 0 {
		return time.Time{}
	}

	return time.UnixMilli(millis).UTC()
}
