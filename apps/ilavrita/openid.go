package main

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/core"
)

// Where OpenID Connect says a reader looks.
//
// The discovery document is at the issuer's root rather than beside the SMART
// one, because a client builds its address by appending to the `iss` claim it
// read out of an identity token. That claim names this server, so the document
// has to be where appending lands.
const (
	openIDConfigurationPath = "/.well-known/openid-configuration"
	identityKeysPath        = "/jwks"
)

// identityLifetime is how long an identity token is accepted for.
//
// It says who signed in, and a client reads it once on receipt. Five minutes is
// long enough to survive clock skew between two machines and short enough that
// a copy taken from a log is already useless.
const identityLifetime = 5 * time.Minute

// openIDConfiguration is the discovery document OpenID Connect Discovery 1.0
// defines, holding what this server actually answers.
type openIDConfiguration struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	SubjectTypes          []string `json:"subject_types_supported"`
	SigningAlgorithms     []string `json:"id_token_signing_alg_values_supported"`
	ScopesSupported       []string `json:"scopes_supported"`
	ClaimsSupported       []string `json:"claims_supported"`
}

// describeOpenIDConfiguration publishes where an identity token's signature can
// be checked, and what this server puts in one.
func describeOpenIDConfiguration(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	origin, err := publishing.origin(request.Request)
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	// A deployment that cannot sign has no OpenID Connect to describe, and says
	// so rather than publishing endpoints that would answer nothing.
	if _, err := serving.signingKey(request.Request.Context()); err != nil {
		return refuseOAuth(request, identityUnavailable())
	}

	return request.JSON(http.StatusOK, openIDConfiguration{
		Issuer:                origin,
		AuthorizationEndpoint: origin + oauthBasePath + authorizePath,
		TokenEndpoint:         origin + oauthBasePath + tokenPath,
		JWKSURI:               origin + oauthBasePath + identityKeysPath,
		ResponseTypes:         []string{"code"},

		// Public: the same subject reaches every client, because it is this
		// server's own identifier for the person. Pairwise would mean a
		// different one per client, which this build does not derive.
		SubjectTypes: []string{"public"},

		// RS256 alone, which is what SMART requires of an identity token and
		// what the signing key produces. Naming another would advertise a
		// signature no client would ever see.
		SigningAlgorithms: []string{"RS256"},

		ScopesSupported: []string{"openid", "fhirUser"},

		// fhirUser is SMART's addition to the standard set, and it is the claim
		// an app actually came for: it names the resource this person is.
		ClaimsSupported: []string{"iss", "sub", "aud", "exp", "iat", "fhirUser"},
	})
}

// describeIdentityKeys publishes the public half of what signs identity tokens.
//
// This is the jwks_uri, and it authenticates nobody: the whole point is that
// anybody holding a token can check it.
func describeIdentityKeys(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	key, err := serving.signingKey(request.Request.Context())
	if err != nil {
		return refuseOAuth(request, identityUnavailable())
	}

	document, err := key.PublishedJWKS()
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	request.Response.Header().Set(contentTypeField, "application/json")

	return request.String(http.StatusOK, document)
}

// identityUnavailable reports a deployment that issues no identity token.
//
// Nothing the caller sent is wrong: the deployment has simply not been given
// what it would need to sign. Saying so plainly is what stops a client reading
// an empty document as a server whose keys it failed to parse.
func identityUnavailable() oauthFailure {
	return oauthFailure{
		status: http.StatusNotFound, Code: "not_found",
		Description: "this deployment issues no identity token",
	}
}

// signingKey returns the key this process signs identity tokens with, minting
// one the first time anything needs it.
//
// Read once and held. The only writer is this mint, so a key that changed under
// a running process would be one whose published set no longer names what that
// process had already signed.
func (b *backend) signingKey(ctx context.Context) (project.SigningKey, error) {
	b.identityOnce.Do(func() {
		b.identityKey, b.identityErr = b.signingKeys.EnsureActive(ctx, rand.Reader)
	})

	return b.identityKey, b.identityErr
}

// identityOffer says what this deployment could assert about the person
// authorizing, which is what decides whether an identity scope is grantable or
// refused with a reason.
type identityOffer struct {
	// signing is whether a key exists to sign with at all. Without one there is
	// no identity token, whatever was asked for.
	signing bool

	// user is the absolute URL of the resource this person is, empty when their
	// membership names none. Granting fhirUser without one would promise a claim
	// the token could not carry.
	user string
}

// offeredIdentity works out what this server could assert about the person.
//
// Both halves fail harmlessly: a deployment with no signing key, and a
// membership with no profile, each cost one scope rather than the request.
func (b *backend) offeredIdentity(
	ctx context.Context, request *core.RequestEvent, session project.Session,
) identityOffer {
	if _, err := b.signingKey(ctx); err != nil {
		return identityOffer{}
	}

	offer := identityOffer{signing: true}

	base, err := baseURL(request)
	if err != nil {
		return offer
	}

	member, found, err := b.resolvers.Memberships.Membership(
		ctx, session.Project(), session.Principal())
	if err != nil || !found {
		return offer
	}

	profile, held := member.Profile()
	if !held || !profile.Valid() {
		return offer
	}

	offer.user = base + "/" + string(profile.Type) + "/" + string(profile.ID)

	return offer
}

// sort decides one identity scope.
//
// It answers three things: whether the scope is an identity scope at all,
// whether this deployment would grant it, and — when it would not — the reason
// an app is owed. A caller that only asked the first two would have to invent
// the third.
func (o identityOffer) sort(scope string) (granted bool, reason string, identity bool) {
	switch scope {
	case scopeOpenID:
		if !o.signing {
			return false, "this deployment issues no identity token", true
		}

		return true, "", true

	case scopeFHIRUser:
		switch {
		case !o.signing:
			return false, "this deployment issues no identity token", true
		case o.user == "":
			return false, "this account names no FHIR resource to be", true
		}

		return true, "", true

	case scopeProfile:
		return false, "profile is ambiguous between SMART and OpenID Connect;" +
			" ask for fhirUser, which this server answers", true
	}

	return false, "", false
}

// grantedScope reports whether an approval named one particular scope.
func grantedScope(launch project.LaunchContext, scope string) bool {
	return slices.Contains(strings.Fields(launch.Scopes()), scope)
}

// identityToken mints the token an approval of `openid` promised, and returns
// an empty string when no approval asked for one.
//
// The audience is the client rather than this server: an identity token is read
// by the app it was minted for, and one naming this server would be accepted by
// any app that happened to receive it.
func (b *backend) identityToken(
	ctx context.Context, request *core.RequestEvent, session project.Session,
	client project.ClientApplicationID, launch project.LaunchContext, issued time.Time,
) (string, error) {
	if !grantedScope(launch, "openid") {
		return "", nil
	}

	key, err := b.signingKey(ctx)
	if err != nil {
		return "", err
	}

	origin, err := publishing.origin(request.Request)
	if err != nil {
		return "", err
	}

	// The claim goes in only when it was approved. A token carrying fhirUser to
	// a client that never asked would be volunteering who this person is.
	user := ""
	if grantedScope(launch, "fhirUser") {
		user = b.offeredIdentity(ctx, request, session).user
	}

	return key.Sign(project.IdentityClaims{
		Issuer:    origin,
		Subject:   string(session.Principal().ID),
		Audience:  string(client),
		FHIRUser:  user,
		IssuedAt:  issued,
		ExpiresAt: issued.Add(identityLifetime),
	})
}
