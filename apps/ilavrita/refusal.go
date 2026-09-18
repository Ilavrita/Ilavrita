package main

import (
	"errors"
	"log"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
	"github.com/pocketbase/pocketbase/core"
)

// refusal is the whole answer a client gets instead of a resource: one status,
// one issue code and one fixed sentence. It carries no wrapped error text,
// which is what keeps a resource type, logical id or Project out of a body.
type refusal struct {
	status int
	code   fhir.IssueCode
	detail string
}

// Error makes a refusal travel as an error, so one translation point answers
// both the refusals this package raises and the failures storage returns.
func (r refusal) Error() string {
	return r.detail
}

// The refusals the HTTP layer raises on its own, before or instead of storage.
var (
	unsupportedAccept = refusal{http.StatusNotAcceptable, fhir.CodeNotSupported,
		"This server serves " + fhir.ContentType + " only."}

	unsupportedBody = refusal{http.StatusUnsupportedMediaType, fhir.CodeNotSupported,
		"A request body must be sent as " + fhir.ContentType + "."}

	unknownResourceType = refusal{http.StatusNotFound, fhir.CodeNotFound,
		"This resource type is not a FHIR endpoint on this server."}

	unknownResource = refusal{http.StatusNotFound, fhir.CodeNotFound,
		"No such resource."}

	unreadableBody = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"The request body is not a JSON FHIR resource."}

	oversizedBody = refusal{http.StatusRequestEntityTooLarge, fhir.CodeTooCostly,
		"The request body is larger than this server accepts."}

	unrecognisedHost = refusal{http.StatusBadRequest, fhir.CodeSecurity,
		"This request names a host this server does not serve."}

	mismatchedType = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"The resource type in the body does not match the one in the URL."}

	mismatchedID = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"The id in the body does not match the one in the URL."}

	assignedID = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"A create must carry no id: this server assigns them."}

	malformedIfMatch = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"If-Match must carry exactly one quoted version id."}

	staleVersion = refusal{http.StatusPreconditionFailed, fhir.CodeConflict,
		"The resource is not at the version If-Match claims."}

	unauthenticated = refusal{http.StatusUnauthorized, fhir.CodeLogin,
		"This request carries no authenticated principal."}

	codeRefused = refusal{http.StatusUnauthorized, fhir.CodeLogin,
		"The second-factor code was refused."}

	subscriptionsUnavailable = refusal{http.StatusNotImplemented, fhir.CodeNotSupported,
		"This deployment is not configured to hold subscriptions."}

	unreadableSubscription = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"This subscription states something this server could not honour."}

	payloadUnavailable = refusal{http.StatusNotImplemented, fhir.CodeNotSupported,
		"This deployment is not configured to hold resource payloads."}

	unreadablePayload = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"A payload states its content type and carries readable content."}

	factorUnavailable = refusal{http.StatusNotImplemented, fhir.CodeNotSupported,
		"This deployment is not configured to hold second factors."}

	factorNotEnrolled = refusal{http.StatusNotFound, fhir.CodeNotFound,
		"No second factor is enrolled for this identity."}

	nothingToProve = refusal{http.StatusConflict, fhir.CodeConflict,
		"No second factor is awaiting proof."}

	unreadableSearch = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"This search could not be read."}

	unsupportedSearch = refusal{http.StatusBadRequest, fhir.CodeNotSupported,
		"This server does not implement that search parameter."}

	unreadableLogin = refusal{http.StatusBadRequest, fhir.CodeInvalid,
		"A login names a project, an email address and a password."}

	throttled = refusal{http.StatusTooManyRequests, fhir.CodeThrottled,
		"Too many login attempts. Wait before trying again."}

	notAuthorized = refusal{http.StatusForbidden, fhir.CodeForbidden,
		"This principal may not perform this interaction on this resource type."}

	deletedResource = refusal{http.StatusGone, fhir.CodeDeleted,
		"This resource has been deleted."}

	duplicateResource = refusal{http.StatusConflict, fhir.CodeDuplicate,
		"A resource already holds this logical id."}

	versionConflict = refusal{http.StatusConflict, fhir.CodeConflict,
		"Another write reached this resource first."}

	internalFailure = refusal{http.StatusInternalServerError, fhir.CodeException,
		"This request could not be completed."}
)

