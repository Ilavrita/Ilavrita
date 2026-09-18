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
type SecondFactor struct {
	user        UserID
	state       FactorState
	secret      TOTPSecret
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
	user UserID, state FactorState, secret TOTPSecret, lastStep int64, created, activated time.Time,
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

	return SecondFactor{
		user: user, state: state, secret: secret, lastStep: lastStep,
		createdAt: created.UTC(), activatedAt: activated.UTC(),
	}, nil
}

// User returns whose factor it is.
func (f SecondFactor) User() UserID { return f.user }

// State returns how far the enrolment got.
func (f SecondFactor) State() FactorState { return f.state }

// Secret returns the shared secret. It exists so a store can seal it and a
// verification can compute with it, and for nothing else.
func (f SecondFactor) Secret() TOTPSecret { return f.secret }

// LastStep returns the counter of the last code accepted.
func (f SecondFactor) LastStep() int64 { return f.lastStep }

// CreatedAt returns when the enrolment began.
func (f SecondFactor) CreatedAt() time.Time { return f.createdAt }

// ActivatedAt returns when it was proved, zero while pending.
func (f SecondFactor) ActivatedAt() time.Time { return f.activatedAt }

// Required reports whether this factor stands between its holder and a login.
// A pending one does not: it was enrolled and never proved.
func (f SecondFactor) Required() bool { return f.state == FactorActive }

// Prove checks a code against the factor and returns it advanced past the step
// that code belonged to, so the same code cannot be presented again.
//
// Proving a pending factor activates it. That is the only way one becomes
// required, so nothing can lock a person out of their own account except their
// own phone.
func (f SecondFactor) Prove(presented string, at time.Time) (SecondFactor, error) {
	step, err := f.secret.Verify(presented, at, f.lastStep)
	if err != nil {
		return SecondFactor{}, err
	}

	proved := f
	proved.lastStep = step

	if proved.state == FactorPending {
		proved.state, proved.activatedAt = FactorActive, at.UTC()
	}

	return proved, nil
}
