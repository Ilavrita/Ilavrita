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
// Enrolling replaces whatever the identity had, and the new factor is pending
// until a code proves it. That is how somebody who lost their phone starts
// again — and why it cannot lock anyone out: a pending factor is required of
// nobody.
func enrolSecondFactor(request *core.RequestEvent) error {
	held, err := callerIdentity(request)
	if err != nil {
		return refuse(request, err)
	}

	if !serving.factors.Available() {
		return refuse(request, errFactorUnavailable)
	}

	secret, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		return refuse(request, err)
	}

	factor, err := project.EnrolSecondFactor(held.User(), secret, serving.clock())
	if err != nil {
		return refuse(request, err)
	}

	if err := serving.factors.Enrol(request.Request.Context(), factor); err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, factorEnrolment{
		Secret: secret.Encoded(),
		URI:    secret.ProvisioningURI(factorIssuer, string(held.User())),
	})
}

// factorIssuer is the name an authenticator app files the entry under.
const factorIssuer = "Ilavrita"

// activateSecondFactor proves a pending factor, which is the only way one
// becomes required. Nothing can put a factor between a person and their account
// except a code from the phone holding it.
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

	proved, err := factor.Prove(presented, serving.clock())
	if err != nil {
		return refuse(request, err)
	}

	if err := serving.factors.Prove(request.Request.Context(), proved); err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusOK, factorStatus{Enrolled: true, State: string(proved.State())})
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