// refuse answers one failed interaction. Every path out of a handler that is
// not a resource goes through here, so no failure can answer in another shape.
func refuse(request *core.RequestEvent, err error) error {
	answer := translate(err)

	if answer.status == http.StatusInternalServerError {
		report(err)
	}

	// RFC 9110 15.5.2 makes a challenge mandatory on 401. It names the scheme
	// this server will accept, which is not one a request carries today.
	if answer.status == http.StatusUnauthorized {
		request.Response.Header().Set(authenticateField, authenticateChallenge)
	}

	outcome := fhir.NewOperationOutcome(fhir.SeverityError, answer.code, answer.detail)

	return respondFHIR(request, answer.status, outcome)
}

// translate maps one failure onto the single status and issue code it is
// assigned. An unrecognised error is an internal failure rather than a guess:
// answering a client from an error nobody classified is how detail leaks.
func translate(err error) refusal {
	var refused refusal

	switch {
	case errors.As(err, &refused):
		return refused
	case errors.Is(err, errNoPrincipal), errors.Is(err, errCredentialsRefused):
		return unauthenticated
	case errors.Is(err, errTooManyAttempts):
		return throttled
	case errors.Is(err, errPayloadUnavailable):
		return payloadUnavailable
	case errors.Is(err, errSubscriptionsUnavailable), errors.Is(err, subscription.ErrMissingOwner):
		return subscriptionsUnavailable
	case errors.Is(err, subscription.ErrMalformedCriteria),
		errors.Is(err, subscription.ErrUnknownChannel),
		errors.Is(err, subscription.ErrMalformedEndpoint),
		errors.Is(err, subscription.ErrUnknownStatus):
		return unreadableSubscription
	case errors.Is(err, errMissingContentType), errors.Is(err, errUnreadablePayload):
		return unreadablePayload
	case errors.Is(err, files.ErrTooLarge):
		return oversizedBody
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrMalformedKey):
		return unknownResource
	case errors.Is(err, errFactorUnavailable):
		return factorUnavailable
	case errors.Is(err, errFactorNotEnrolled):
		return factorNotEnrolled
	case errors.Is(err, project.ErrCodeRefused), errors.Is(err, project.ErrMalformedCode),
		errors.Is(err, project.ErrFactorInForce):
		return codeRefused
	case errors.Is(err, project.ErrNothingToProve):
		return nothingToProve
	case errors.Is(err, errMalformedLogin), errors.Is(err, errMalformedControlRequest):
		return unreadableLogin
	case errors.Is(err, search.ErrUnknownParameter), errors.Is(err, search.ErrUnsupportedModifier):
		return unsupportedSearch
	case errors.Is(err, search.ErrMalformedValue), errors.Is(err, search.ErrMalformedPaging),
		errors.Is(err, errUnreadableSearchBody):
		return unreadableSearch
	case errors.Is(err, errNotAdmin), errors.Is(err, errNotSuperAdmin):
		return notAuthorized
	case errors.Is(err, errUnknownProject):
		return unknownResource
	case errors.Is(err, storage.ErrDenied):
		return notAuthorized
	case errors.Is(err, storage.ErrNotFound):
		return unknownResource
	case errors.Is(err, storage.ErrDeleted):
		return deletedResource
	case errors.Is(err, storage.ErrAlreadyExists):
		return duplicateResource
	case errors.Is(err, storage.ErrVersionConflict):
		return versionConflict
	default:
		return internalFailure
	}
}

// report logs what the body must never carry. A scope escape is evidence the
// Project boundary itself may have failed, so it is logged as an incident and
// not as one more failed request (REST-29).
func report(err error) {
	if errors.Is(err, sqlite.ErrScopeEscape) {
		log.Printf("SECURITY ALERT: a stored row escaped the scope it was read under: %v", err)

		return
	}

	log.Printf("ERROR: a FHIR interaction failed: %v", err)
}
