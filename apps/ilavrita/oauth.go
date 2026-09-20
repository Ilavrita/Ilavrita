package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The SMART App Launch surface.
//
// docs/design/smart-endpoints-spec.md is where the reasoning lives; the two
// decisions worth repeating here are that an access token is a session — there
// is no second kind of bearer credential — and that consent is two calls rather
// than a page, because this server renders no HTML.
const (
	oauthBasePath = "/oauth2"
	authorizePath = "/authorize"
	consentPath   = "/consent"
	tokenPath     = "/token"
)

// appSessionLifetime is how long an access token lives. SMART apps refresh, and
// a shorter window is a smaller thing to lose; the session ceiling is twelve
// hours and this sits well inside it.
const appSessionLifetime = time.Hour

// authorizationCodeLifetime is how long an app has to redeem its code. It is the
// round trip the app is already making.
const authorizationCodeLifetime = 60 * time.Second

// refreshLifetime is how long one grant may be refreshed for before the person
// is asked again. The ceiling travels across rotations rather than restarting,
// so an app that refreshes hourly still expires on the same day as one that
// refreshed once.
const refreshLifetime = 30 * 24 * time.Hour

// oauthFailure is one refusal in the shape RFC 6749 section 5.2 names. It is not
// an OperationOutcome: this surface is OAuth rather than FHIR, and a client
// library reading it expects the OAuth shape.
type oauthFailure struct {
	status      int
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// Error makes a refusal an error, so a handler can return one directly.
func (f oauthFailure) Error() string {
	if f.Description == "" {
		return f.Code
	}

	return f.Code + ": " + f.Description
}

// The OAuth error codes this surface answers with.
func invalidRequest(why string) oauthFailure {
	return oauthFailure{status: http.StatusBadRequest, Code: "invalid_request", Description: why}
}

func invalidClient(why string) oauthFailure {
	return oauthFailure{status: http.StatusUnauthorized, Code: "invalid_client", Description: why}
}

func invalidGrant(why string) oauthFailure {
	return oauthFailure{status: http.StatusBadRequest, Code: "invalid_grant", Description: why}
}

func invalidScope(why string) oauthFailure {
	return oauthFailure{status: http.StatusBadRequest, Code: "invalid_scope", Description: why}
}

func unsupportedGrant(why string) oauthFailure {
	return oauthFailure{status: http.StatusBadRequest, Code: "unsupported_grant_type", Description: why}
}

func serverFailure() oauthFailure {
	return oauthFailure{status: http.StatusInternalServerError, Code: "server_error"}
}

// refuseOAuth answers a refusal, and answers an unexpected error as a server
// fault rather than as whatever it happens to say: an internal message reaching
// a client is a client learning about this server's insides.
func refuseOAuth(request *core.RequestEvent, err error) error {
	var failure oauthFailure
	if !errors.As(err, &failure) {
		failure = serverFailure()
	}

	// A refused client authentication must say how to authenticate, which is
	// what makes 401 something a library can act on.
	if failure.Code == "invalid_client" {
		request.Response.Header().Set("WWW-Authenticate", `Basic realm="oauth2"`)
	}

	return request.JSON(failure.status, failure)
}

// registerOAuthRoutes publishes the SMART endpoints.
func registerOAuthRoutes(routes *router.Router[*core.RequestEvent]) {
	base := routes.Group(oauthBasePath)

	// The authorization endpoint is driven by the deployment's own consent UI,
	// which presents a session token. That is a bearer credential, so this
	// surface answers no preflight and carries no cross-origin headers — the
	// same stance the login surface takes.
	base.Unbind(apis.DefaultCorsMiddlewareId)

	// The authorization endpoint is SMART's, so a browser reaches it and leaves
	// for the consent page. It authenticates nobody: the person has not signed
	// in yet, which is the whole reason they are being sent somewhere.
	base.GET(authorizePath, beginAuthorization)

	// The consent page reads this and posts to the one below, both carrying the
	// person's own session. That is a bearer credential, so these two answer no
	// preflight — the page is the deployment's own and shares its origin.
	base.GET(consentPath, describeAuthorization)
	base.POST(authorizePath, approveAuthorization)

	// The token endpoint keeps the runtime's cross-origin handling, because a
	// browser app redeems its own code from its own origin. A code plus a
	// verifier is not an ambient credential a hostile page could replay — the
	// page would need the verifier, which never left the app — so allowing it
	// costs nothing, and refusing it would make every public browser app
	// unimplementable.
	routes.Group(oauthBasePath).POST(tokenPath, issueToken)

	// Discovery is read before a client holds anything, so it authenticates
	// nobody and keeps the runtime's cross-origin handling: a browser app reads
	// it from its own origin before it has a token to protect.
	routes.GET(smartConfigurationPath, describeSmartConfiguration)
}

// authorizationAsk is what a client asked for, read from a query or a form.
type authorizationAsk struct {
	responseType  string
	clientID      project.ClientApplicationID
	redirectURI   string
	scope         string
	state         string
	audience      string
	challenge     string
	challengeWay  string
	launchPatient string
}

// readAsk reads an authorization request from wherever this method carries it.
func readAsk(request *core.RequestEvent) (authorizationAsk, error) {
	held := request.Request.URL.Query()

	if request.Request.Method == http.MethodPost {
		if err := request.Request.ParseForm(); err != nil {
			return authorizationAsk{}, invalidRequest("the request body is not a form")
		}

		// A form-encoded approval states the request again; a JSON one states it
		// in the query, because a JSON body cannot also be a form.
		if len(request.Request.PostForm) > 0 {
			held = request.Request.PostForm
		}
	}

	return authorizationAsk{
		responseType:  held.Get("response_type"),
		clientID:      project.ClientApplicationID(held.Get("client_id")),
		redirectURI:   held.Get("redirect_uri"),
		scope:         held.Get("scope"),
		state:         held.Get("state"),
		audience:      held.Get("aud"),
		challenge:     held.Get("code_challenge"),
		challengeWay:  held.Get("code_challenge_method"),
		launchPatient: held.Get("launch"),
	}, nil
}

// pendingAuthorization is what a validated ask resolves to: the registration
// asking, the person who would approve, and what this server would grant.
type pendingAuthorization struct {
	app       project.ClientApplication
	session   project.Session
	challenge project.CodeChallenge
	grantable []string
	refused   []refusedScope
}

// refusedScope is one scope this server will not grant, and why.
//
// It is reported rather than dropped because SMART requires a client to read
// what it was granted, and a scope silently discarded is one the app believes it
// holds.
type refusedScope struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// resolveAsk validates an authorization request against the person asking.
//
// Order matters. The redirect address is checked before anything that could be
// reported back through it, because RFC 6749 section 4.1.2.1 is explicit that a
// server must not redirect to an address it has not verified — that is how an
// error response becomes a delivery mechanism for somebody else's code.
func (b *backend) resolveAsk(
	request *core.RequestEvent, ask authorizationAsk,
) (pendingAuthorization, error) {
	session, found, err := b.session(request)
	if err != nil {
		return pendingAuthorization{}, serverFailure()
	}

	// The authorization endpoint acts for a person. Without one there is nobody
	// whose standing a scope could narrow.
	if !found {
		return pendingAuthorization{}, invalidClient("this endpoint is reached by an authenticated person")
	}

	if ask.responseType != "code" {
		return pendingAuthorization{}, oauthFailure{
			status: http.StatusBadRequest, Code: "unsupported_response_type",
			Description: "this server issues authorization codes",
		}
	}

	if err := project.ValidateClientApplicationID(ask.clientID); err != nil {
		return pendingAuthorization{}, invalidRequest("client_id names no registration")
	}

	app, _, known, err := b.applications.ByID(request.Request.Context(), session.Project(), ask.clientID)
	if err != nil {
		return pendingAuthorization{}, serverFailure()
	}

	// An unknown registration and a suspended one answer alike: which client
	// ids exist is not something a guess should reveal.
	if !known || app.State() != project.ServiceActive {
		return pendingAuthorization{}, invalidClient("that registration cannot authorize")
	}

	if !app.RedirectURIs().Allows(ask.redirectURI) {
		return pendingAuthorization{}, invalidRequest("redirect_uri is not one this client registered")
	}

	if err := audienceMatches(request, ask.audience); err != nil {
		return pendingAuthorization{}, err
	}

	challenge, err := project.ParseCodeChallenge(ask.challenge, ask.challengeWay)
	if err != nil {
		return pendingAuthorization{}, invalidRequest(
			"this server requires PKCE with code_challenge_method=S256")
	}

	grantable, refused := sortScopes(ask.scope)
	if len(grantable) == 0 {
		return pendingAuthorization{}, invalidScope("no scope asked for is one this server grants")
	}

	return pendingAuthorization{
		app: app, session: session, challenge: challenge,
		grantable: grantable, refused: refused,
	}, nil
}

// audienceMatches refuses a request aimed somewhere else.
//
// Without it, an app pointed at a hostile authorization server will present that
// server's token here, or this server's token there. SMART requires the check,
// and it is the one thing binding an authorization to the resource server it was
// meant for.
func audienceMatches(request *core.RequestEvent, stated string) error {
	if stated == "" {
		return invalidRequest("aud is required and names this server's FHIR base")
	}

	expected, err := baseURL(request)
	if err != nil {
		return serverFailure()
	}

	if strings.TrimRight(stated, "/") != strings.TrimRight(expected, "/") {
		return invalidRequest("aud does not name this server")
	}

	return nil
}

// sessionScopes are the SMART scopes describing the token rather than the
// resources it reaches. They are recorded so a client can read back what it was
// granted; nothing downstream narrows by them, and authz.Narrow ignores them
// because they name no resource type.
var sessionScopes = []string{
	"launch/patient", "online_access", "offline_access",
}

// contextScopes ask for a launch context this build does not convey.
//
// launch is the EHR-launch scope, and there is no EHR launch here; the launch
// parameter this server reads is a patient id rather than the opaque handle an
// EHR issues. launch/encounter asks for an encounter in the token response, and
// nothing puts one there.
//
// They are refused by name for the same reason the identity scopes are: a client
// told it was granted launch/encounter will look for an encounter and find
// nothing, and a scope honoured in name only is worse than one plainly refused.
var contextScopes = []string{"launch", "launch/encounter"}

// identityScopes ask for an OpenID Connect identity token, and this build issues
// none.
//
// They are refused by name rather than granted, because granting them is a
// promise: a client that asked for openid and was told it received it will look
// for an id_token in the response and find nothing. That is the silent
// widening this surface refuses everywhere else — a scope quietly honoured in
// name only is worse than one plainly refused, because only one of them is
// visible to the app that depended on it.
var identityScopes = []string{"openid", "fhirUser", "profile"}

// sortScopes separates what this server would grant from what it refuses.
//
// Every refusal carries its reason, so an app told it may not have something
// learns which something and why, rather than discovering at request time that a
// scope it believed it held reaches nothing.
func sortScopes(stated string) ([]string, []refusedScope) {
	var (
		grantable []string
		refused   []refusedScope
	)

	for _, one := range strings.Fields(stated) {
		if slices.Contains(identityScopes, one) {
			refused = append(refused, refusedScope{
				Scope:  one,
				Reason: "this server issues no identity token",
			})

			continue
		}

		if slices.Contains(contextScopes, one) {
			refused = append(refused, refusedScope{
				Scope:  one,
				Reason: "this server conveys no launch context beyond the patient",
			})

			continue
		}

		if slices.Contains(sessionScopes, one) {
			grantable = append(grantable, one)

			continue
		}

		parsed, err := authz.ParseScope(one)
		if err != nil {
			refused = append(refused, refusedScope{Scope: one, Reason: reasonFor(err)})

			continue
		}

		// A person cannot approve a backend service's scope. It narrows against
		// the standing of whoever asks, and here that is them — so honouring one
		// would hand a service scope this person's own reach, which is the
		// widening the whole surface exists to prevent. A service asks for these
		// through client credentials, where the standing is its own.
		if parsed.Context == authz.ContextSystem {
			refused = append(refused, refusedScope{
				Scope:  one,
				Reason: "a backend service scope is obtained through client credentials, not by asking a person",
			})

			continue
		}

		grantable = append(grantable, one)
	}

	return grantable, refused
}

// reasonFor renders why a scope was refused, without saying anything but what
// the scope's own shape already says.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, authz.ErrUnsupportedScope):
		return "this server does not grant that scope"
	case errors.Is(err, authz.ErrMalformedScope):
		return "that is not a SMART scope"
	default:
		return "refused"
	}
}

