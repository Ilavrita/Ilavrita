package project

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// LinkID identifies one directed link between two Projects.
type LinkID string

// LinkKind separates data sharing from control-plane delegation. A link is one
// or the other, and its kind is read from its payload rather than stored beside
// it, so the two can never disagree.
type LinkKind string

// The kinds of link.
const (
	LinkKindData           LinkKind = "data"
	LinkKindAdministrative LinkKind = "administrative"
)

// LinkStatus is a link's lifecycle position.
type LinkStatus string

// The lifecycle states of a link.
const (
	LinkProposed  LinkStatus = "proposed"
	LinkActive    LinkStatus = "active"
	LinkSuspended LinkStatus = "suspended"
	LinkRevoked   LinkStatus = "revoked"
)

// Valid reports whether the status is one this server recognises.
func (s LinkStatus) Valid() bool {
	switch s {
	case LinkProposed, LinkActive, LinkSuspended, LinkRevoked:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. Revoked is
// terminal: renewing a revoked link is a new proposal both sides approve again.
func (s LinkStatus) CanTransitionTo(next LinkStatus) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case LinkProposed:
		return next == LinkActive || next == LinkRevoked
	case LinkActive:
		return next == LinkSuspended || next == LinkRevoked
	case LinkSuspended:
		return next == LinkActive || next == LinkRevoked
	default:
		return false
	}
}

// AdminCapability is one control-plane power over a Project.
type AdminCapability string

// Capabilities an administrative link may confer. The set is closed: anything
// absent is refused when the link is built.
const (
	CapabilityQuotaWrite     AdminCapability = "project.quota.write"
	CapabilityLifecycleWrite AdminCapability = "project.lifecycle.write"
	CapabilitySettingsWrite  AdminCapability = "project.settings.write"
	CapabilityMembershipRead AdminCapability = "project.membership.read"
)

// Capabilities no link may ever confer. Each is one membership, policy or
// identity write away from a data grant, which is the escalation FR-052 forbids.
const (
	CapabilityMembershipWrite  AdminCapability = "project.membership.write"
	CapabilityMembershipAdmin  AdminCapability = "project.membership.admin"
	CapabilityPolicyWrite      AdminCapability = "project.policy.write"
	CapabilitySecretWrite      AdminCapability = "project.secret.write"
	CapabilityAuthPolicyWrite  AdminCapability = "project.authentication_policy.write"
	CapabilityIdentityProvider AdminCapability = "project.identity_provider.write"
)

// LinkConferrable reports whether an administrative link may carry this
// capability. It is an allowlist, so an unknown capability is refused too.
func (c AdminCapability) LinkConferrable() bool {
	switch c {
	case CapabilityQuotaWrite, CapabilityLifecycleWrite, CapabilitySettingsWrite, CapabilityMembershipRead:
		return true
	default:
		return false
	}
}

// linkGrant is what a link confers. The interface is sealed to this package and
// has exactly two implementations, so no link can carry both a data scope and a
// control-plane capability.
type linkGrant interface {
	linkKind() LinkKind
}

// DataShare is what a data link confers: a closed list of resource types and the
// grantor's AccessPolicy. It holds no capability and exposes no method that
// could produce one.
type DataShare struct {
	types  []storage.ResourceType
	policy PolicyRef
}

func (DataShare) linkKind() LinkKind {
	return LinkKindData
}

// ResourceTypes returns the closed list this share covers. The authorizer mints
// one Grant per type, so a type absent here produces no Grant at all.
func (s DataShare) ResourceTypes() []storage.ResourceType {
	return slices.Clone(s.types)
}

// Covers reports whether the share names this resource type.
func (s DataShare) Covers(resourceType storage.ResourceType) bool {
	return slices.Contains(s.types, resourceType)
}

// Policy returns the restriction, always owned by the grantor Project.
func (s DataShare) Policy() PolicyRef {
	return s.policy
}

// AdminDelegation is what an administrative link confers: control-plane powers
// over named principals. It holds no resource type and no policy reference, so
// administrative reach has no path to a data scope (FR-052).
type AdminDelegation struct {
	capabilities []AdminCapability
	principals   []PrincipalRef
}

func (AdminDelegation) linkKind() LinkKind {
	return LinkKindAdministrative
}

// Capabilities returns the delegated powers.
func (d AdminDelegation) Capabilities() []AdminCapability {
	return slices.Clone(d.capabilities)
}

// Principals returns the named holders. A delegation never names a role, whose
// roster that same role could then rewrite.
func (d AdminDelegation) Principals() []PrincipalRef {
	return slices.Clone(d.principals)
}

