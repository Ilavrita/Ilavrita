package project

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// MembershipID identifies one principal's standing in one Project.
type MembershipID string

// PrincipalID is the id of the user, client application or bot a membership
// belongs to.
type PrincipalID string

// PrincipalKind names which family of principal holds a membership.
type PrincipalKind string

// The kinds of principal a membership can belong to.
const (
	PrincipalUser              PrincipalKind = "user"
	PrincipalClientApplication PrincipalKind = "client_application"
	PrincipalBot               PrincipalKind = "bot"
)

// Valid reports whether the principal kind is one this server recognises.
func (k PrincipalKind) Valid() bool {
	switch k {
	case PrincipalUser, PrincipalClientApplication, PrincipalBot:
		return true
	default:
		return false
	}
}

// PrincipalRef names the one principal behind a membership. One principal, so a
// membership can never resolve to two identities.
type PrincipalRef struct {
	Kind PrincipalKind
	ID   PrincipalID
}

// Valid reports whether the reference names a recognised principal.
func (p PrincipalRef) Valid() bool {
	return p.Kind.Valid() && p.ID != ""
}

// ProfileRef points at the FHIR resource a member acts as. It carries no
// Project: a profile resolves inside the membership's own Project, so a
// cross-project profile is not expressible.
type ProfileRef struct {
	Type storage.ResourceType
	ID   storage.LogicalID
}

// Valid reports whether the reference names a resource.
func (r ProfileRef) Valid() bool {
	return r.Type != "" && r.ID != ""
}

// PolicyRef names an AccessPolicy inside one Project. Its fields are unexported
// and set only from the Project that owns the policy, so a restriction can never
// be resolved against the project doing the reaching.
type PolicyRef struct {
	project ID
	id      storage.LogicalID
}

// Project returns the Project that owns the policy.
func (r PolicyRef) Project() ID {
	return r.project
}

// ID returns the policy's logical id.
func (r PolicyRef) ID() storage.LogicalID {
	return r.id
}

// IsZero reports whether the reference names no policy.
func (r PolicyRef) IsZero() bool {
	return r.project == "" || r.id == ""
}

// PolicyAttachment asks for a policy binding. It names no Project, because a
// binding always resolves in the Project that owns the membership.
type PolicyAttachment struct {
	Policy     storage.LogicalID
	Ordinal    int
	Parameters json.RawMessage
}

// PolicyBinding is a bound AccessPolicy on a membership, already resolved
// against the membership's own Project.
type PolicyBinding struct {
	policy     PolicyRef
	ordinal    int
	parameters json.RawMessage
}

// Policy returns the bound policy.
func (b PolicyBinding) Policy() PolicyRef {
	return b.policy
}

// Ordinal returns the binding's evaluation order.
func (b PolicyBinding) Ordinal() int {
	return b.ordinal
}

// Parameters returns a copy of the binding's parameters.
func (b PolicyBinding) Parameters() json.RawMessage {
	return slices.Clone(b.parameters)
}

// MembershipState is a membership's lifecycle position.
type MembershipState string

// The lifecycle states of a membership.
const (
	MembershipInvited   MembershipState = "invited"
	MembershipActive    MembershipState = "active"
	MembershipSuspended MembershipState = "suspended"
	MembershipRevoked   MembershipState = "revoked"
)

// Valid reports whether the state is one this server recognises.
func (s MembershipState) Valid() bool {
	switch s {
	case MembershipInvited, MembershipActive, MembershipSuspended, MembershipRevoked:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. Revoked is
// terminal; a returning member gets a new membership, not a revived one.
func (s MembershipState) CanTransitionTo(next MembershipState) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case MembershipInvited:
		return next == MembershipActive || next == MembershipRevoked
	case MembershipActive:
		return next == MembershipSuspended || next == MembershipRevoked
	case MembershipSuspended:
		return next == MembershipActive || next == MembershipRevoked
	default:
		return false
	}
}

// TransitionTo returns the next state, or refuses the move.
func (s MembershipState) TransitionTo(next MembershipState) (MembershipState, error) {
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}

	if !s.CanTransitionTo(next) {
		return "", fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s, next)
	}

	return next, nil
}

// MembershipSource records how a membership came to exist, so an audit can tell
// a bootstrap claim from an invitation.
type MembershipSource string