// authorizationView is what the consent UI renders.
type authorizationView struct {
	Client      string         `json:"client"`
	ClientID    string         `json:"clientId"`
	Description string         `json:"description,omitempty"`
	RedirectURI string         `json:"redirectUri"`
	State       string         `json:"state,omitempty"`
	Grantable   []string       `json:"grantable"`
	Refused     []refusedScope `json:"refused,omitempty"`
	Patient     string         `json:"launchPatient,omitempty"`
}

// describeAuthorization answers what a client asked for and what this server
// would grant. It writes nothing: approving is a separate call, and a GET that
// created an approval would be one a link could trigger.
func describeAuthorization(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	ask, err := readAsk(request)
	if err != nil {
		return refuseOAuth(request, err)
	}

	pending, err := serving.resolveAsk(request, ask)
	if err != nil {
		return refuseOAuth(request, err)
	}

	return request.JSON(http.StatusOK, authorizationView{
		Client: pending.app.Name(), ClientID: string(pending.app.ID()),
		Description: pending.app.Description(),
		RedirectURI: ask.redirectURI, State: ask.state,
		Grantable: pending.grantable, Refused: pending.refused,
		Patient: ask.launchPatient,
	})
}

// approvalRequest is the person's decision: which of the offered scopes they
// agreed to.
type approvalRequest struct {
	Approved []string `json:"approved"`
}

