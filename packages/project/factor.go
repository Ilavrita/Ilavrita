package project

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrMissingFactorUser reports a second factor belonging to nobody.
	ErrMissingFactorUser = errors.New("project: a second factor belongs to one identity")

	// ErrUnknownFactorState reports a state outside the enum.
	ErrUnknownFactorState = errors.New("project: unknown second-factor state")

	// ErrFactorNotProved reports an activation of a factor nobody proved they
	// hold. Enrolling one and never proving it must not lock a person out of
	// their own account.
	ErrFactorNotProved = errors.New("project: a second factor is activated by proving it")

	// ErrNothingToProve reports a confirmation of a factor with nothing
	// awaiting proof.
	ErrNothingToProve = errors.New("project: no second factor is awaiting proof")

	// ErrFactorInForce reports a replacement of a factor that is in force,
	// attempted without a code from the factor being replaced.
	//
	// Replacing one is how somebody moves to a new phone, and it is also how
	// somebody holding a stolen session would turn the factor off — the two
	// are the same request. A code from the factor in force is what tells them
	// apart, and it is the same rule withdrawing one follows.
	ErrFactorInForce = errors.New("project: replacing a factor in force needs a code from it")
)

// FactorState is how far an enrolment got.
type FactorState string

// The states a second factor can be in.
const (
	// FactorPending is enrolled but unproved. It does not stand between anyone
	// and their account: a person who scanned the code and then lost the phone
	// must still be able to log in.
	FactorPending FactorState = "pending"

	// FactorActive has been proved and is required from then on.
	FactorActive FactorState = "active"
)

// SecondFactor is one identity's enrolled factor.
//
// An active one may carry a replacement awaiting proof. The factor in force
// stays in force until that proof arrives, so moving to a new phone never
// leaves a window in which the account has no second factor at all.
type SecondFactor struct {
	user        UserID
	state       FactorState
	secret      TOTPSecret
	replacement TOTPSecret
	lastStep    int64
	createdAt   time.Time
	activatedAt time.Time
}

// EnrolSecondFactor begins an enrolment. It is pending until a code proves the
// person actually holds the secret.
func EnrolSecondFactor(user UserID, secret TOTPSecret, at time.Time) (SecondFactor, error) {
	if user == "" {
		return SecondFactor{}, ErrMissingFactorUser
	}

	if secret.IsZero() {
		return SecondFactor{}, ErrMalformedSecret
	}

	return SecondFactor{user: user, state: FactorPending, secret: secret, createdAt: at.UTC()}, nil
}

// RestoreSecondFactor rebuilds a stored one. A state outside the enum rebuilds
// nothing rather than the nearest one it recognises.
func RestoreSecondFactor(
	user UserID,
	state FactorState,
	secret, replacement TOTPSecret,
	lastStep int64,
	created, activated time.Time,
) (SecondFactor, error) {
	if user == "" {
		return SecondFactor{}, ErrMissingFactorUser
	}

	if state != FactorPending && state != FactorActive {
		return SecondFactor{}, fmt.Errorf("%w: %q", ErrUnknownFactorState, string(state))
	}

	if secret.IsZero() {
		return SecondFactor{}, ErrMalformedSecret
	}

	// A replacement only exists for a factor in force. One beside a pending
	// factor would be a second unproved secret, and nothing could say which of
	// them a code was meant to prove.
	if !replacement.IsZero() && state != FactorActive {
		return SecondFactor{}, fmt.Errorf("%w: a %s factor carries a replacement",
			ErrUnknownFactorState, state)
	}

	return SecondFactor{
		user: user, state: state, secret: secret, replacement: replacement, lastStep: lastStep,
		createdAt: created.UTC(), activatedAt: activated.UTC(),
	}, nil
}

// User returns whose factor it is.
func (f SecondFactor) User() UserID { return f.user }

// State returns how far the enrolment got.
func (f SecondFactor) State() FactorState { return f.state }

// Secret returns the shared secret in force. It exists so a store can seal it
// and a verification can compute with it, and for nothing else.
func (f SecondFactor) Secret() TOTPSecret { return f.secret }

// Replacement returns the secret awaiting proof, zero when none is.
func (f SecondFactor) Replacement() TOTPSecret { return f.replacement }

// Replacing reports whether a new secret is waiting to take over.
func (f SecondFactor) Replacing() bool { return !f.replacement.IsZero() }

// LastStep returns the counter of the last code accepted.
func (f SecondFactor) LastStep() int64 { return f.lastStep }

// CreatedAt returns when the enrolment began.
func (f SecondFactor) CreatedAt() time.Time { return f.createdAt }

// ActivatedAt returns when it was proved, zero while pending.
func (f SecondFactor) ActivatedAt() time.Time { return f.activatedAt }

// Required reports whether this factor stands between its holder and a login.
// A pending one does not: it was enrolled and never proved.
func (f SecondFactor) Required() bool { return f.state == FactorActive }

// Prove checks a code against the factor in force and returns it advanced past
// the step that code belonged to, so the same code cannot be presented again.
//
// It never changes what the factor is. A login proves the factor as it stands;
// what a code is allowed to change is Confirm's business.
func (f SecondFactor) Prove(presented string, at time.Time) (SecondFactor, error) {
	step, err := f.secret.Verify(presented, at, f.lastStep)
	if err != nil {
		return SecondFactor{}, err
	}

	proved := f
	proved.lastStep = step

	return proved, nil
}

// Replace puts a new secret behind the factor, awaiting proof.
//
// The factor in force stays in force until that proof arrives: an account does
// not lose its second factor because somebody started moving to a new phone.
// Replacing one that is in force needs a code from it, which the caller is
// expected to have proved before calling this.
func (f SecondFactor) Replace(secret TOTPSecret, at time.Time) (SecondFactor, error) {
	if secret.IsZero() {
		return SecondFactor{}, ErrMalformedSecret
	}

	// Nothing is in force yet, so there is nothing to keep: the unproved secret
	// is simply replaced by another unproved one.
	if f.state != FactorActive {
		return EnrolSecondFactor(f.user, secret, at)
	}

	replacing := f
	replacing.replacement = secret

	return replacing, nil
}

// Confirm proves the secret that is awaiting proof and puts it in force.
//
// This is the only way a factor becomes required, and the only way one is
// replaced: nothing can put a factor between a person and their account, or
// take one away, except a code from the phone holding it.
func (f SecondFactor) Confirm(presented string, at time.Time) (SecondFactor, error) {
	awaiting := f.secret
	if f.Replacing() {
		awaiting = f.replacement
	} else if f.state == FactorActive {
		// Active and nothing pending: there is nothing this code could prove.
		return SecondFactor{}, ErrNothingToProve
	}

	step, err := awaiting.Verify(presented, at, f.lastStep)
	if err != nil {
		return SecondFactor{}, err
	}

	confirmed := f
	confirmed.secret, confirmed.replacement = awaiting, TOTPSecret{}
	confirmed.lastStep = step
	confirmed.state, confirmed.activatedAt = FactorActive, at.UTC()

	return confirmed, nil
}
