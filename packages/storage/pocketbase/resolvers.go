package pocketbase

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrUnknownProjectState reports a projects row holding a state outside the
	// enum. An unrecognised state denies instead of being read as the nearest
	// known one.
	ErrUnknownProjectState = errors.New("pocketbase: project holds an unrecognised lifecycle state")

	// ErrUnknownLinkStatus reports a project_links row holding a status outside
	// the enum. No standing is rebuilt on one.
	ErrUnknownLinkStatus = errors.New("pocketbase: link holds an unrecognised status")

	// ErrUnreadableRule reports an access_policy_rules row matching none of the
	// three restriction shapes its own CHECK allows, which is schema drift and
	// never a guess at which shape was meant.
	ErrUnreadableRule = errors.New("pocketbase: access policy rule states no readable restriction")
)

// The columns a membership rebuilds from, in the order membershipRow reads them.
const membershipColumns = "m.id, m.project_kind, m.state, m.admin, m.super_admin," +
	" m.invitation_source, m.profile_type, m.profile_id, m.via_link_grantee_project, m.via_link_kind"

// The Project is bound as a literal on every relation read here, so no predicate
// compares one row's Project to another relation's.
const (
	membershipQuery = "SELECT " + membershipColumns + " FROM project_memberships m" +
		" WHERE m.project_id = ? AND m.principal_kind = ? AND m.principal_id = ?"

	// A user's standing dies with the identity behind it, and a project-scoped
	// identity answers only in its own realm: MembershipState has no value that
	// could carry either fact, so a row failing this reports no membership.
	activeIdentityPredicate = " AND EXISTS (SELECT 1 FROM users u WHERE u.id = m.user_id" +
		" AND u.state = 'active' AND (u.home_project_id IS NULL OR u.home_project_id = ?))"

	// A client application's standing dies with the registration behind it, which
	// is pinned to one Project: MembershipState has no value that could carry
	// either fact, so a row failing this reports no membership.
	activeClientApplicationPredicate = " AND EXISTS (SELECT 1 FROM client_applications c" +
		" WHERE c.project_id = ? AND c.id = m.client_application_id AND c.state = 'active')"

	// A bot is server-invoked and holds no credential, so its registration row is
	// the only thing that can withdraw it.
	activeBotPredicate = " AND EXISTS (SELECT 1 FROM bots b" +
		" WHERE b.project_id = ? AND b.id = m.bot_id AND b.state = 'active')"

	// A revoked row is history: at most one membership per principal is not
	// revoked, so the live one answers first and the choice stays deterministic.
	membershipOrder = " ORDER BY CASE WHEN m.state = 'revoked' THEN 1 ELSE 0 END, m.id LIMIT 1"

	bindingQuery = "SELECT policy_id, ordinal, policy_params FROM project_membership_policies" +
		" WHERE project_id = ? AND membership_id = ? ORDER BY ordinal, policy_id"

	// The membership's own Project is the link's grantor, pinned by the foreign
	// key, so this binds it as the grantor literal.
	linkStatusQuery = "SELECT status FROM project_links" +
		" WHERE grantor_project = ? AND grantee_project = ? AND kind = ?"

	projectStateQuery = "SELECT state FROM projects WHERE id = ?"

	policyQuery = "SELECT 1 FROM access_policies WHERE project_id = ? AND id = ?"

	policyParameterQuery = "SELECT name FROM access_policy_parameters" +
		" WHERE project_id = ? AND policy_id = ? ORDER BY name"

	policyRuleQuery = "SELECT kind, res_type, action, unrestricted," +
		" compartment_type, compartment_id, compartment_param FROM access_policy_rules" +
		" WHERE project_id = ? AND policy_id = ? ORDER BY ordinal"
)

// conn returns the transaction the context carries, so a resolver called inside
// one reads through it rather than waiting on a connection it holds.
func conn(ctx context.Context, db *sql.DB) executor {
	if tx, open := ctx.Value(transactionKey{}).(*sql.Tx); open {
		return tx
	}

	return db
}

// linkIdentifier names a link by its natural key, because project_links carries
// no id column: the two Projects and the kind are the link's identity.
func linkIdentifier(grantee, grantor project.ID, kind project.LinkKind) project.LinkID {
	return project.LinkID(string(grantee) + "/" + string(grantor) + "/" + string(kind))
}

// MembershipResolver reads one principal's standing from project_memberships. It
// answers with the row's own lifecycle state, so a revoked or link-minted
// membership rebuilds as standing nobody holds rather than as no row at all.
type MembershipResolver struct {
	db *sql.DB
}

var _ authz.MembershipResolver = (*MembershipResolver)(nil)