// approvedResponse is where the client is sent next.
type approvedResponse struct {
	Redirect string `json:"redirect"`
}

// approveAuthorization records an approval and mints the code that stands for it.
func approveAuthorization(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	ask, err := readAsk(request)
	if err != nil {
		return refuseOAuth(request, err)
	}

	pending, err := serving.resolveAsk(request, ask)
	if err != nil {
		return refuseOAuth(request, err)
	}

	approved, err := approvedScopes(request, pending)
	if err != nil {
		return refuseOAuth(request, err)
	}

	launch, err := project.NewLaunchContext(ask.launchPatient, strings.Join(approved, " "))
	if err != nil {
		return refuseOAuth(request, invalidScope("nothing was approved"))
	}

	token, err := serving.mintCode(request.Request.Context(), pending, ask, launch)
	if err != nil {
		return refuseOAuth(request, err)
	}

	return request.JSON(http.StatusOK, approvedResponse{
		Redirect: redirectWith(ask.redirectURI, token, ask.state),
	})
}

// approvedScopes reads what the person agreed to, and refuses anything wider
// than what this server said it would grant.
//
// A body naming no scopes is the whole grantable set: a UI that offers no choice
// approved what it showed. A body naming one this server refused is a client
// widening between the two calls, which is the case worth refusing loudly.
func approvedScopes(request *core.RequestEvent, pending pendingAuthorization) ([]string, error) {
	var body approvalRequest

	if request.Request.Body != nil {
		// A form-encoded approval carries no JSON body, which is not an error:
		// the scopes then default to everything offered.
		_ = json.NewDecoder(request.Request.Body).Decode(&body)
	}

	if len(body.Approved) == 0 {
		return pending.grantable, nil
	}

	for _, one := range body.Approved {
		if !slices.Contains(pending.grantable, one) {
			return nil, invalidScope(fmt.Sprintf("%q is not a scope this server offered", one))
		}
	}

	return body.Approved, nil
}

