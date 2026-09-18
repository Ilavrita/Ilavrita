package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/audit"
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

	// errTooManyAttempts reports a login refused for its rate rather than its
	// credential. It is deliberately the same answer whether or not the address
	// exists, so the limit cannot be used to enumerate one.
	errTooManyAttempts = errors.New("ilavrita: too many login attempts; wait before trying again")

	// errMalformedLogin reports a body this server cannot read as a login.
	errMalformedLogin = errors.New("ilavrita: a login names a project, an email address and a password")
)

// loginRequest is what a caller presents. The Project is named by slug, because
// that is what a person knows; the id is an internal identifier.
type loginRequest struct {
	Project  string `json:"project"`
	Email    string `json:"email"`
	Password string `json:"password"`

	// Code is the second factor, when the identity has one. It is carried with
	// the password rather than asked for afterwards: a server that answered
	// "now the code, please" would be saying the password was right, and would
	// say it to anyone who guessed an address that exists.
	//
	// So a client offers the field always and fills it when its user has a
	// factor, which is something the person knows and the server never says.
	Code string `json:"code,omitempty"`
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

	base.POST(factorPath, enrolSecondFactor)
	base.DELETE(factorPath, withdrawSecondFactor)
	base.POST(factorActivatePath, activateSecondFactor)
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

	// Asked before the password is proved: argon2id is expensive by design, and
	// answering a guess is the work an attacker wants this server to do.
	identity := serving.attempts.identity(body.Project, body.Email)
	address := serving.attempts.address(request.RemoteIP())

	if err := serving.attempts.permits(request.Request.Context(), identity, address); err != nil {
		reason := audit.ReasonThrottled
		if !errors.Is(err, errTooManyAttempts) {
			reason = audit.ReasonUnavailable
		}

		serving.refusedLogin(request.Request.Context(), audit.OutcomeRefused, reason)

		return refuse(request, err)
	}

	var (
		issued project.Session
		token  project.SessionToken
	)

	// The session and the record of it share one commit boundary: a token
	// handed out by a process that then failed to write down that it had is a
	// credential nothing accounts for (AUD-1).
	err := serving.resources.WithinTransaction(request.Request.Context(), func(ctx context.Context) error {
		var err error

		issued, token, err = serving.authenticate(ctx, body)
		if err != nil {
			return err
		}

		return serving.recordLogin(ctx, issued)
	})
	if err != nil {
		outcome, reason := audit.OutcomeFailed, audit.ReasonUnavailable

		// A fault is not a wrong password. Counting one would let an outage in
		// this server lock out the people whose credentials are correct.
		if errors.Is(err, errCredentialsRefused) {
			outcome, reason = audit.OutcomeRefused, audit.ReasonNotAuthorized

			serving.attempts.failed(request.Request.Context(), identity, address)
		}

		// Recorded outside the transaction that failed, so it survives the
		// rollback.
		serving.refusedLogin(request.Request.Context(), outcome, reason)

		return refuse(request, err)
	}

	serving.attempts.succeeded(request.Request.Context(), identity)

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

	if err := b.proveSecondFactor(ctx, user.ID(), body.Code); err != nil {
		return project.Session{}, project.SessionToken{}, err
	}

	return b.issue(ctx, owner.ID(), user.ID(), standing.ID())
}

// proveSecondFactor requires the code when the identity has a factor it proved.
//
// A refusal is errCredentialsRefused, the same answer a wrong password gets: a
// distinct one would tell whoever is guessing that the password was right, and
// that this identity exists and has a second factor.
//
// A pending factor requires nothing. It was enrolled and never proved, and a
// person who scanned the code and then lost the phone must still be able to log
// in and start again.
func (b *backend) proveSecondFactor(ctx context.Context, user project.UserID, code string) error {
	if !b.factors.Available() {
		return nil
	}

	factor, enrolled, err := b.factors.Enrolled(ctx, user)
	if err != nil {
		// A factor this deployment cannot open is not one anybody proved.
		// Treating it as absent would turn a misconfigured key into a way past
		// everyone's second factor.
		return err
	}

	if !enrolled || !factor.Required() {
		return nil
	}

	proved, err := factor.Prove(code, b.clock())
	if err != nil {
		return errCredentialsRefused
	}

	if err := b.factors.Prove(ctx, proved); err != nil {
		// Only a refused code is a refused credential. A fault in this server is
		// not one: reporting it as a wrong password would both lie and count an
		// outage against the throttle of somebody whose code was right.
		if errors.Is(err, project.ErrCodeRefused) {
			return errCredentialsRefused
		}

		return err
	}

	return nil
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
	// Answered from the lookup already performed for this request, when one was.
	if held, carried := request.Request.Context().Value(sessionKey{}).(resolvedSession); carried {
		return held.session, held.found, held.err
	}

	return b.lookUpSession(request).unpack()
}

// sessionKey carries one request's session lookup, so nothing looks it up twice
// and nothing can answer differently the second time.
type sessionKey struct{}

// resolvedSession is what one lookup came to, error included: a lookup that
// failed must fail the same way for every reader of it.
type resolvedSession struct {
	session project.Session
	found   bool
	err     error
}

func (r resolvedSession) unpack() (project.Session, bool, error) {
	return r.session, r.found, r.err
}

// lookUpSession turns the presented token back into a session.
func (b *backend) lookUpSession(request *core.RequestEvent) resolvedSession {
	session, found, err := b.readSession(request)

	return resolvedSession{session: session, found: found, err: err}
}

func (b *backend) readSession(request *core.RequestEvent) (project.Session, bool, error) {
	// A backend with no session port names nobody. It is a wiring mistake rather
	// than a decision, but the answer that fails closed is the same one.
	if b.sessions == nil {
		return project.Session{}, false, nil
	}

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

// recordLogin writes down one credential this server proved. It returns its
// error so the caller inside the transaction fails with it: a token handed out
// by a process that then failed to record it is a credential nothing accounts
// for (AUD-1).
func (b *backend) recordLogin(ctx context.Context, issued project.Session) error {
	return b.record(ctx, audit.EventConfig{
		Project:    issued.Project(),
		Principal:  issued.Principal(),
		Membership: issued.Membership(),
		Action:     audit.ActionAuthenticate,
		Outcome:    audit.OutcomeAllowed,
	})
}

// refusedLogin writes down one attempt this server did not honour.
//
// It names nobody and no Project. The route answers a wrong password and an
// unknown address alike, and a record naming the user it found would say which
// of the checks got that far — turning the trail into the address oracle that
// the uniform answer exists to prevent (AUD-5). What an operator needs is still
// there: refusals are counted, and a run of them is the signal, not which
// address each one guessed at.
//
// Nothing is left to undo by the time this is called, so a record that cannot
// be written is reported rather than changing the answer the caller was given.
func (b *backend) refusedLogin(ctx context.Context, outcome audit.Outcome, reason audit.Reason) {
	err := b.record(ctx, audit.EventConfig{
		Project:   project.SystemScope,
		Principal: audit.UnidentifiedPrincipal,
		Action:    audit.ActionAuthenticate,
		Outcome:   outcome,
		Reason:    reason,
	})
	if err != nil {
		report(err)
	}
}
