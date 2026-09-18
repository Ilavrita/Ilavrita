package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The control plane. It is deliberately not under /fhir/R4: nothing here is a
// FHIR resource, and an OperationOutcome would misdescribe a Project.
const (
	controlBasePath  = "/admin"
	projectsPath     = "/projects"
	projectPath      = "/projects/{project}"
	membershipsPath  = "/projects/{project}/memberships"
	identitiesPath   = "/projects/{project}/users"
	applicationsPath = "/projects/{project}/client-applications"
	projectParameter = "project"
)

// credentialLifetime is how long a client application's first secret lives. The
// domain caps it at ninety days; this is what this build issues.
const credentialLifetime = 30 * 24 * time.Hour

var (
	// errNotAdmin reports a control-plane request from standing that does not
	// administer the Project it names. It is separate from errNoPrincipal, because
	// being unauthenticated and being unprivileged are different answers.
	errNotAdmin = errors.New("ilavrita: this principal does not administer that project")

	// errNotSuperAdmin reports a server-level request from standing that is not
	// Super Admin. Creating a Project is not a Project's own business.
	errNotSuperAdmin = errors.New("ilavrita: this principal does not administer the install")

	// errMalformedControlRequest reports a body this server cannot read.
	errMalformedControlRequest = errors.New("ilavrita: this request body names nothing this server can act on")

	// errUnknownProject reports a Project that names no row.
	errUnknownProject = errors.New("ilavrita: no such project")
)

// createProjectRequest asks for one Project.
type createProjectRequest struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
}

// projectResponse describes one Project.
type projectResponse struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	Slug              string `json:"slug"`
	Name              string `json:"name"`
	State             string `json:"state"`
	Environment       string `json:"environment"`
	AllowClinicalData bool   `json:"allowClinicalData"`
}

// createIdentityRequest invites one identity into a Project.
type createIdentityRequest struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// identityResponse describes an invited identity. It carries no credential: the
// password the caller set is proved at login and never read back.
type identityResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	State string `json:"state"`
}

// createMembershipRequest grants one identity standing in the Project.
type createMembershipRequest struct {
	ID       string   `json:"id"`
	User     string   `json:"user"`
	Admin    bool     `json:"admin"`
	Policies []string `json:"policies"`
}

// membershipResponse describes standing as it was written.
type membershipResponse struct {
	ID       string   `json:"id"`
	Project  string   `json:"project"`
	User     string   `json:"user"`
	State    string   `json:"state"`
	Admin    bool     `json:"admin"`
	Policies []string `json:"policies,omitempty"`
}

// createApplicationRequest registers one programmatic caller.
type createApplicationRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// applicationResponse describes a registration and, once, the secret it was
// issued. The secret is never readable again: only its digest is stored.
type applicationResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Secret    string `json:"secret,omitempty"`
	ExpiresAt string `json:"secretExpiresAt,omitempty"`
}

// registerControlRoutes publishes the surface an operator administers through.
// Every route here resolves standing first: the control plane is the one place
// where being a member is not enough.
func registerControlRoutes(routes *router.Router[*core.RequestEvent]) {
	base := routes.Group(controlBasePath)

	// A control-plane token is a bearer credential, so this surface answers no
	// preflight and carries no cross-origin headers at all.
	base.Unbind(apis.DefaultCorsMiddlewareId)

	base.POST(projectsPath, createProject)
	base.GET(projectPath, readProject)
	base.POST(identitiesPath, inviteIdentity)
	base.POST(membershipsPath, grantMembership)
	base.POST(applicationsPath, registerApplication)
}

// administers resolves the standing a control-plane request rests on. It reads
// the session's own Project, never the one the path names, so a token issued for
// one Project cannot administer another by asking.
func (b *backend) administers(request *core.RequestEvent, named project.ID) error {
	standing, session, err := b.standing(request)
	if err != nil {
		return err
	}

	// Super Admin administers every Project; anyone else administers only the one
	// their standing is in, and only while that standing is administrative.
	if standing.IsSuperAdmin() {
		return nil
	}

	if !standing.IsAdmin() || session.Project() != named {
		return errNotAdmin
	}

	return nil
}