// Allows reports whether this delegation gives one named principal one
// capability. The allowlist is re-checked here, not only at construction.
func (d AdminDelegation) Allows(capability AdminCapability, principal PrincipalRef) bool {
	return capability.LinkConferrable() &&
		slices.Contains(d.capabilities, capability) &&
		slices.Contains(d.principals, principal)
}

// Approvals records the two-sided consent a link needs. Without the grantee's,
// a grantor can push its records into the grantee's searches, exports and Bots.
type Approvals struct {
	grantorBy MembershipID
	grantorAt time.Time
	granteeBy MembershipID
	granteeAt time.Time
}

// Complete reports whether both sides have approved.
func (a Approvals) Complete() bool {
	return a.grantorBy != "" && !a.grantorAt.IsZero() &&
		a.granteeBy != "" && !a.granteeAt.IsZero()
}

// Grantor returns who approved on the grantor's side, and when.
func (a Approvals) Grantor() (MembershipID, time.Time) {
	return a.grantorBy, a.grantorAt
}

// Grantee returns who approved on the grantee's side, and when.
func (a Approvals) Grantee() (MembershipID, time.Time) {
	return a.granteeBy, a.granteeAt
}

// Link is one directed grant from a grantor Project to a grantee Project. Its
// fields are unexported and its payload is fixed by the constructor that matches
// its kind, so no later write can add the other kind's payload.
type Link struct {
	id          LinkID
	grantor     ID
	grantee     ID
	grant       linkGrant
	status      LinkStatus
	approvals   Approvals
	activatedAt time.Time
	historyFrom time.Time
	expiresAt   *time.Time
}

// NewDataLink proposes a data link. The policy is bound to the grantor by
// construction, so a grantee cannot lower the restriction through a same-named
// policy of its own.
func NewDataLink(id LinkID, grantor, grantee ID, policy storage.LogicalID, types ...storage.ResourceType) (Link, error) {
	if err := validateEnds(id, grantor, grantee); err != nil {
		return Link{}, err
	}

	if policy == "" {
		return Link{}, ErrMissingPolicy
	}

	if len(types) == 0 {
		return Link{}, ErrMissingResourceType
	}

	for _, resourceType := range types {
		if resourceType == "" {
			return Link{}, ErrMissingResourceType
		}
	}

	share := DataShare{
		types:  slices.Clone(types),
		policy: PolicyRef{project: grantor, id: policy},
	}

	return Link{id: id, grantor: grantor, grantee: grantee, grant: share, status: LinkProposed}, nil
}

// NewAdminLink proposes an administrative link. Every capability is checked
// against the link-conferrable allowlist and every holder is named, so the link
// can neither write a membership nor target an ambient role.
func NewAdminLink(id LinkID, grantor, grantee ID, principals []PrincipalRef, capabilities ...AdminCapability) (Link, error) {
	if err := validateEnds(id, grantor, grantee); err != nil {
		return Link{}, err
	}

	if len(capabilities) == 0 {
		return Link{}, ErrMissingCapability
	}

	for _, capability := range capabilities {
		if !capability.LinkConferrable() {
			return Link{}, fmt.Errorf("%w: %q", ErrCapabilityNotLinkConferrable, string(capability))
		}
	}

	if len(principals) == 0 {
		return Link{}, ErrUnnamedPrincipal
	}

	for _, principal := range principals {
		if !principal.Valid() {
			return Link{}, fmt.Errorf("%w: %q", ErrInvalidPrincipal, string(principal.Kind))
		}
	}

	delegation := AdminDelegation{
		capabilities: slices.Clone(capabilities),
		principals:   slices.Clone(principals),
	}

	return Link{id: id, grantor: grantor, grantee: grantee, grant: delegation, status: LinkProposed}, nil
}

// validateEnds rejects a link that names no id, an invalid Project, or the same
// Project twice.
func validateEnds(id LinkID, grantor, grantee ID) error {
	if id == "" {
		return fmt.Errorf("%w: link", ErrMissingID)
	}

	if err := ValidateID(grantor); err != nil {
		return err
	}

	if err := ValidateID(grantee); err != nil {
		return err
	}

	if grantor == grantee {
		return fmt.Errorf("%w: %s", ErrSelfLink, grantor)
	}

	return nil
}

// ID returns the link's identifier.
func (l Link) ID() LinkID {
	return l.id
}

// Grantor returns the Project being reached into.
func (l Link) Grantor() ID {
	return l.grantor
}

// Grantee returns the Project doing the reaching.
func (l Link) Grantee() ID {
	return l.grantee
}

