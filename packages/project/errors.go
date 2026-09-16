package project

import "errors"

var (
	// ErrInvalidProjectID reports an empty or wildcard project id.
	ErrInvalidProjectID = errors.New("project: invalid project id")

	// ErrMissingID reports a record built without its own identifier.
	ErrMissingID = errors.New("project: identifier is required")

	// ErrUnknownKind reports a kind outside the enum.
	ErrUnknownKind = errors.New("project: unknown kind")

	// ErrUnknownState reports a state outside the enum. An unrecognised state
	// fails closed instead of being read as the nearest known one.
	ErrUnknownState = errors.New("project: unknown state")

	// ErrInvalidTransition reports a move the state machine forbids.
	ErrInvalidTransition = errors.New("project: invalid state transition")

	// ErrInvalidPrincipal reports a membership with no single named principal.
	ErrInvalidPrincipal = errors.New("project: membership needs exactly one named principal")

	// ErrMissingEmail reports an identity built with no address. Nothing can be
	// invited, resolved or deduplicated without one.
	ErrMissingEmail = errors.New("project: a user needs an email address")

	// ErrInvalidEmail reports input NormaliseEmail refused. An address it cannot
	// be sure of is rejected, never repaired into one it can.
	ErrInvalidEmail = errors.New("project: invalid email address")

	// ErrInvalidCredentialState reports a state and a credential that contradict
	// each other, such as an active identity that never set a password.
	ErrInvalidCredentialState = errors.New("project: user state and credential disagree")

	// ErrInvalidProfile reports a profile reference missing its type or its id.
	ErrInvalidProfile = errors.New("project: profile needs a resource type and a logical id")

	// ErrInvalidSource reports a membership source outside the enum, or one
	// claiming a link, which only NewLinkedMembership may set.
	ErrInvalidSource = errors.New("project: invalid membership source")

	// ErrSuperAdminOutsideSuperProject reports super admin claimed anywhere but
	// the Super Project, mirroring the database constraint that refuses the row.
	ErrSuperAdminOutsideSuperProject = errors.New("project: super admin exists only in the super project")

	// ErrLinkSourcedPrivilege reports an attempt to give link-sourced standing an
	// admin flag or a policy binding. Administrative reach must not become a data
	// grant (FR-052).
	ErrLinkSourcedPrivilege = errors.New("project: link-sourced membership holds no admin flag and no policy binding")

	// ErrSelfLink reports a link whose grantor and grantee are the same Project.
	ErrSelfLink = errors.New("project: a link needs two different projects")

	// ErrMissingPolicy reports a data link with no AccessPolicy. An absent
	// restriction is refused, never lowered to unrestricted.
	ErrMissingPolicy = errors.New("project: a data link needs a grantor-owned access policy")

	// ErrMissingResourceType reports a data link that names no resource type.
	ErrMissingResourceType = errors.New("project: a data link needs at least one resource type")

	// ErrMissingCapability reports an administrative link that names no capability.
	ErrMissingCapability = errors.New("project: an administrative link needs at least one capability")

	// ErrCapabilityNotLinkConferrable reports a capability outside the closed
	// allowlist, such as a membership or policy write.
	ErrCapabilityNotLinkConferrable = errors.New("project: capability may not be conferred by a link")

	// ErrUnnamedPrincipal reports an administrative link naming no principal. A
	// link targets enumerated principals, never a role whose roster it can write.
	ErrUnnamedPrincipal = errors.New("project: an administrative link must name its principals")

	// ErrApprovalIncomplete reports activation attempted before both sides agreed.
	ErrApprovalIncomplete = errors.New("project: a link activates only after both sides approve")

	// ErrMissingProjectName reports a Project described without a slug or a name.
	ErrMissingProjectName = errors.New("project: a project needs a slug and a name")

	// ErrInvalidInstance reports an install record that contradicts its own
	// bootstrap state, such as a claimed install still holding token material.
	ErrInvalidInstance = errors.New("project: instance contradicts its bootstrap state")

	// ErrNotProvisioned reports a claim against an install the migration has not
	// created yet. Nothing is minted for one.
	ErrNotProvisioned = errors.New("project: the install has no instance record")

	// ErrBootstrapComplete reports a claim against an install already claimed.
	// The single claim is spent, so a replay of the same token confers nothing.
	ErrBootstrapComplete = errors.New("project: the install is already claimed")

	// ErrClaimTokenInvalid reports a claim token that is not the one on file, or
	// a claim against an install holding no token at all.
	ErrClaimTokenInvalid = errors.New("project: claim token does not match")

	// ErrClaimTokenExpired reports a claim token presented past its expiry. An
	// expired token grants nothing without anyone having to revoke it.
	ErrClaimTokenExpired = errors.New("project: claim token has expired")

	// ErrInvalidTokenExpiry reports a claim token asked to live for no time at
	// all, or forever.
	ErrInvalidTokenExpiry = errors.New("project: a claim token needs a positive lifetime")
)