// administersInstall resolves Super Admin, which is what a server-level write
// rests on. A Project's own admin holds nothing here.
func (b *backend) administersInstall(request *core.RequestEvent) error {
	standing, _, err := b.standing(request)
	if err != nil {
		return err
	}

	if !standing.IsSuperAdmin() {
		return errNotSuperAdmin
	}

	return nil
}

// standing resolves the session's own membership, which is the only standing a
// control-plane decision may rest on.
func (b *backend) standing(request *core.RequestEvent) (project.Membership, project.Session, error) {
	session, found, err := b.session(request)
	if err != nil {
		return project.Membership{}, project.Session{}, err
	}

	if !found {
		return project.Membership{}, project.Session{}, errNoPrincipal
	}

	held, resolved, err := b.resolvers.Memberships.Membership(
		request.Request.Context(), session.Project(), session.Principal())
	if err != nil {
		return project.Membership{}, project.Session{}, err
	}

	if !resolved || !held.HoldsStanding() {
		return project.Membership{}, project.Session{}, errNoPrincipal
	}

	return held, session, nil
}

// createProject provisions one ordinary Project. The Super Project is not
// creatable here: it is provisioned once, with the install record.
func createProject(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	if err := serving.administersInstall(request); err != nil {
		return refuse(request, err)
	}

	var body createProjectRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedControlRequest)
	}

	described, err := project.NewProject(project.Config{
		ID: project.ID(body.ID), Slug: body.Slug, Name: body.Name,
		State: project.StateActive, Environment: body.Environment, AllowClinicalData: true,
	})
	if err != nil {
		return refuse(request, err)
	}

	if _, err := serving.projects.Create(request.Request.Context(), described); err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, describeProject(described))
}

// readProject answers with one Project, for an operator that administers it.
func readProject(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	named := project.ID(request.Request.PathValue(projectParameter))

	if err := serving.administers(request, named); err != nil {
		return refuse(request, err)
	}

	described, _, found, err := serving.projects.ByID(request.Request.Context(), named)
	if err != nil {
		return refuse(request, err)
	}

	if !found {
		return refuse(request, errUnknownProject)
	}

	return request.JSON(http.StatusOK, describeProject(described))
}

// inviteIdentity creates one project-scoped identity and sets its credential, so
// an operator can hand someone a way in without a second round trip.
func inviteIdentity(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	named := project.ID(request.Request.PathValue(projectParameter))

	if err := serving.administers(request, named); err != nil {
		return refuse(request, err)
	}

	var body createIdentityRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedControlRequest)
	}

	invited, err := serving.invite(request.Request.Context(), named, body)
	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, identityResponse{
		ID: string(invited.ID()), Email: invited.Email().Display(), State: string(project.UserActive),
	})
}

// invite writes the identity and accepts its invitation in one sequence, so no
// identity is left in a state nobody can act on.
func (b *backend) invite(
	ctx context.Context, owner project.ID, body createIdentityRequest,
) (project.User, error) {
	email, err := project.NormaliseEmail(body.Email)
	if err != nil {
		return project.User{}, err
	}

	invited, err := project.NewProjectUser(owner, project.UserConfig{
		ID: project.UserID(body.ID), Email: email, State: project.UserInvited,
	})
	if err != nil {
		return project.User{}, err
	}

	version, err := b.users.Create(ctx, invited)
	if err != nil {
		return project.User{}, err
	}

	hash, err := project.HashPassword(body.Password, rand.Reader)
	if err != nil {
		return project.User{}, err
	}

	if _, err := b.users.AcceptInvitation(ctx, invited.ID(), hash, version); err != nil {
		return project.User{}, err
	}

	return invited, nil
}

