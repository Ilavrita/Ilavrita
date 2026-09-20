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

	// ErrClinicalDataInSuperProject reports the Project that administers the
	// install being asked to hold patient data. It governs; it never treats.
	ErrClinicalDataInSuperProject = errors.New("project: the super project holds no clinical data")

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

	// ErrInvalidServiceID reports a client application, bot or credential id
	// outside its family's namespace. The membership uniqueness index keys on the
	// principal id alone, so two families sharing one id would be one principal.
	ErrInvalidServiceID = errors.New("project: identifier is outside its principal namespace")

	// ErrInvalidRedirectURI reports an address an authorization code must not be
	// handed back to: one that is relative, carries a fragment, is cleartext
	// somewhere other than loopback, or names a scheme that runs its content
	// rather than addressing something.
	ErrInvalidRedirectURI = errors.New("project: invalid redirect uri")

	// ErrMissingServiceName reports a machine principal registered with no name.
	// Nothing an operator can revoke in a hurry is nameless.
	ErrMissingServiceName = errors.New("project: a client application or bot needs a name")

	// ErrServiceNotActive reports a credential asked for by a suspended or
	// revoked registration. A suspension that still mints keys suspends nothing.
	ErrServiceNotActive = errors.New("project: only an active registration issues a credential")

	// ErrInvalidCredentialExpiry reports a credential with no expiry, one that
	// dies before it was issued, or one asked to outlive the ceiling. A secret
	// without an expiry never dies, and one with a distant one was never rotated.
	ErrInvalidCredentialExpiry = errors.New("project: a credential needs an expiry within its permitted lifetime")

	// ErrInvalidSecretState reports a credential whose state and secret material
	// contradict each other, such as a revoked one that still holds a hash.
	ErrInvalidSecretState = errors.New("project: credential state and secret material disagree")

	// ErrCredentialRevoked reports a move against a credential already spent.
	// Revocation destroyed the secret, so nothing is left to revoke or reissue.
	ErrCredentialRevoked = errors.New("project: a revoked credential is never reissued")

	// ErrMachinePrincipalPrivilege reports super admin on a non-human principal.
	// Administering the install is answerable work, and a secret answers to no one.
	ErrMachinePrincipalPrivilege = errors.New("project: super admin belongs to a user principal")

	// ErrBotPrivilege reports administrative standing on a bot. A bot runs code
	// this server invokes, so admin on one is a control-plane write that code reaches.
	ErrBotPrivilege = errors.New("project: a bot holds no administrative standing")

	// ErrInvalidSession reports a session whose state, material or lifetime
	// contradict each other, or a request presenting no token at all. Nothing is
	// read as a session that merely happens to be shaped like one.
	ErrInvalidSession = errors.New("project: invalid session")

	// ErrInvalidLaunch reports a launch context that grants nothing, or one whose
	// patient is not a single logical id. An app's session that cannot say what
	// the app was granted is one nothing narrows, so it is refused rather than
	// stored and later read as an ordinary login.
	ErrInvalidLaunch = errors.New("project: invalid launch context")

	// ErrMachinePrincipalProfile reports a profile on a client application or a
	// bot. A machine principal's authority is its AccessPolicy, never a
	// compartment it occupies.
	ErrMachinePrincipalProfile = errors.New("project: a machine principal carries no profile")
)
