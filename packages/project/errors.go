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
)