// NewMembershipResolver binds the resolver to an open database. The caller owns
// the pool and is responsible for opening it with foreign keys enforced.
func NewMembershipResolver(db *sql.DB) *MembershipResolver {
	return &MembershipResolver{db: db}
}

// membershipRow is one project_memberships row as SQLite hands it over.
type membershipRow struct {
	id          string
	projectKind string
	state       string
	admin       int
	superAdmin  int
	source      string
	profileType sql.NullString
	profileID   sql.NullString
	linkGrantee sql.NullString
	linkKind    sql.NullString
}

func (m *membershipRow) dest() []any {
	return []any{
		&m.id, &m.projectKind, &m.state, &m.admin, &m.superAdmin,
		&m.source, &m.profileType, &m.profileID, &m.linkGrantee, &m.linkKind,
	}
}

// profile returns the resource the member acts as. The schema pairs the two
// columns, so a half-set profile reads as none.
func (m *membershipRow) profile() *project.ProfileRef {
	if !m.profileType.Valid || !m.profileID.Valid {
		return nil
	}

	return &project.ProfileRef{
		Type: storage.ResourceType(m.profileType.String),
		ID:   storage.LogicalID(m.profileID.String),
	}
}

// membershipStatement compiles the lookup. Every principal kind adds the
// liveness predicate for the registry behind it, and a kind with no registry
// compiles nothing, so a fourth kind denies rather than inheriting standing
// gated by project_memberships.state alone.
func membershipStatement(proj project.ID, principal project.PrincipalRef) (string, []any, error) {
	var predicate string

	switch principal.Kind {
	case project.PrincipalUser:
		predicate = activeIdentityPredicate
	case project.PrincipalClientApplication:
		predicate = activeClientApplicationPredicate
	case project.PrincipalBot:
		predicate = activeBotPredicate
	default:
		return "", nil, fmt.Errorf("%w: %q", project.ErrInvalidPrincipal, string(principal.Kind))
	}

	// Every arm binds the request's Project a second time, so no predicate
	// compares one relation's Project to another relation's.
	args := []any{string(proj), string(principal.Kind), string(principal.ID), string(proj)}

	return membershipQuery + predicate + membershipOrder, args, nil
}

// Membership returns the principal's standing in one Project. An absent row is
// not found rather than a zero Membership, which a caller could misread as
// standing that happens to be unprivileged.
func (r *MembershipResolver) Membership(
	ctx context.Context, proj project.ID, principal project.PrincipalRef,
) (project.Membership, bool, error) {
	if err := project.ValidateID(proj); err != nil {
		return project.Membership{}, false, err
	}

	if !principal.Valid() {
		return project.Membership{}, false, fmt.Errorf("%w: %q", project.ErrInvalidPrincipal, string(principal.Kind))
	}

	text, args, err := membershipStatement(proj, principal)
	if err != nil {
		return project.Membership{}, false, err
	}

	var row membershipRow

	switch err := conn(ctx, r.db).QueryRowContext(ctx, text, args...).Scan(row.dest()...); {
	case errors.Is(err, sql.ErrNoRows):
		return project.Membership{}, false, nil
	case err != nil:
		return project.Membership{}, false, fmt.Errorf("pocketbase: read membership in %s: %w", proj, err)
	}

	if row.linkGrantee.Valid {
		return r.linkedMembership(ctx, proj, principal, row)
	}

	return r.directMembership(ctx, proj, principal, row)
}

// directMembership rebuilds standing the Project granted itself, bindings and
// all. The row's state travels through untouched, so a revoked or suspended
// membership rebuilds as one holding no standing.
func (r *MembershipResolver) directMembership(
	ctx context.Context, proj project.ID, principal project.PrincipalRef, row membershipRow,
) (project.Membership, bool, error) {
	policies, err := r.bindings(ctx, proj, project.MembershipID(row.id))
	if err != nil {
		return project.Membership{}, false, err
	}

	membership, err := project.NewMembership(project.MembershipConfig{
		ID:          project.MembershipID(row.id),
		Project:     proj,
		ProjectKind: project.Kind(row.projectKind),
		Principal:   principal,
		Profile:     row.profile(),
		State:       project.MembershipState(row.state),
		Admin:       row.admin == 1,
		SuperAdmin:  row.superAdmin == 1,
		Policies:    policies,
		Source:      project.MembershipSource(row.source),
	})
	if err != nil {
		return project.Membership{}, false, fmt.Errorf("pocketbase: rebuild membership %s in %s: %w", row.id, proj, err)
	}

	return membership, true, nil
}