// The ways a membership can come to exist.
const (
	SourceBootstrap MembershipSource = "bootstrap"
	SourceInvite    MembershipSource = "invite"
	SourceIdPJIT    MembershipSource = "idp_jit"
	SourceSCIM      MembershipSource = "scim"
	SourceAPI       MembershipSource = "api"
	SourceMigration MembershipSource = "migration"
	SourceLink      MembershipSource = "link"
)

// Valid reports whether the source is one this server recognises.
func (s MembershipSource) Valid() bool {
	switch s {
	case SourceBootstrap, SourceInvite, SourceIdPJIT, SourceSCIM, SourceAPI, SourceMigration, SourceLink:
		return true
	default:
		return false
	}
}

// Membership is one principal's standing in one Project. Every field is
// unexported, so no package can write a privileged membership as a literal and
// Resolve stays the only way privilege enters the system.
type Membership struct {
	id          MembershipID
	project     ID
	projectKind Kind
	principal   PrincipalRef
	profile     *ProfileRef
	state       MembershipState
	admin       bool
	superAdmin  bool
	policies    []PolicyBinding
	source      MembershipSource
	viaLink     LinkID
}

// MembershipConfig is the input to NewMembership. Source may not be SourceLink:
// link-sourced standing is minted only by NewLinkedMembership, whose signature
// cannot express a privilege.
type MembershipConfig struct {
	ID          MembershipID
	Project     ID
	ProjectKind Kind
	Principal   PrincipalRef
	Profile     *ProfileRef
	State       MembershipState
	Admin       bool
	SuperAdmin  bool
	Policies    []PolicyAttachment
	Source      MembershipSource
}

// NewMembership builds a membership held directly in a Project. Super admin is
// refused outside the Super Project, mirroring the database constraint rather
// than trusting a caller to have checked it.
func NewMembership(cfg MembershipConfig) (Membership, error) {
	if cfg.ID == "" {
		return Membership{}, fmt.Errorf("%w: membership", ErrMissingID)
	}

	if err := ValidateID(cfg.Project); err != nil {
		return Membership{}, err
	}

	if !cfg.ProjectKind.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(cfg.ProjectKind))
	}

	if !cfg.Principal.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrInvalidPrincipal, string(cfg.Principal.Kind))
	}

	if cfg.Profile != nil && !cfg.Profile.Valid() {
		return Membership{}, ErrInvalidProfile
	}

	if !cfg.State.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	if !cfg.Source.Valid() || cfg.Source == SourceLink {
		return Membership{}, fmt.Errorf("%w: %q", ErrInvalidSource, string(cfg.Source))
	}

	if cfg.SuperAdmin && !cfg.ProjectKind.AllowsSuperAdmin() {
		return Membership{}, fmt.Errorf("%w: %s", ErrSuperAdminOutsideSuperProject, cfg.Project)
	}

	if err := confineMachinePrincipal(cfg); err != nil {
		return Membership{}, err
	}

	policies, err := bindPolicies(cfg.Project, cfg.Policies)
	if err != nil {
		return Membership{}, err
	}

	return Membership{
		id: cfg.ID, project: cfg.Project, projectKind: cfg.ProjectKind,
		principal: cfg.Principal, profile: cfg.Profile, state: cfg.State,
		admin: cfg.Admin, superAdmin: cfg.SuperAdmin,
		policies: policies, source: cfg.Source,
	}, nil
}

// NewLinkedMembership mints the standing an administrative link confers. It
// takes no privilege argument at all: a link-minted membership that could hold
// admin or a policy binding is one write away from a data grant (FR-052).
func NewLinkedMembership(id MembershipID, project ID, principal PrincipalRef, via LinkID) (Membership, error) {
	if id == "" {
		return Membership{}, fmt.Errorf("%w: membership", ErrMissingID)
	}

	if via == "" {
		return Membership{}, fmt.Errorf("%w: link", ErrMissingID)
	}

	if err := ValidateID(project); err != nil {
		return Membership{}, err
	}

	if !principal.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrInvalidPrincipal, string(principal.Kind))
	}

	return Membership{
		id: id, project: project, projectKind: KindStandard,
		principal: principal, state: MembershipActive,
		source: SourceLink, viaLink: via,
	}, nil
}