// mintCode writes the approval down and returns the code that stands for it.
func (b *backend) mintCode(
	ctx context.Context, pending pendingAuthorization,
	ask authorizationAsk, launch project.LaunchContext,
) (project.AuthorizationCodeToken, error) {
	id, err := project.MintAuthorizationCodeID(rand.Reader)
	if err != nil {
		return project.AuthorizationCodeToken{}, serverFailure()
	}

	redirect, err := project.ParseRedirectURI(ask.redirectURI)
	if err != nil {
		return project.AuthorizationCodeToken{},
			invalidRequest("redirect_uri is not an address a code is delivered to")
	}

	code, token, err := project.IssueAuthorizationCode(
		pending.session.Project(), id, pending.app.ID(),
		pending.session.User(), pending.session.Membership(),
		redirect, pending.challenge, launch,
		time.Now().UTC(), authorizationCodeLifetime, rand.Reader)
	if err != nil {
		return project.AuthorizationCodeToken{}, serverFailure()
	}

	if err := b.codes.Issue(ctx, code); err != nil {
		return project.AuthorizationCodeToken{}, serverFailure()
	}

	return token, nil
}

// redirectWith builds the address the client is sent back to.
func redirectWith(address string, token project.AuthorizationCodeToken, state string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		// resolveAsk already proved this parses, so reaching here is a bug
		// rather than an input. Answering the bare address hands back no code,
		// which is the safe half of a wrong answer.
		return address
	}

	held := parsed.Query()
	held.Set("code", token.Reveal())

	if state != "" {
		held.Set("state", state)
	}

	parsed.RawQuery = held.Encode()

	return parsed.String()
}