// linkedMembership rebuilds standing an administrative link minted. It takes no
// privilege and no binding, and it reports itself link-sourced, which is what
// makes HoldsStanding false however healthy the link itself looks.
func (r *MembershipResolver) linkedMembership(
	ctx context.Context, proj project.ID, principal project.PrincipalRef, row membershipRow,
) (project.Membership, bool, error) {
	grantee := project.ID(row.linkGrantee.String)
	kind := project.LinkKind(row.linkKind.String)

	found, err := r.linkExists(ctx, proj, grantee, kind)
	if err != nil || !found {
		return project.Membership{}, false, err
	}

	via := linkIdentifier(grantee, proj, kind)

	membership, err := project.NewLinkedMembership(project.MembershipID(row.id), proj, principal, via)
	if err != nil {
		return project.Membership{}, false, fmt.Errorf("pocketbase: rebuild membership %s in %s: %w", row.id, proj, err)
	}

	return membership, true, nil
}

// linkExists reads the minting link's lifecycle from project_links at query
// time. A membership whose link is gone stands on nothing, and an unrecognised
// status denies rather than being read as the nearest known one.
func (r *MembershipResolver) linkExists(
	ctx context.Context, grantor, grantee project.ID, kind project.LinkKind,
) (bool, error) {
	var status string

	switch err := conn(ctx, r.db).QueryRowContext(ctx, linkStatusQuery,
		string(grantor), string(grantee), string(kind)).Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocketbase: read link %s into %s: %w", grantor, grantee, err)
	}

	if !project.LinkStatus(status).Valid() {
		return false, fmt.Errorf("%w: %s into %s holds %q", ErrUnknownLinkStatus, grantor, grantee, status)
	}

	return true, nil
}

// bindings reads one membership's policy attachments in evaluation order. Each
// resolves against the Project bound here, which is the only Project a binding
// can name.
func (r *MembershipResolver) bindings(
	ctx context.Context, proj project.ID, membership project.MembershipID,
) ([]project.PolicyAttachment, error) {
	rows, err := conn(ctx, r.db).QueryContext(ctx, bindingQuery, string(proj), string(membership))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read policy bindings of %s: %w", membership, err)
	}
	defer func() { _ = rows.Close() }()

	var attachments []project.PolicyAttachment

	for rows.Next() {
		var (
			policy     string
			ordinal    int64
			parameters string
		)

		if err := rows.Scan(&policy, &ordinal, &parameters); err != nil {
			return nil, fmt.Errorf("pocketbase: scan policy binding of %s: %w", membership, err)
		}

		attachments = append(attachments, project.PolicyAttachment{
			Policy:     storage.LogicalID(policy),
			Ordinal:    int(ordinal),
			Parameters: json.RawMessage(parameters),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read policy bindings of %s: %w", membership, err)
	}

	return attachments, nil
}

// ProjectResolver reads a Project's lifecycle state from projects.
type ProjectResolver struct {
	db *sql.DB
}

var _ authz.ProjectResolver = (*ProjectResolver)(nil)

// NewProjectResolver binds the resolver to an open database.
func NewProjectResolver(db *sql.DB) *ProjectResolver {
	return &ProjectResolver{db: db}
}

// State returns the Project's lifecycle position. A Project that names no row is
// an error, never a state: every value of project.State describes a Project that
// exists, so reporting one would read "never provisioned" as "merely suspended".
func (r *ProjectResolver) State(ctx context.Context, proj project.ID) (project.State, error) {
	if err := project.ValidateID(proj); err != nil {
		return "", err
	}

	var state string

	switch err := conn(ctx, r.db).QueryRowContext(ctx, projectStateQuery, string(proj)).Scan(&state); {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("pocketbase: read state of %s: %w: %w", proj, storage.ErrNotFound, err)
	case err != nil:
		return "", fmt.Errorf("pocketbase: read state of %s: %w", proj, err)
	}

	lifecycle := project.State(state)
	if !lifecycle.Valid() {
		return "", fmt.Errorf("%w: %s holds %q", ErrUnknownProjectState, proj, state)
	}

	return lifecycle, nil
}

// PolicyResolver loads an AccessPolicy from access_policies and its two child
// tables. It rebuilds through authz.NewAccessPolicy, the only place a rule is
// checked against the parameters its policy declares.
type PolicyResolver struct {
	db *sql.DB
}

var _ authz.PolicyResolver = (*PolicyResolver)(nil)

// NewPolicyResolver binds the resolver to an open database.
func NewPolicyResolver(db *sql.DB) *PolicyResolver {
	return &PolicyResolver{db: db}
}

// Policy returns the policy a reference names, keyed on the reference's own
// Project and id. A policy that no longer exists is not found rather than an
// error: nothing is what it says, and nothing is not permission.
func (r *PolicyResolver) Policy(
	ctx context.Context, ref project.PolicyRef,
) (authz.AccessPolicy, bool, error) {
	if ref.IsZero() {
		return authz.AccessPolicy{}, false, nil
	}

	owner, id := string(ref.Project()), string(ref.ID())

	var exists int

	switch err := conn(ctx, r.db).QueryRowContext(ctx, policyQuery, owner, id).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return authz.AccessPolicy{}, false, nil
	case err != nil:
		return authz.AccessPolicy{}, false, fmt.Errorf("pocketbase: read policy %s/%s: %w", owner, id, err)
	}

	parameters, err := r.parameters(ctx, ref)
	if err != nil {
		return authz.AccessPolicy{}, false, err
	}

	rules, err := r.rules(ctx, ref)
	if err != nil {
		return authz.AccessPolicy{}, false, err
	}

	policy, err := authz.NewAccessPolicy(authz.PolicyConfig{
		Project: ref.Project(), ID: ref.ID(), Parameters: parameters, Rules: rules,
	})
	if err != nil {
		return authz.AccessPolicy{}, false, fmt.Errorf("pocketbase: rebuild policy %s/%s: %w", owner, id, err)
	}

	return policy, true, nil
}