// confineMachinePrincipal refuses the standing a machine may not hold, mirroring
// the three CHECKs project_memberships carries: administering the install is
// answerable work, and a compartment profile is authority no policy granted.
func confineMachinePrincipal(cfg MembershipConfig) error {
	if cfg.SuperAdmin && cfg.Principal.Kind != PrincipalUser {
		return fmt.Errorf("%w: %q", ErrMachinePrincipalPrivilege, string(cfg.Principal.Kind))
	}

	if cfg.Admin && cfg.Principal.Kind == PrincipalBot {
		return fmt.Errorf("%w: %s", ErrBotPrivilege, cfg.Principal.ID)
	}

	if cfg.Profile != nil && cfg.Principal.Kind != PrincipalUser {
		return fmt.Errorf("%w: %q", ErrMachinePrincipalProfile, string(cfg.Principal.Kind))
	}

	return nil
}

// bindPolicies resolves every attachment against the owning Project, which is
// the only Project a binding can name.
func bindPolicies(owner ID, attachments []PolicyAttachment) ([]PolicyBinding, error) {
	bindings := make([]PolicyBinding, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Policy == "" {
			return nil, ErrMissingPolicy
		}

		bindings = append(bindings, PolicyBinding{
			policy:     PolicyRef{project: owner, id: attachment.Policy},
			ordinal:    attachment.Ordinal,
			parameters: slices.Clone(attachment.Parameters),
		})
	}

	return bindings, nil
}

// ID returns the membership's identifier.
func (m Membership) ID() MembershipID {
	return m.id
}

// Project returns the Project this membership stands in.
func (m Membership) Project() ID {
	return m.project
}

// ProjectKind returns the kind of the Project this membership stands in.
func (m Membership) ProjectKind() Kind {
	return m.projectKind
}

// Principal returns the principal holding this membership.
func (m Membership) Principal() PrincipalRef {
	return m.principal
}

// Profile returns the resource the member acts as, if one is set.
func (m Membership) Profile() (ProfileRef, bool) {
	if m.profile == nil {
		return ProfileRef{}, false
	}

	return *m.profile, true
}

// State returns the membership's lifecycle position.
func (m Membership) State() MembershipState {
	return m.state
}

// Source returns how the membership came to exist.
func (m Membership) Source() MembershipSource {
	return m.source
}

// ViaLink returns the administrative link that minted this membership, if any.
func (m Membership) ViaLink() (LinkID, bool) {
	return m.viaLink, m.viaLink != ""
}

// HoldsStanding reports whether this is the principal's own live standing in the
// Project: active, and held directly rather than minted by an administrative
// link. Nothing the Project confers travels to a membership without it.
func (m Membership) HoldsStanding() bool {
	return m.state == MembershipActive && m.viaLink == ""
}

// IsAdmin reports effective project-admin standing. An invited, suspended or
// revoked membership holds none, and neither does one a link minted.
func (m Membership) IsAdmin() bool {
	return m.admin && m.HoldsStanding()
}

// IsSuperAdmin reports effective server-wide standing, which needs an active
// direct membership in the Super Project.
func (m Membership) IsSuperAdmin() bool {
	return m.superAdmin && m.HoldsStanding() && m.projectKind.AllowsSuperAdmin()
}

// Policies returns the data-plane bindings this membership resolves to. A
// membership holding no standing resolves to none, which is an empty Scope on
// every FHIR route.
func (m Membership) Policies() []PolicyBinding {
	if !m.HoldsStanding() {
		return nil
	}

	return slices.Clone(m.policies)
}

// StoredAdmin returns the admin column's value rather than effective standing.
// A store persists what the row holds; IsAdmin is what an authorization decision
// reads, and the two differ for an invited, suspended or link-minted membership.
func (m Membership) StoredAdmin() bool {
	return m.admin
}

// StoredSuperAdmin returns the super_admin column's value, for the same reason.
func (m Membership) StoredSuperAdmin() bool {
	return m.superAdmin
}

// StoredPolicies returns every binding the row holds, including those a
// membership holding no standing resolves to nothing. Policies is what a
// decision reads; this is what a store writes.
func (m Membership) StoredPolicies() []PolicyBinding {
	return slices.Clone(m.policies)
}

// TransitionTo returns the membership in its next state.
func (m Membership) TransitionTo(next MembershipState) (Membership, error) {
	state, err := m.state.TransitionTo(next)
	if err != nil {
		return Membership{}, err
	}

	m.state = state

	return m, nil
}