// tokenResponse is what a client receives, in the shape RFC 6749 section 5.1
// names plus the SMART launch parameters.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Patient      string `json:"patient,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// issueToken exchanges a grant for an access token.
func issueToken(request *core.RequestEvent) error {
	if serving == nil {
		return refuseOAuth(request, serverFailure())
	}

	if err := request.Request.ParseForm(); err != nil {
		return refuseOAuth(request, invalidRequest("the request body is not a form"))
	}

	switch grant := request.Request.PostForm.Get("grant_type"); grant {
	case "authorization_code":
		return issueFromCode(request)
	case "refresh_token":
		return issueFromRefresh(request)
	case "":
		return refuseOAuth(request, invalidRequest("grant_type is required"))
	default:
		return refuseOAuth(request, unsupportedGrant(grant+" is not a grant this server issues"))
	}
}

// issueFromCode redeems a code and mints the session it stands for.
func issueFromCode(request *core.RequestEvent) error {
	held := request.Request.PostForm

	presented, err := project.ParseAuthorizationCode(held.Get("code"))
	if err != nil {
		return refuseOAuth(request, invalidGrant("no code was presented"))
	}

	client, secret, err := presentedClient(request)
	if err != nil {
		return refuseOAuth(request, err)
	}

	ctx := request.Request.Context()

	// Redeeming is what names the Project: the digest is the lookup key, and the
	// row it finds is what every later step is bound by. It also destroys the
	// code, so everything below runs against an approval nothing can replay.
	code, found, err := serving.codes.Redeem(
		ctx, presented, client, held.Get("redirect_uri"), held.Get("code_verifier"), time.Now().UTC())
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	if !found {
		return refuseOAuth(request, invalidGrant("that code cannot be redeemed"))
	}

	if err := serving.clientProved(ctx, code.Project(), client, secret); err != nil {
		return refuseOAuth(request, err)
	}

	// The refresh grant is opened before the session, so the session can record
	// which grant minted it: a session that forgot would survive the revocation
	// a replay of that grant triggers.
	chain, refresh, err := serving.openRefreshChain(
		ctx, code.Project(), code.Client(), code.User(), code.Membership(), code.Launch())
	if err != nil {
		return refuseOAuth(request, err)
	}

	issued, token, err := serving.issueAppSession(
		ctx, code.Project(), code.User(), code.Membership(), code.Launch(), chain)
	if err != nil {
		return refuseOAuth(request, err)
	}

	return request.JSON(http.StatusOK, tokenResponse{
		AccessToken:  token.Reveal(),
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(issued.ExpiresAt()).Seconds()),
		Scope:        code.Launch().Scopes(),
		Patient:      code.Launch().Patient(),
		RefreshToken: refresh,
	})
}

