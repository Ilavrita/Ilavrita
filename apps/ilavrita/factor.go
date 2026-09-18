package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/core"
)

// The routes an identity manages its own second factor through. Each acts on
// the caller's own factor and no one else's: there is no user in the path,
// because a factor somebody else could enrol for you is not a factor.
const (
	factorPath         = "/mfa"
	factorActivatePath = "/mfa/activate"
)

var (
	// errFactorUnavailable reports a deployment that configured no sealing key.
	// Enrolment is refused rather than storing the secret in the clear, which
	// would make the database a list of everyone's second factor.
	errFactorUnavailable = errors.New("ilavrita: this deployment cannot hold second factors")

	// errFactorNotEnrolled reports an activation or a withdrawal of a factor
	// that is not there.
	errFactorNotEnrolled = errors.New("ilavrita: no second factor is enrolled")
)

// factorEnrolment is what enrolling answers: the secret, once.
type factorEnrolment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// factorCode is what activating and withdrawing carry.
type factorCode struct {
	Code string `json:"code"`
}

// factorStatus is what the session route reports about the caller's factor.
type factorStatus struct {
	Enrolled bool   `json:"enrolled"`
	State    string `json:"state,omitempty"`
}

// enrolSecondFactor begins an enrolment for the caller's own identity.
//
// The secret is returned once, here, and never again: this is the only moment
// it can be shown, because what is stored afterwards is sealed and what an
// authenticator app holds is the person's own.
//
// Enrolling over a factor that is in force is moving to a new phone, and it
// needs a current code from the old one — it is the same request somebody
// holding a stolen session would make to turn the factor off, and the code is
// what tells the two apart. The factor in force stays in force until the new
// one is proved, so the move never leaves the account without one.
//
// Enrolling when there is nothing in force needs no code: an unproved factor
// protects nobody, and requiring one would strand somebody whose first attempt
// went wrong.
func enrolSecondFactor(request *core.RequestEvent) error {
	held, presented, err := callerCode(request)
	if err != nil {
		return refuse(request, err)
	}

	existing, enrolled, err := serving.factors.Enrolled(request.Request.Context(), held.User())
	if err != nil {
		return refuse(request, err)
	}

	secret, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		return refuse(request, err)
	}

	if enrolled && existing.Required() {
		err = replaceSecondFactor(request, existing, secret, presented)
	} else {
		err = beginSecondFactor(request, held, secret)
	}

	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, factorEnrolment{
		Secret: secret.Encoded(),
		URI:    secret.ProvisioningURI(factorIssuer, string(held.User())),
	})
}

// beginSecondFactor enrols where nothing is in force.
func beginSecondFactor(
	request *core.RequestEvent, held project.Session, secret project.TOTPSecret,
) error {
	factor, err := project.EnrolSecondFactor(held.User(), secret, serving.clock())
	if err != nil {
		return err
	}

	return serving.factors.Enrol(request.Request.Context(), factor)
}

// replaceSecondFactor puts a new secret behind a factor that is in force, once
// a code from that factor has proved who is asking.
func replaceSecondFactor(
	request *core.RequestEvent,
	existing project.SecondFactor,
	secret project.TOTPSecret,
	presented string,
) error {
	proved, err := existing.Prove(presented, serving.clock())
	if err != nil {
		return err
	}

	replacing, err := proved.Replace(secret, serving.clock())
	if err != nil {
		return err
	}

	return serving.factors.Replace(request.Request.Context(), replacing)
}

// factorIssuer is the name an authenticator app files the entry under.
const factorIssuer = "Ilavrita"

// activateSecondFactor proves whatever is awaiting proof and puts it in force.
//
// This is the only way a factor becomes required, and the only way one is
// replaced: nothing can put a factor between a person and their account, or
// swap the phone that answers for it, except a code from that phone.
func activateSecondFactor(request *core.RequestEvent) error {
	held, presented, err := callerCode(request)
	if err != nil {
		return refuse(request, err)
	}

	factor, enrolled, err := serving.factors.Enrolled(request.Request.Context(), held.User())
	if err != nil {
		return refuse(request, err)
	}

	if !enrolled {
		return refuse(request, errFactorNotEnrolled)
	}

	confirmed, err := factor.Confirm(presented, serving.clock())
	if err != nil {
		return refuse(request, err)
	}

	if err := serving.factors.Confirm(request.Request.Context(), confirmed); err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusOK, factorStatus{Enrolled: true, State: string(confirmed.State())})
}

// withdrawSecondFactor removes the caller's factor, and requires a current code
// to do it. A session is a bearer credential: without the code, stealing one
// would be enough to take the second factor off the account it protects.
func withdrawSecondFactor(request *core.RequestEvent) error {
	held, presented, err := callerCode(request)
	if err != nil {
		return refuse(request, err)
	}

	factor, enrolled, err := serving.factors.Enrolled(request.Request.Context(), held.User())
	if err != nil {
		return refuse(request, err)
	}

	if !enrolled {
		return refuse(request, errFactorNotEnrolled)
	}

	// A pending factor is required of nobody, so no code can be demanded for
	// one: the phone that would produce it may be exactly what was lost.
	if factor.Required() {
		proved, err := factor.Prove(presented, serving.clock())
		if err != nil {
			return refuse(request, err)
		}

		if err := serving.factors.Prove(request.Request.Context(), proved); err != nil {
			return refuse(request, err)
		}
	}

	if err := serving.factors.Withdraw(request.Request.Context(), held.User()); err != nil {
		return refuse(request, err)
	}

	return request.NoContent(http.StatusNoContent)
}

// callerIdentity names the identity acting on its own factor. A session is the
// only answer, as it is everywhere else.
func callerIdentity(request *core.RequestEvent) (project.Session, error) {
	if serving == nil {
		return project.Session{}, errNotServing
	}

	session, found, err := serving.session(request)
	if err != nil {
		return project.Session{}, err
	}

	if !found {
		return project.Session{}, errNoPrincipal
	}

	return session, nil
}

// callerCode reads the identity and the code one request carries.
func callerCode(request *core.RequestEvent) (project.Session, string, error) {
	held, err := callerIdentity(request)
	if err != nil {
		return project.Session{}, "", err
	}

	if !serving.factors.Available() {
		return project.Session{}, "", errFactorUnavailable
	}

	var body factorCode
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return project.Session{}, "", errMalformedLogin
	}

	return held, body.Code, nil
}
