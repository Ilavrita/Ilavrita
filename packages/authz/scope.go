package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrInvalidPrincipal reports a request naming no recognised principal. A
	// malformed call is refused outright, never answered with an empty Scope a
	// caller could mistake for a decision.
	ErrInvalidPrincipal = errors.New("authz: request needs one named principal")

	// ErrMissingResolver reports a request with a port left nil. A nil port is a
	// wiring mistake, not a step that legitimately resolved to nothing.
	ErrMissingResolver = errors.New("authz: request needs every resolver")

	// ErrMissingInstant reports a request carrying no decision instant. Link
	// lifecycle is judged against it, so a zero instant is refused rather than
	// silently read as the epoch.
	ErrMissingInstant = errors.New("authz: request needs the instant it is decided at")

	// ErrPolicyMismatch reports a resolver answering with a policy other than the
	// one the reference names, which is a resolver bug and never a widening.
	ErrPolicyMismatch = errors.New("authz: resolver returned a policy the reference does not name")
)

// linkActions are the only actions a link may confer, mirroring the
// project_link_types CHECK: cross-Project write and delete are unrepresentable
// rather than merely refused.
var linkActions = []storage.Action{storage.ActionRead, storage.ActionSearch, storage.ActionHistory}

// MembershipResolver finds one principal's standing in one Project, in whatever
// lifecycle state it currently holds. Membership's own accessors already gate on
// state and on link provenance, so this port must not pre-filter.
type MembershipResolver interface {
	Membership(ctx context.Context, proj project.ID, principal project.PrincipalRef) (project.Membership, bool, error)
}

// ProjectResolver reads a Project's current lifecycle State, which is a
// precondition of admission rather than a predicate applied after fetching.
type ProjectResolver interface {
	State(ctx context.Context, proj project.ID) (project.State, error)
}

// PolicyResolver loads the AccessPolicy a reference names. It answers with the
// policy and never with Grants, so no implementation can hand back a Grant for a
// triple nobody asked about.
type PolicyResolver interface {
	Policy(ctx context.Context, ref project.PolicyRef) (AccessPolicy, bool, error)
}

// Resolvers are the ports one decision reads. Every one is required: a missing
// port denies the call outright instead of quietly contributing nothing.
type Resolvers struct {
	Memberships MembershipResolver
	Projects    ProjectResolver
	Policies    PolicyResolver
	Links       project.LinkResolver
}

// Request names one authorization decision. Kind, Type and Action are required
// because a Scope answers this exact triple, never everything a principal might
// be allowed to do.
type Request struct {
	Principal project.PrincipalRef

	// Project is the home Project: where the principal's own standing lives.
	Project project.ID

	// LinkedProjects are the grantor Projects the caller explicitly opted into.
	// An effective link nobody named contributes nothing (LNK-8).
	LinkedProjects []project.ID

	Kind   storage.Kind
	Type   storage.ResourceType
	Action storage.Action

	// Now is the instant the decision is made at, against which link activation
	// and expiry are judged.
	Now time.Time

	// Launch is what the asking session was launched with, and it is required:
	// NoLaunch for a request no app is behind, ParseLaunch for a session's own
	// context. Leaving it alone denies the call rather than skipping the
	// narrowing, because skipping the narrowing is what widens.
	Launch Launch

	Resolvers Resolvers
}

// BuildScope resolves one request through principal, membership, Project state,
// capability, policy and links, returning the Grants that survive every step.
// Anything unknown, missing or failing yields an empty Scope, never a partial one.
func BuildScope(ctx context.Context, req Request) (storage.Scope, error) {
	if err := req.validate(); err != nil {
		return storage.Scope{}, err
	}

	membership, found, err := req.Resolvers.Memberships.Membership(ctx, req.Project, req.Principal)
	if err != nil {
		return storage.Scope{}, fmt.Errorf("authz: resolve membership in %s: %w", req.Project, err)
	}

	if !found || !stands(membership, req) {
		return storage.Scope{}, nil
	}

	admitted, err := admits(ctx, req.Resolvers.Projects, req.Project, req.Action)
	if err != nil {
		return storage.Scope{}, err
	}

	if !admitted {
		return storage.Scope{}, nil
	}

	held, err := membershipGrants(ctx, req, membership)
	if err != nil {
		return storage.Scope{}, err
	}

	reached, err := linkGrants(ctx, req)
	if err != nil {
		return storage.Scope{}, err
	}

	// The narrowing is applied here rather than by the caller, because this is
	// the only function that produces a Scope: an app's request cannot reach
	// storage through a path that forgot to restrict it.
	return req.Launch.Narrow(storage.NewScope(append(held, reached...)...)), nil
}

// validate refuses a malformed call. These are the caller's mistakes, kept
// distinct from the ordinary answer that a well-formed request authorizes nothing.
func (r Request) validate() error {
	if !r.Principal.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidPrincipal, string(r.Principal.Kind))
	}

	if err := project.ValidateID(r.Project); err != nil {
		return err
	}

	for _, grantor := range r.LinkedProjects {
		if err := project.ValidateID(grantor); err != nil {
			return err
		}
	}

	if err := validateTriple(r.Kind, r.Type, r.Action); err != nil {
		return err
	}

	if r.Now.IsZero() {
		return ErrMissingInstant
	}

	if !r.Launch.stated {
		return ErrMissingLaunch
	}

	return r.Resolvers.validate()
}