// parameters reads the names a binding must supply values for.
func (r *PolicyResolver) parameters(ctx context.Context, ref project.PolicyRef) ([]authz.ParameterName, error) {
	owner, id := string(ref.Project()), string(ref.ID())

	rows, err := conn(ctx, r.db).QueryContext(ctx, policyParameterQuery, owner, id)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read parameters of %s/%s: %w", owner, id, err)
	}
	defer func() { _ = rows.Close() }()

	var names []authz.ParameterName

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pocketbase: scan parameter of %s/%s: %w", owner, id, err)
		}

		names = append(names, authz.ParameterName(name))
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read parameters of %s/%s: %w", owner, id, err)
	}

	return names, nil
}

// ruleRow is one access_policy_rules row as SQLite hands it over.
type ruleRow struct {
	kind             string
	resourceType     string
	action           string
	unrestricted     int
	compartmentType  sql.NullString
	compartmentID    sql.NullString
	compartmentParam sql.NullString
}

func (r *ruleRow) dest() []any {
	return []any{
		&r.kind, &r.resourceType, &r.action, &r.unrestricted,
		&r.compartmentType, &r.compartmentID, &r.compartmentParam,
	}
}

// rules reads the policy's rules in evaluation order.
func (r *PolicyResolver) rules(ctx context.Context, ref project.PolicyRef) ([]authz.Rule, error) {
	owner, id := string(ref.Project()), string(ref.ID())

	rows, err := conn(ctx, r.db).QueryContext(ctx, policyRuleQuery, owner, id)
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read rules of %s/%s: %w", owner, id, err)
	}
	defer func() { _ = rows.Close() }()

	var rules []authz.Rule

	for rows.Next() {
		var row ruleRow

		if err := rows.Scan(row.dest()...); err != nil {
			return nil, fmt.Errorf("pocketbase: scan rule of %s/%s: %w", owner, id, err)
		}

		rule, err := buildRule(row)
		if err != nil {
			return nil, fmt.Errorf("pocketbase: rebuild rule of %s/%s: %w", owner, id, err)
		}

		rules = append(rules, rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read rules of %s/%s: %w", owner, id, err)
	}

	return rules, nil
}

// buildRule rebuilds one rule through the constructor matching its restriction.
// The three shapes are the ones the row's own CHECK allows; a fourth is drift,
// and it fails rather than resolving to the widest of the three.
func buildRule(row ruleRow) (authz.Rule, error) {
	kind := storage.Kind(row.kind)
	resourceType := storage.ResourceType(row.resourceType)
	action := storage.Action(row.action)
	subjectType := storage.ResourceType(row.compartmentType.String)

	switch {
	case row.unrestricted == 1:
		return authz.NewUnrestrictedRule(kind, resourceType, action)
	case row.compartmentID.Valid:
		subject, err := authz.LiteralSubject(subjectType, storage.LogicalID(row.compartmentID.String))
		if err != nil {
			return authz.Rule{}, err
		}

		return authz.NewRule(kind, resourceType, action, subject)
	case row.compartmentParam.Valid:
		subject, err := authz.ParameterSubject(subjectType, authz.ParameterName(row.compartmentParam.String))
		if err != nil {
			return authz.Rule{}, err
		}

		return authz.NewRule(kind, resourceType, action, subject)
	default:
		return authz.Rule{}, fmt.Errorf("%w: %s %s", ErrUnreadableRule, row.resourceType, row.action)
	}
}
