package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// consentURLVariable names where a person is sent to approve an authorization.
//
// SMART's authorization endpoint is a browser endpoint: an app sends the person
// there and expects them to come back at the redirect address carrying a code.
// This server renders no HTML, so it sends them somewhere that does — a page the
// deployment supplies, which reads GET /oauth2/consent and posts the approval.
//
// That keeps the consent screen out of this repository, where it would have to
// be styled, translated and kept accessible by whoever deployed it, while making
// the endpoint itself the one SMART specifies.
const consentURLVariable = "ILAVRITA_CONSENT_URL"

// errNoConsentPage reports a deployment with nowhere to send anybody. It is a
// configuration failure rather than a client error, and it is said plainly: the
// alternative is an endpoint that redirects to nothing.
var errNoConsentPage = errors.New(
	"ilavrita: " + consentURLVariable + " must name the page a person approves an authorization on")

// consentPage is where an authorization request sends the person. The zero value
// is a deployment that configured none, and the authorization endpoint answers a
// server fault rather than redirecting nowhere.
var consentPage string

// configuredConsentPage reads where a person approves.
//
// An address that is not absolute, or carries a fragment, is refused at startup
// rather than at the first authorization: a redirect target this server cannot
// build is one no app could ever complete a launch through, and a deployment
// should learn that when it starts rather than when somebody tries.
func configuredConsentPage() (string, error) {
	stated := strings.TrimSpace(os.Getenv(consentURLVariable))
	if stated == "" {
		return "", nil
	}

	parsed, err := url.Parse(stated)
	if err != nil {
		return "", errors.Join(errNoConsentPage, err)
	}

	if !parsed.IsAbs() || parsed.Fragment != "" {
		return "", errNoConsentPage
	}

	return stated, nil
}

// beginAuthorization is SMART's authorization endpoint: a browser arrives, and
// leaves for somewhere it can approve.
//
// What it does *not* do here is resolve the client or verify the redirect
// address, and that is not an omission. A browser arriving from an app carries
// no session, so this server does not yet know which Project is being asked
// about — a client id is unique within one, not across the install. The Project
// is whatever the person who approves belongs to, so the checks that need it
// happen at GET /oauth2/consent, once somebody has signed in.
//
// Redirecting before those checks is safe because the address redirected to is
// the deployment's own consent page, not the app's. RFC 6749 section 4.1.2.1
// forbids reporting an error to an address that has not been verified; it says
// nothing about sending the person to a first-party page, which is what this is.
func beginAuthorization(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	ask, err := readAsk(request)
	if err != nil {
		return refuseOAuth(request, err)
	}

	// The shape, which needs nothing this server has to look up.
	if ask.responseType != "code" {
		return refuseOAuth(request, oauthFailure{
			status: http.StatusBadRequest, Code: "unsupported_response_type",
			Description: "this server issues authorization codes",
		})
	}

	for name, stated := range map[string]string{
		"client_id":             string(ask.clientID),
		"redirect_uri":          ask.redirectURI,
		"scope":                 ask.scope,
		"aud":                   ask.audience,
		"code_challenge":        ask.challenge,
		"code_challenge_method": ask.challengeWay,
	} {
		if stated == "" {
			return refuseOAuth(request, invalidRequest(name+" is required"))
		}
	}

	if consentPage == "" {
		return refuseOAuth(request, serverFailure())
	}

	sent, err := consentAddress(ask)
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	return request.Redirect(http.StatusFound, sent)
}

// consentAddress builds where the person is sent, carrying the request they are
// approving so the page can describe it without inventing anything.
//
// Every parameter travels as it arrived. The page hands them back to
// GET /oauth2/consent, which is what validates them — so a value changed in
// between is one that fails there rather than one this server has already
// accepted.
func consentAddress(ask authorizationAsk) (string, error) {
	parsed, err := url.Parse(consentPage)
	if err != nil {
		return "", err
	}

	held := parsed.Query()

	for name, value := range map[string]string{
		"response_type":         ask.responseType,
		"client_id":             string(ask.clientID),
		"redirect_uri":          ask.redirectURI,
		"scope":                 ask.scope,
		"state":                 ask.state,
		"aud":                   ask.audience,
		"code_challenge":        ask.challenge,
		"code_challenge_method": ask.challengeWay,
		"launch":                ask.launchPatient,
	} {
		if value != "" {
			held.Set(name, value)
		}
	}

	parsed.RawQuery = held.Encode()

	return parsed.String(), nil
}