// validate refuses a port left nil, which would otherwise read as a step that
// found nothing rather than one that was never wired.
func (r Resolvers) validate() error {
	if r.Memberships == nil || r.Projects == nil || r.Policies == nil || r.Links == nil {
		return ErrMissingResolver
	}

	return nil
}

// stands reports whether a membership is standing a decision may rest on: the
// one the request named, and live. Both grant steps sit behind it, because the
// link step reads no accessor that gates on standing itself.
func stands(membership project.Membership, req Request) bool {
	return membership.Project() == req.Project &&
		membership.Principal() == req.Principal &&
		membership.HoldsStanding()
}

// admits reports whether a Project's lifecycle permits the action at all. An
// unrecognised state admits nothing, because State.Admits has no permissive
// default to fall through to.
func admits(ctx context.Context, projects ProjectResolver, proj project.ID, action storage.Action) (bool, error) {
	state, err := projects.State(ctx, proj)
	if err != nil {
		return false, fmt.Errorf("authz: resolve state of %s: %w", proj, err)
	}

	return state.Admits(action), nil
}

// dataCapability is the capability step. A data decision reads policy bindings
// and nothing else, and Membership exposes no accessor that turns administrative
// standing into one, which is how FR-052 holds structurally (see the guard test).
func dataCapability(membership project.Membership) []project.PolicyBinding {
	return membership.Policies()
}

// membershipGrants compiles what the principal's own standing allows. Every
// Grant it can mint is pinned to the home Project, because a binding's policy
// reference names no other Project by construction.
func membershipGrants(ctx context.Context, req Request, membership project.Membership) ([]storage.Grant, error) {
	var grants []storage.Grant

	for _, binding := range dataCapability(membership) {
		params, err := ParseParameters(binding.Parameters())
		if err != nil {
			return nil, err
		}

		compiled, err := compile(ctx, req.Resolvers.Policies, binding.Policy(), GrantRequest{
			Project: req.Project, Kind: req.Kind, Type: req.Type, Action: req.Action,
			Origin: OriginMembership, Parameters: params,
		})
		if err != nil {
			return nil, err
		}

		grants = append(grants, compiled...)
	}

	return grants, nil
}

// linkGrants resolves the one step whose Grants can name another Project. It
// asks the resolver once, for the home Project only, so a grantor's own inbound
// links are never consulted and A to B to C cannot compose.
func linkGrants(ctx context.Context, req Request) ([]storage.Grant, error) {
	grantors := requestedGrantors(req)
	if len(grantors) == 0 || !slices.Contains(linkActions, req.Action) {
		return nil, nil
	}

	inbound, err := req.Resolvers.Links.Inbound(ctx, req.Project)
	if err != nil {
		return nil, fmt.Errorf("authz: resolve links into %s: %w", req.Project, err)
	}

	var grants []storage.Grant

	for _, grantor := range grantors {
		reached, err := grantorGrants(ctx, req, inbound, grantor)
		if err != nil {
			return nil, err
		}

		grants = append(grants, reached...)
	}

	return grants, nil
}

// requestedGrantors is the opt-in list, deduplicated and without the home
// Project, which no link may name as its own grantor.
func requestedGrantors(req Request) []project.ID {
	named := make([]project.ID, 0, len(req.LinkedProjects))
	for _, grantor := range req.LinkedProjects {
		if grantor != req.Project && !slices.Contains(named, grantor) {
			named = append(named, grantor)
		}
	}

	return named
}

// grantorGrants compiles what one named grantor's links allow. Every gate is
// read from the link itself at this instant, so a revoked, suspended or expired
// link grants nothing without anything else having to notice it changed.
func grantorGrants(
	ctx context.Context, req Request, inbound []project.Link, grantor project.ID,
) ([]storage.Grant, error) {
	admitted, err := admits(ctx, req.Resolvers.Projects, grantor, req.Action)
	if err != nil {
		return nil, err
	}

	if !admitted {
		return nil, nil
	}

	var grants []storage.Grant

	for _, link := range inbound {
		share, ok := reaches(link, req, grantor)
		if !ok {
			continue
		}

		compiled, err := compile(ctx, req.Resolvers.Policies, share.Policy(), GrantRequest{
			Project: grantor, Kind: req.Kind, Type: req.Type, Action: req.Action,
			Origin: OriginLink, Parameters: Parameters{},
		})
		if err != nil {
			return nil, err
		}

		grants = append(grants, compiled...)
	}

	return grants, nil
}

// reaches reports what one link shares with this request, if anything. Both ends
// are re-checked against the request, so a resolver answering with another
// Project's link contributes nothing.
func reaches(link project.Link, req Request, grantor project.ID) (project.DataShare, bool) {
	if link.Grantee() != req.Project || link.Grantor() != grantor {
		return project.DataShare{}, false
	}

	share, ok := link.Share(req.Now)
	if !ok || !share.Covers(req.Type) {
		return project.DataShare{}, false
	}

	return share, true
}

// compile loads the policy a reference names and resolves it for one request. A
// reference resolving to nothing contributes nothing: a policy that no longer
// exists says nothing, and nothing is not permission.
func compile(
	ctx context.Context, policies PolicyResolver, ref project.PolicyRef, req GrantRequest,
) ([]storage.Grant, error) {
	policy, found, err := policies.Policy(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("authz: resolve policy %s/%s: %w", ref.Project(), ref.ID(), err)
	}

	if !found {
		return nil, nil
	}

	if !policy.Matches(ref) {
		return nil, fmt.Errorf("%w: %s/%s", ErrPolicyMismatch, ref.Project(), ref.ID())
	}

	return policy.Compile(req)
}
