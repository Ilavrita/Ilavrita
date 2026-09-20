package main

import (
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/pocketbase/pocketbase/core"
)

// smartConfigurationPath is where SMART says a client looks, relative to the
// FHIR base rather than the host: one deployment may serve several bases, and
// each answers for itself.
const smartConfigurationPath = fhir.BasePath + "/.well-known/smart-configuration"

// smartConfiguration is the discovery document SMART App Launch defines.
//
// Everything in it is derived from what this server actually serves. Nothing is
// a constant that could drift: the endpoints are built from the same origin the
// CapabilityStatement publishes, the challenge methods are the ones
// ParseCodeChallenge accepts, and the grant types are the ones the token
// endpoint answers to. A discovery document advertising more than that would be
// one a conformance suite believes.
type smartConfiguration struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	GrantTypes            []string `json:"grant_types_supported"`
	ResponseTypes         []string `json:"response_types_supported"`
	ChallengeMethods      []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthWays []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported       []string `json:"scopes_supported"`
	Capabilities          []string `json:"capabilities"`
}

// describeSmartConfiguration publishes where a client goes and what it may ask
// for.
func describeSmartConfiguration(request *core.RequestEvent) error {
	origin, err := publishing.origin(request.Request)
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	// The issuer is the FHIR base, which is also what aud must name: a client
	// reading this document and sending that aud is one this server accepts, and
	// the two cannot disagree because both are built here.
	return request.JSON(http.StatusOK, smartConfiguration{
		Issuer:                origin + fhir.BasePath,
		AuthorizationEndpoint: origin + oauthBasePath + authorizePath,
		TokenEndpoint:         origin + oauthBasePath + tokenPath,

		// What issueToken actually answers to. client_credentials is absent
		// because private_key_jwt is not built; advertising it would make this
		// document a promise the token endpoint breaks.
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},

		// S256 alone. "plain" is unrepresentable here, so naming it would
		// advertise a downgrade ParseCodeChallenge refuses.
		ChallengeMethods: []string{"S256"},

		// A public client authenticates with PKCE alone, which is what "none"
		// names; a confidential one presents its secret over Basic.
		TokenEndpointAuthWays: []string{"none", "client_secret_basic"},

		ScopesSupported: sessionScopes,

		// Only what this build does. launch-standalone is the flow the
		// authorization endpoint serves, and permission-v1 and permission-v2 are
		// the scope syntaxes ParseScope reads.
		Capabilities: []string{
			"launch-standalone",
			"client-public",
			"client-confidential-symmetric",
			"context-standalone-patient",
			"permission-v1",
			"permission-v2",
		},
	})
}