// openRefreshChain starts a grant an app may refresh, when the approval asked
// for one.
//
// An approval that named neither offline_access nor online_access is one the
// person agreed to for this session. Issuing a refresh token for it anyway would
// extend a grant nobody extended, so the empty chain comes back and the token
// response carries no refresh_token — which is how a client learns it must send
// the person back rather than discovering it an hour later.
func (b *backend) openRefreshChain(
	ctx context.Context, proj project.ID, client project.ClientApplicationID,
	user project.UserID, membership project.MembershipID, launch project.LaunchContext,
) (project.RefreshChainID, string, error) {
	if !project.RefreshRequested(launch) {
		return "", "", nil
	}

	chain, err := project.MintRefreshChainID(rand.Reader)
	if err != nil {
		return "", "", serverFailure()
	}

	id, err := project.MintRefreshTokenID(rand.Reader)
	if err != nil {
		return "", "", serverFailure()
	}

	grant, token, err := project.IssueRefreshToken(
		proj, id, chain, client, user, membership, launch,
		time.Now().UTC(), refreshLifetime, rand.Reader)
	if err != nil {
		return "", "", serverFailure()
	}

	if err := b.refreshes.Issue(ctx, grant); err != nil {
		return "", "", serverFailure()
	}

	return chain, token.Reveal(), nil
}

// issueFromRefresh exchanges a refresh token for a new session.
//
// Consent is not re-run and the scopes are not re-read from the client: the new
// session carries the launch context the original approval produced, and the
// narrowing happens per request against whatever the policy says then. That is
// the whole reason scopes are stored with the session rather than baked into a
// token.
func issueFromRefresh(request *core.RequestEvent) error {
	held := request.Request.PostForm

	presented, err := project.ParseRefreshToken(held.Get("refresh_token"))
	if err != nil {
		return refuseOAuth(request, invalidGrant("no refresh token was presented"))
	}

	client, secret, err := presentedClient(request)
	if err != nil {
		return refuseOAuth(request, err)
	}

	ctx := request.Request.Context()

	next, err := project.MintRefreshTokenID(rand.Reader)
	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	rotated, refresh, found, err := serving.refreshes.Rotate(
		ctx, presented, client, next, time.Now().UTC(), rand.Reader)

	// A token this server issued and already spent is a copy somebody else is
	// holding. The grant dies — every rotation of it, and every session it
	// minted — because the alternative is letting whoever replayed it keep what
	// they already obtained until it expired on its own.
	if errors.Is(err, sqlite.ErrRefreshReplayed) {
		if err := serving.refreshes.RevokeChain(
			ctx, rotated.Project(), rotated.Chain(), time.Now().UTC()); err != nil {
			return refuseOAuth(request, serverFailure())
		}

		return refuseOAuth(request, invalidGrant("that refresh token has already been used"))
	}

	if err != nil {
		return refuseOAuth(request, serverFailure())
	}

	if !found {
		return refuseOAuth(request, invalidGrant("that refresh token cannot be redeemed"))
	}

	if err := serving.clientProved(ctx, rotated.Project(), client, secret); err != nil {
		return refuseOAuth(request, err)
	}

	issued, token, err := serving.issueAppSession(
		ctx, rotated.Project(), rotated.User(), rotated.Membership(),
		rotated.Launch(), rotated.Chain())
	if err != nil {
		return refuseOAuth(request, err)
	}

	return request.JSON(http.StatusOK, tokenResponse{
		AccessToken:  token.Reveal(),
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(issued.ExpiresAt()).Seconds()),
		Scope:        rotated.Launch().Scopes(),
		Patient:      rotated.Launch().Patient(),
		RefreshToken: refresh.Reveal(),
	})
}

