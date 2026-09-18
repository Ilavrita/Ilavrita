package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The authentication surface. It sits outside /fhir/R4 because it is not FHIR:
// nothing here is a resource, and an OperationOutcome would misdescribe it.
const (
	authBasePath = "/auth"
	loginPath    = "/login"
	logoutPath   = "/logout"
	sessionPath  = "/session"
)

// sessionLifetime is how long a login lasts before a credential must be proved
// again. The domain caps it; this is the value this build issues.
const sessionLifetime = 8 * time.Hour

// bearerPrefix is the one scheme this server reads a token under.
const bearerPrefix = "Bearer "

// authorizationField is the header a session token arrives in.
const authorizationField = "Authorization"

var (
	// errCredentialsRefused is the single answer to every failed login: an unknown
	// address, a wrong password, a disabled identity and an identity holding no
	// standing in the Project are indistinguishable, because telling them apart
	// tells an attacker which addresses and Projects exist.
	errCredentialsRefused = errors.New("ilavrita: those credentials do not authenticate here")

	// errMalformedLogin reports a body this server cannot read as a login.
	errMalformedLogin = errors.New("ilavrita: a login names a project, an email address and a password")
)

// loginRequest is what a caller presents. The Project is named by slug, because
// that is what a person knows; the id is an internal identifier.
type loginRequest struct {
	Project  string `json:"project"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is what a successful login returns. The token appears here once
// and is never readable again: only its digest is stored.
type loginResponse struct {
	Token      string `json:"token"`
	ExpiresAt  string `json:"expiresAt"`
	Project    string `json:"project"`
	Membership string `json:"membership"`
}

// sessionResponse describes the caller a token names, for a client that holds one
// and needs to know who it is.
type sessionResponse struct {
	Project    string `json:"project"`
	User       string `json:"user"`
	Membership string `json:"membership"`
	ExpiresAt  string `json:"expiresAt"`
	Admin      bool   `json:"admin"`
	SuperAdmin bool   `json:"superAdmin"`
}

// registerAuthRoutes publishes the routes that turn a credential into a session.
// They answer before anything is authorized, because who is asking is prior to
// what they may do.
func registerAuthRoutes(routes *router.Router[*core.RequestEvent]) {
	base := routes.Group(authBasePath)

	// The runtime allows every origin by default. A session token is a bearer
	// credential, so this surface answers no preflight and carries no
	// cross-origin headers at all.
	base.Unbind(apis.DefaultCorsMiddlewareId)

	base.POST(loginPath, logIn)
	base.POST(logoutPath, logOut)
	base.GET(sessionPath, describeSession)
}

// logIn proves a credential and issues one session. Every refusal answers the
// same way, so nothing here reports which step failed.
func logIn(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	var body loginRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedLogin)
	}

	if body.Project == "" || body.Email == "" || body.Password == "" {
		return refuse(request, errMalformedLogin)
	}

	issued, token, err := serving.authenticate(request.Request.Context(), body)
	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusOK, loginResponse{
		Token:      token.Reveal(),
		ExpiresAt:  issued.ExpiresAt().Format(time.RFC3339),
		Project:    string(issued.Project()),
		Membership: string(issued.Membership()),
	})
}

// authenticate resolves the Project, proves the credential, resolves the standing
// and issues the session. Every failure returns the same error, so a caller learns
// only that the four together did not hold.
func (b *backend) authenticate(
	ctx context.Context, body loginRequest,
) (project.Session, project.SessionToken, error) {
	owner, _, found, err := b.projects.BySlug(ctx, body.Project)
	if err != nil || !found {
		return project.Session{}, project.SessionToken{}, credentialFailure(err)
	}

	email, err := project.NormaliseEmail(body.Email)
	if err != nil {
		return project.Session{}, project.SessionToken{}, errCredentialsRefused
	}

	user, found, err := b.identity(ctx, owner.ID(), email, body.Password)
	if err != nil || !found {
		return project.Session{}, project.SessionToken{}, credentialFailure(err)
	}

	principal := project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(user.ID())}

	standing, held, err := b.resolvers.Memberships.Membership(ctx, owner.ID(), principal)
	if err != nil {
		return project.Session{}, project.SessionToken{}, err
	}

	// Standing is what a token carries, so an identity holding none is refused
	// here rather than handed a token that would authorize nothing.
	if !held || !standing.HoldsStanding() {
		return project.Session{}, project.SessionToken{}, errCredentialsRefused
	}

	return b.issue(ctx, owner.ID(), user.ID(), standing.ID())
}

// identity proves the credential in the Project's own realm, then in the system
// realm. A server-scoped identity administers across Projects, so it must be able
// to log in to one it holds standing in without being anchored to it.
func (b *backend) identity(
	ctx context.Context, owner project.ID, email project.Email, password string,
) (project.User, bool, error) {
	for _, realm := range []project.IdentityRealm{project.DeriveRealm(owner), project.SystemRealm} {
		user, found, err := b.users.Authenticate(ctx, realm, email, password)
		if err != nil {
			return project.User{}, false, err
		}

		if found {
			return user, true, nil
		}
	}

	return project.User{}, false, nil
}

// issue mints the session a successful login returns.
func (b *backend) issue(
	ctx context.Context, owner project.ID, user project.UserID, standing project.MembershipID,
) (project.Session, project.SessionToken, error) {
	id, err := project.MintSessionID(rand.Reader)
	if err != nil {
		return project.Session{}, project.SessionToken{}, err
	}

	session, token, err := project.IssueSession(
		owner, id, user, standing, time.Now().UTC(), sessionLifetime, rand.Reader)
	if err != nil {
		return project.Session{}, project.SessionToken{}, err
	}

	if err := b.sessions.Issue(ctx, session); err != nil {
		return project.Session{}, project.SessionToken{}, err
	}

	return session, token, nil
}

// credentialFailure hides which step refused. A driver error is still reported as
// itself, because that is this server's fault and not the caller's.
func credentialFailure(err error) error {
	if err != nil {
		return err
	}

	return errCredentialsRefused
}

// logOut destroys the session the request presented. A token that names nothing
// was already not a session, so this answers the same either way.
func logOut(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	session, found, err := serving.session(request)
	if err != nil {
		return refuse(request, err)
	}

	if !found {
		return request.NoContent(http.StatusNoContent)
	}

	if err := serving.sessions.Revoke(
		request.Request.Context(), session.Project(), session.ID(), time.Now().UTC()); err != nil {
		return refuse(request, err)
	}

	return request.NoContent(http.StatusNoContent)
}

// describeSession answers who the presented token names, so a client holding one
// does not have to remember what it logged in as.
func describeSession(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	session, found, err := serving.session(request)
	if err != nil {
		return refuse(request, err)
	}

	if !found {
		return refuse(request, errNoPrincipal)
	}

	standing, held, err := serving.resolvers.Memberships.Membership(
		request.Request.Context(), session.Project(), session.Principal())
	if err != nil {
		return refuse(request, err)
	}

	if !held || !standing.HoldsStanding() {
		return refuse(request, errNoPrincipal)
	}

	return request.JSON(http.StatusOK, sessionResponse{
		Project:    string(session.Project()),
		User:       string(session.User()),
		Membership: string(session.Membership()),
		ExpiresAt:  session.ExpiresAt().Format(time.RFC3339),
		Admin:      standing.IsAdmin(),
		SuperAdmin: standing.IsSuperAdmin(),
	})
}

// session resolves the token a request presents, or nothing. A malformed header
// and an unknown token are the same answer: nothing named a session.
func (b *backend) session(request *core.RequestEvent) (project.Session, bool, error) {
	raw := strings.TrimSpace(request.Request.Header.Get(authorizationField))
	if !strings.HasPrefix(raw, bearerPrefix) {
		return project.Session{}, false, nil
	}

	token, err := project.ParseSessionToken(strings.TrimSpace(strings.TrimPrefix(raw, bearerPrefix)))
	if err != nil {
		return project.Session{}, false, nil
	}

	return b.sessions.Resolve(request.Request.Context(), token, time.Now().UTC())
}
