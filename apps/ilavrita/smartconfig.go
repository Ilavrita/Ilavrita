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
	// Issuer is omitted rather than empty when this deployment cannot sign an
	// identity token. SMART makes it conditional on the sso-openid-connect
	// capability — "otherwise, omitted" — and an issuer published by a server
	// that answers no OpenID Connect is one a client would follow to nothing.
	Issuer string `json:"issuer,omitempty"`

	// JWKSURI travels with the issuer, and is omitted with it.
	JWKSURI               string   `json:"jwks_uri,omitempty"`
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

	// Whether this deployment can sign decides three fields together: a document
	// claiming sso-openid-connect without an issuer, or an issuer without the
	// capability, is one a client cannot act on either way.
	identity := false
	if serving != nil {
		_, err := serving.signingKey(request.Request.Context())
		identity = err == nil
	}

	held := smartConfiguration{
		AuthorizationEndpoint: origin + oauthBasePath + authorizePath,
		TokenEndpoint:         origin + oauthBasePath + tokenPath,

		// SMART names two options here, and this server answers both. The
		// refresh grant is served too and is deliberately not listed: it is a
		// grant this endpoint answers, but not one of the two this field
		// enumerates, and a document that invents entries in a closed list is
		// one a validator reads as wrong rather than as generous.
		GrantTypes:    []string{"authorization_code", "client_credentials"},
		ResponseTypes: []string{"code"},

		// S256 alone. "plain" is unrepresentable here, so naming it would
		// advertise a downgrade ParseCodeChallenge refuses.
		ChallengeMethods: []string{"S256"},

		// SMART names three, and this server answers two of them. A public
		// client authenticates with PKCE alone, which RFC 8414 spells "none" and
		// SMART does not list — so it is left out rather than added to a closed
		// list. That a public client needs no secret is said by the
		// client-public capability below.
		TokenEndpointAuthWays: []string{"client_secret_basic", "private_key_jwt"},

		// Every scope here is one this server grants, because SMART says a
		// server SHALL support all of them. launch/encounter is absent: there is
		// no encounter context to convey, and listing it would be claiming one.
		ScopesSupported: []string{
			"launch/patient", "offline_access", "online_access",
			"user/*.rs", "patient/*.rs",
		},

		// Only what this build does — and everything it does, which matters as
		// much: a capability set is satisfied by the capabilities listed, so
		// omitting one this server supports makes a use case it serves look
		// unserved. permission-patient is what completes "Patient Access for
		// Standalone Apps", and permission-user what completes the clinician
		// set beside it.
		//
		// launch-ehr is absent because it is not built. sso-openid-connect is
		// added below when this deployment holds a key to sign with.
		Capabilities: []string{
			"launch-standalone",
			"client-public",
			"client-confidential-symmetric",
			"client-confidential-asymmetric",
			"context-standalone-patient",
			"permission-patient",
			"permission-user",
			"permission-offline",
			"permission-v1",
			"permission-v2",
		},
	}

	if identity {
		held.Issuer = origin
		held.JWKSURI = origin + oauthBasePath + identityKeysPath
		held.ScopesSupported = append(held.ScopesSupported, scopeOpenID, scopeFHIRUser)
		held.Capabilities = append(held.Capabilities, "sso-openid-connect")
	}

	return request.JSON(http.StatusOK, held)
}
