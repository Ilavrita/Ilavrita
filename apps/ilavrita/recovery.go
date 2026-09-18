package main

import (
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// secondFactorPath is where an administrator takes a lost factor off an
// identity. It names the Project as well as the identity, because the standing
// the request rests on is standing in a Project.
const secondFactorPath = "/projects/{project}/users/{user}/second-factor"

// userParameter names the identity a recovery acts on.
const userParameter = "user"

var (
	// errNoSuchIdentity reports an identity holding no standing in the named
	// Project. It is the same answer as one that does not exist: telling the two
	// apart tells an administrator of one Project who belongs to another.
	errNoSuchIdentity = errors.New("ilavrita: no such identity in that project")

	// errOwnFactor reports an administrator taking their own factor off.
	//
	// That is the hole this whole design closes: withdrawing your own factor
	// needs a code from it, so an administrator who could reach around that with
	// their own session would make a stolen administrator session enough to
	// disable the second factor it was meant to survive. Recovery is something
	// somebody else does for you.
	errOwnFactor = errors.New("ilavrita: an identity's own second factor is not recovered this way")
)

// recoverSecondFactor takes a lost second factor off an identity.
//
// This is the recovery path for a lost phone, and it is deliberately the only
// one: replacing a factor needs a code from the factor it replaces, which is
// what stops a stolen session switching MFA off, and leaves somebody who lost
// the phone with nothing to present. What answers that is another person with
// standing to act, not a weaker rule.
//
// The identity is signed out everywhere as part of it. The reset is also what
// an operator reaches for when the phone was stolen rather than lost, and a
// session already open on that phone would otherwise outlive the factor.
func recoverSecondFactor(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	named := project.ID(request.Request.PathValue(projectParameter))
	held := project.UserID(request.Request.PathValue(userParameter))

	if err := serving.administers(request, named); err != nil {
		return refuse(request, err)
	}

	if err := serving.recover(request, named, held); err != nil {
		return refuse(request, err)
	}

	return request.NoContent(http.StatusNoContent)
}

// recover performs the reset, having decided it may.
func (b *backend) recover(
	request *core.RequestEvent, named project.ID, held project.UserID,
) error {
	if b.factors == nil || !b.factors.Available() {
		return errFactorUnavailable
	}

	_, session, err := b.standing(request)
	if err != nil {
		return err
	}

	if session.User() == held {
		return errOwnFactor
	}

	// Standing in the Project the administrator administers, so an
	// administrator of one Project cannot reach an identity in another.
	if err := b.memberOf(request, named, held); err != nil {
		return err
	}

	ctx := request.Request.Context()

	if _, enrolled, err := b.factors.Enrolled(ctx, held); err != nil {
		return err
	} else if !enrolled {
		return errFactorNotEnrolled
	}

	if err := b.factors.Withdraw(ctx, held); err != nil {
		return err
	}

	if _, err := b.sessions.RevokeEveryUserSession(ctx, named, held, b.clock()); err != nil {
		return err
	}

	// Recorded as the deletion it is, against the thing deleted. A reset is the
	// one way a second factor comes off without the phone that answers for it,
	// so the trail has to say who did it and to whom.
	return b.record(ctx, audit.EventConfig{
		Project:    named,
		Principal:  session.Principal(),
		Membership: session.Membership(),
		Action:     audit.ActionDelete,
		Resource:   project.ProfileRef{Type: recoveredResource, ID: storage.LogicalID(held)},
		Outcome:    audit.OutcomeAllowed,
		Reason:     audit.ReasonNone,
	})
}

// recoveredResource is what a reset is recorded against. It is not a FHIR type
// and never will be, so an audit reader can tell this from a resource deletion
// by the type alone.
const recoveredResource storage.ResourceType = "SecondFactor"

// memberOf refuses an identity holding no standing in the named Project.
func (b *backend) memberOf(request *core.RequestEvent, named project.ID, held project.UserID) error {
	membership, found, err := b.resolvers.Memberships.Membership(
		request.Request.Context(), named,
		project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(held)})
	if err != nil {
		return err
	}

	if !found || membership.State() != project.MembershipActive {
		return errNoSuchIdentity
	}

	return nil
}