// grantMembership gives one identity standing in the Project, with whatever
// policies the caller names. The domain decides what that standing may hold.
func grantMembership(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	named := project.ID(request.Request.PathValue(projectParameter))

	if err := serving.administers(request, named); err != nil {
		return refuse(request, err)
	}

	var body createMembershipRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedControlRequest)
	}

	member, err := describedMembership(named, body)
	if err != nil {
		return refuse(request, err)
	}

	if err := serving.memberships.Create(request.Request.Context(), member); err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, membershipResponse{
		ID: string(member.ID()), Project: string(member.Project()),
		User: string(member.Principal().ID), State: string(member.State()),
		Admin: member.StoredAdmin(), Policies: body.Policies,
	})
}

// describedMembership builds the standing a request asked for, in the Project the
// path names rather than any the body might.
func describedMembership(named project.ID, body createMembershipRequest) (project.Membership, error) {
	attachments := make([]project.PolicyAttachment, 0, len(body.Policies))
	for index, policy := range body.Policies {
		attachments = append(attachments, project.PolicyAttachment{
			Policy: storage.LogicalID(policy), Ordinal: index,
		})
	}

	return project.NewMembership(project.MembershipConfig{
		ID: project.MembershipID(body.ID), Project: named, ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(body.User)},
		State:     project.MembershipActive, Admin: body.Admin,
		Policies: attachments, Source: project.SourceAPI,
	})
}

// registerApplication registers one programmatic caller and issues its first
// secret. The secret is returned once and never readable again.
func registerApplication(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	named := project.ID(request.Request.PathValue(projectParameter))

	if err := serving.administers(request, named); err != nil {
		return refuse(request, err)
	}

	var body createApplicationRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedControlRequest)
	}

	registered, secret, credential, err := serving.register(request.Request.Context(), named, body)
	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, applicationResponse{
		ID: string(registered.ID()), Name: registered.Name(), State: string(registered.State()),
		Secret: secret.Reveal(), ExpiresAt: credential.ExpiresAt().Format(time.RFC3339),
	})
}

// register mints the registration and its first credential together, so a caller
// never receives an application it cannot authenticate as.
func (b *backend) register(
	ctx context.Context, owner project.ID, body createApplicationRequest,
) (project.ClientApplication, project.ClientSecret, project.Credential, error) {
	var (
		none     project.ClientApplication
		noSecret project.ClientSecret
		noRecord project.Credential
	)

	id, err := project.MintClientApplicationID(rand.Reader)
	if err != nil {
		return none, noSecret, noRecord, err
	}

	registered, err := project.NewClientApplication(owner, project.ClientApplicationConfig{
		ID: id, Name: body.Name, Description: body.Description, State: project.ServiceActive,
	})
	if err != nil {
		return none, noSecret, noRecord, err
	}

	if _, err := b.applications.Create(ctx, registered); err != nil {
		return none, noSecret, noRecord, err
	}

	credentialID, err := project.MintCredentialID(rand.Reader)
	if err != nil {
		return none, noSecret, noRecord, err
	}

	issuedAt := time.Now().UTC()

	credential, secret, err := registered.IssueCredential(project.CredentialConfig{
		ID: credentialID, CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(credentialLifetime),
	}, rand.Reader)
	if err != nil {
		return none, noSecret, noRecord, err
	}

	if err := b.applications.IssueCredential(ctx, credential); err != nil {
		return none, noSecret, noRecord, err
	}

	return registered, secret, credential, nil
}

// describeProject renders one Project for an operator.
func describeProject(described project.Project) projectResponse {
	return projectResponse{
		ID: string(described.ID()), Kind: string(described.Kind()), Slug: described.Slug(),
		Name: described.Name(), State: string(described.State()),
		Environment: described.Environment(), AllowClinicalData: described.AllowsClinicalData(),
	}
}