// Kind reports what the link carries, read from the payload itself so it cannot
// disagree with it.
func (l Link) Kind() LinkKind {
	if l.grant == nil {
		return ""
	}

	return l.grant.linkKind()
}

// Status returns the link's lifecycle position.
func (l Link) Status() LinkStatus {
	return l.status
}

// Approvals returns the recorded consent of both sides.
func (l Link) Approvals() Approvals {
	return l.approvals
}

// ActivatedAt returns when the link first became active.
func (l Link) ActivatedAt() time.Time {
	return l.activatedAt
}

// HistoryFrom returns the floor on how far back a grantee may read, defaulting
// to the moment the link activated.
func (l Link) HistoryFrom() time.Time {
	return l.historyFrom
}

// ExpiresAt returns when the link stops authorizing, if it ever does.
func (l Link) ExpiresAt() (time.Time, bool) {
	if l.expiresAt == nil {
		return time.Time{}, false
	}

	return *l.expiresAt, true
}

// ApproveGrantor records the grantor's consent.
func (l Link) ApproveGrantor(by MembershipID, at time.Time) (Link, error) {
	if by == "" {
		return Link{}, fmt.Errorf("%w: approving membership", ErrMissingID)
	}

	l.approvals.grantorBy, l.approvals.grantorAt = by, at

	return l, nil
}

// ApproveGrantee records the grantee's consent, which is what stops a grantor
// pushing records into a grantee's results unasked.
func (l Link) ApproveGrantee(by MembershipID, at time.Time) (Link, error) {
	if by == "" {
		return Link{}, fmt.Errorf("%w: approving membership", ErrMissingID)
	}

	l.approvals.granteeBy, l.approvals.granteeAt = by, at

	return l, nil
}

// TransitionTo moves the link's status. Becoming active needs both approvals,
// and the first activation fixes the activation time and the history floor.
func (l Link) TransitionTo(next LinkStatus, at time.Time) (Link, error) {
	if !l.status.Valid() {
		return Link{}, fmt.Errorf("%w: %q", ErrUnknownState, string(l.status))
	}

	if !l.status.CanTransitionTo(next) {
		return Link{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, l.status, next)
	}

	if next == LinkActive {
		if !l.approvals.Complete() {
			return Link{}, fmt.Errorf("%w: %s", ErrApprovalIncomplete, l.id)
		}

		if l.activatedAt.IsZero() {
			l.activatedAt = at
			l.historyFrom = at
		}
	}

	l.status = next

	return l, nil
}

// WithExpiry sets when the link stops authorizing. An expired link authorizes
// nothing without any status change, so nobody has to remember to revoke it.
func (l Link) WithExpiry(at time.Time) Link {
	l.expiresAt = &at

	return l
}

// WithHistoryFrom moves the floor on how far back a grantee may read, which is
// the grantor's explicit opt-in to versions predating the link.
func (l Link) WithHistoryFrom(from time.Time) Link {
	l.historyFrom = from

	return l
}

// Effective reports whether the link authorizes anything at this instant. A
// proposed, suspended, revoked, expired or half-approved link authorizes
// nothing.
func (l Link) Effective(now time.Time) bool {
	if l.status != LinkActive || !l.approvals.Complete() {
		return false
	}

	if l.activatedAt.IsZero() || now.Before(l.activatedAt) {
		return false
	}

	return l.expiresAt == nil || now.Before(*l.expiresAt)
}

// Share returns what a data link shares. An administrative link and a link that
// is not effective both return false, so no administrative standing reaches a
// resource type (FR-052).
func (l Link) Share(now time.Time) (DataShare, bool) {
	if !l.Effective(now) {
		return DataShare{}, false
	}

	share, ok := l.grant.(DataShare)

	return share, ok
}

// Delegation returns what an administrative link delegates. A data link and a
// link that is not effective both return false.
func (l Link) Delegation(now time.Time) (AdminDelegation, bool) {
	if !l.Effective(now) {
		return AdminDelegation{}, false
	}

	delegation, ok := l.grant.(AdminDelegation)

	return delegation, ok
}

// Inbound returns the links that let one grantee reach into another Project. It
// matches the grantee end only and never follows a result's grantor onward, so
// A to B and B to C do not compose into A to C.
func Inbound(links []Link, grantee ID, now time.Time) []Link {
	reaching := make([]Link, 0, len(links))
	for _, link := range links {
		if link.grantee == grantee && link.Effective(now) {
			reaching = append(reaching, link)
		}
	}

	return reaching
}

// LinkResolver returns the links that let a grantee reach into other Projects.
// Resolution is one hop: an implementation must not walk a chain.
type LinkResolver interface {
	Inbound(ctx context.Context, grantee ID) ([]Link, error)
}