// presentedClient reads which client is redeeming, and whatever secret it
// presented. A confidential client authenticates with HTTP Basic, which is what
// RFC 6749 section 2.3.1 names first and what SMART client libraries send.
func presentedClient(request *core.RequestEvent) (project.ClientApplicationID, string, error) {
	if id, secret, basic := request.Request.BasicAuth(); basic {
		return project.ClientApplicationID(id), secret, nil
	}

	id := project.ClientApplicationID(request.Request.PostForm.Get("client_id"))
	if err := project.ValidateClientApplicationID(id); err != nil {
		return "", "", invalidClient("client_id names no registration")
	}

	return id, "", nil
}

// clientProved refuses a client that did not authenticate the way its
// registration says it must.
//
// The registered kind decides, not whether a secret happened to be presented. A
// confidential client sending none must be refused rather than falling through
// to PKCE alone, and a public client sending one is refused too: a public client
// holds no secret, so whatever it sent is something it should not have.
func (b *backend) clientProved(
	ctx context.Context, proj project.ID, id project.ClientApplicationID, secret string,
) error {
	app, _, known, err := b.applications.ByID(ctx, proj, id)
	if err != nil {
		return serverFailure()
	}

	if !known || app.State() != project.ServiceActive {
		return invalidClient("that registration cannot redeem a code")
	}

	if !app.Kind().KeepsASecret() {
		if secret != "" {
			return invalidClient("a public client presents no secret")
		}

		return nil
	}

	if secret == "" {
		return invalidClient("this client authenticates with its secret")
	}

	presented, err := project.ParseClientSecret(secret)
	if err != nil {
		return invalidClient("that secret is not one this server issued")
	}

	// The comparison happens inside the store, because no read in that package
	// hands a stored digest out: Credentials rebuilds every record with a
	// sentinel in place of the hash, so a caller holding those records could
	// never match anything against them.
	proved, err := b.applications.ProvesSecret(ctx, proj, id, presented, time.Now().UTC())
	if err != nil {
		return serverFailure()
	}

	if !proved {
		return invalidClient("that secret is not one this server issued")
	}

	return nil
}

// issueAppSession mints one access token for an approval, recording which
// refresh grant minted it so a replay of that grant can revoke this too.
func (b *backend) issueAppSession(
	ctx context.Context,
	proj project.ID, user project.UserID, membership project.MembershipID,
	launch project.LaunchContext, chain project.RefreshChainID,
) (project.Session, project.SessionToken, error) {
	id, err := project.MintSessionID(rand.Reader)
	if err != nil {
		return project.Session{}, project.SessionToken{}, serverFailure()
	}

	issued, token, err := project.IssueAppSession(
		proj, id, user, membership, launch, chain,
		time.Now().UTC(), appSessionLifetime, rand.Reader)
	if err != nil {
		return project.Session{}, project.SessionToken{}, serverFailure()
	}

	if err := b.sessions.Issue(ctx, issued); err != nil {
		return project.Session{}, project.SessionToken{}, serverFailure()
	}

	return issued, token, nil
}
