package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The install every control-plane case administers.
const (
	superSlug     = "super"
	founderEmail  = "founder@example.test"
	adminPassword = "a rather long founder password"
)

// administeredServer provisions an install, claims it, and returns routes plus a
// Super Admin's token. The claim is the real bootstrap: standing in the Super
// Project comes from spending the token and from nowhere else.
func administeredServer(t *testing.T) (http.Handler, string, *sql.DB) {
	t.Helper()

	db := preparedDatabase(t)
	ctx := context.Background()

	projects := sqlite.NewProjectStore(db)

	super, err := project.NewSuperProject("prj_super", superSlug, "Super Project")
	if err != nil {
		t.Fatalf("NewSuperProject: %v", err)
	}

	boot := project.NewBootstrapper(projects, nil, rand.Reader)

	provisioning, err := boot.Provision(ctx, project.ProvisionConfig{
		SuperProject: super, TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	founder := credentialledIdentity(t, db, "prj_super", "usr_founder", founderEmail, adminPassword)

	if _, err := boot.Claim(ctx, provisioning.Token, founder); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	serve(t, &backend{
		resources:    sqlite.NewResourceStore(db),
		users:        sqlite.NewUserStore(db),
		projects:     projects,
		sessions:     sqlite.NewSessionStore(db),
		memberships:  sqlite.NewMembershipStore(db),
		applications: sqlite.NewClientApplicationStore(db),
		attempts:     newAttemptLimiter(sqlite.NewAttemptStore(db), nil),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	})

	routes := controlRoutes(t)

	return routes, tokenFrom(t, logInAs(t, routes, superSlug, founderEmail, adminPassword)), db
}

// credentialledIdentity writes one identity that can log in, and returns the
// principal it authenticates as.
func credentialledIdentity(
	t *testing.T, db *sql.DB, owner project.ID, id project.UserID, address, password string,
) project.PrincipalRef {
	t.Helper()

	ctx := context.Background()

	email, err := project.NormaliseEmail(address)
	if err != nil {
		t.Fatalf("NormaliseEmail: %v", err)
	}

	invited, err := project.NewProjectUser(owner, project.UserConfig{
		ID: id, Email: email, State: project.UserInvited,
	})
	if err != nil {
		t.Fatalf("NewProjectUser: %v", err)
	}

	users := sqlite.NewUserStore(db)

	version, err := users.Create(ctx, invited)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}

	hash, err := project.HashPassword(password, rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if _, err := users.AcceptInvitation(ctx, id, hash, version); err != nil {
		t.Fatalf("accept the invitation for %s: %v", id, err)
	}

	return project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(id)}
}

// controlRoutes builds the mux the process serves for auth and administration.
func controlRoutes(t *testing.T) http.Handler {
	t.Helper()

	publishAt(t, testHost)

	routes := router.NewRouter(
		func(response http.ResponseWriter, request *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
			return &core.RequestEvent{Event: router.Event{Response: response, Request: request}}, nil
		})

	registerAuthRoutes(routes)
	registerControlRoutes(routes)

	mux, err := routes.BuildMux()
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}

	return mux
}

// logInAs logs in against any Project slug.
func logInAs(t *testing.T, routes http.Handler, slug, address, password string) *httptest.ResponseRecorder {
	t.Helper()

	body := `{"project":"` + slug + `","email":"` + address + `","password":"` + password + `"}`

	sent := httptest.NewRequest(http.MethodPost, authBasePath+loginPath, strings.NewReader(body))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/json")

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// administer sends one control-plane request carrying a token.
func administer(
	t *testing.T, routes http.Handler, method, path, token string, body any,
) *httptest.ResponseRecorder {
	t.Helper()

	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatalf("encode the request: %v", err)
		}
	}

	sent := httptest.NewRequest(method, path, &encoded)
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/json")

	if token != "" {
		sent.Header.Set(authorizationField, bearerPrefix+token)
	}

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// TestAnInstallIsAdministeredEndToEnd walks the whole surface: a Super Admin
// creates a Project, invites someone into it, grants them standing, and the
// person they invited logs in. Before this there was no route for any of it.
func TestAnInstallIsAdministeredEndToEnd(t *testing.T) {
	routes, token, _ := administeredServer(t)

	created := administer(t, routes, http.MethodPost, controlBasePath+"/projects", token,
		createProjectRequest{ID: "clinic-b", Slug: "clinic-b", Name: "Clinic B"})
	assertStatus(t, created, http.StatusCreated)

	read := administer(t, routes, http.MethodGet, controlBasePath+"/projects/clinic-b", token, nil)
	assertStatus(t, read, http.StatusOK)

	var described projectResponse
	if err := json.Unmarshal(read.Body.Bytes(), &described); err != nil {
		t.Fatalf("decode the project: %v", err)
	}

	if described.Slug != "clinic-b" || described.Kind != string(project.KindStandard) {
		t.Errorf("the project reads %+v", described)
	}

	invited := administer(t, routes, http.MethodPost, controlBasePath+"/projects/clinic-b/users", token,
		createIdentityRequest{ID: "usr_nurse", Email: "nurse@example.test", Password: "another long password"})
	assertStatus(t, invited, http.StatusCreated)

	granted := administer(t, routes, http.MethodPost, controlBasePath+"/projects/clinic-b/memberships", token,
		createMembershipRequest{ID: "pm_nurse", User: "usr_nurse"})
	assertStatus(t, granted, http.StatusCreated)

	// The identity the operator just created can now log in on its own.
	assertStatus(t, logInAs(t, routes, "clinic-b", "nurse@example.test", "another long password"),
		http.StatusOK)
}

// TestRegisteringAClientApplicationReturnsItsSecretOnce. The secret appears in
// this response and nowhere else, ever.
func TestRegisteringAClientApplicationReturnsItsSecretOnce(t *testing.T) {
	routes, token, db := administeredServer(t)

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", token,
		createProjectRequest{ID: "clinic-b", Slug: "clinic-b", Name: "Clinic B"}), http.StatusCreated)

	registered := administer(t, routes, http.MethodPost,
		controlBasePath+"/projects/clinic-b/client-applications", token,
		createApplicationRequest{Name: "Nightly loader"})
	assertStatus(t, registered, http.StatusCreated)

	var described applicationResponse
	if err := json.Unmarshal(registered.Body.Bytes(), &described); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}

	if described.Secret == "" || !strings.HasPrefix(described.ID, "cli_") {
		t.Fatalf("the registration reads %+v", described)
	}

	// The stored row holds a digest, never the secret that was handed out.
	var stored int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM client_application_credentials WHERE secret_hash = ?",
		described.Secret).Scan(&stored); err != nil {
		t.Fatalf("count stored secrets: %v", err)
	}

	if stored != 0 {
		t.Error("the issued secret is stored verbatim")
	}
}

// TestTheControlPlaneRefusesAnUnauthenticatedRequest. Who is asking is prior to
// what they may do, and the control plane is where that matters most.
func TestTheControlPlaneRefusesAnUnauthenticatedRequest(t *testing.T) {
	routes, _, _ := administeredServer(t)

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", "",
		createProjectRequest{ID: "clinic-b", Slug: "clinic-b", Name: "Clinic B"}), http.StatusUnauthorized)

	assertStatus(t, administer(t, routes, http.MethodGet,
		controlBasePath+"/projects/clinic-b", "invented", nil), http.StatusUnauthorized)
}

// TestAnOrdinaryMemberAdministersNothing. Being a member is not enough: the
// control plane is the one surface where standing has to be administrative.
func TestAnOrdinaryMemberAdministersNothing(t *testing.T) {
	routes, token, _ := administeredServer(t)

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", token,
		createProjectRequest{ID: "clinic-b", Slug: "clinic-b", Name: "Clinic B"}), http.StatusCreated)

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects/clinic-b/users", token,
		createIdentityRequest{ID: "usr_nurse", Email: "nurse@example.test", Password: "another long password"}),
		http.StatusCreated)

	assertStatus(t, administer(t, routes, http.MethodPost,
		controlBasePath+"/projects/clinic-b/memberships", token,
		createMembershipRequest{ID: "pm_nurse", User: "usr_nurse"}), http.StatusCreated)

	nurse := tokenFrom(t, logInAs(t, routes, "clinic-b", "nurse@example.test", "another long password"))

	// An ordinary member holds no administrative standing anywhere.
	assertStatus(t, administer(t, routes, http.MethodGet,
		controlBasePath+"/projects/clinic-b", nurse, nil), http.StatusForbidden)

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", nurse,
		createProjectRequest{ID: "clinic-c", Slug: "clinic-c", Name: "Clinic C"}), http.StatusForbidden)
}

// TestAProjectAdminAdministersOnlyItsOwnProject. A token issued for one Project
// cannot administer another by naming it in the path.
func TestAProjectAdminAdministersOnlyItsOwnProject(t *testing.T) {
	routes, token, _ := administeredServer(t)

	for _, id := range []string{"clinic-b", "clinic-c"} {
		assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", token,
			createProjectRequest{ID: id, Slug: id, Name: id}), http.StatusCreated)
	}

	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects/clinic-b/users", token,
		createIdentityRequest{ID: "usr_admin", Email: "admin@example.test", Password: "a long admin password"}),
		http.StatusCreated)

	assertStatus(t, administer(t, routes, http.MethodPost,
		controlBasePath+"/projects/clinic-b/memberships", token,
		createMembershipRequest{ID: "pm_admin", User: "usr_admin", Admin: true}), http.StatusCreated)

	local := tokenFrom(t, logInAs(t, routes, "clinic-b", "admin@example.test", "a long admin password"))

	// Its own Project, yes.
	assertStatus(t, administer(t, routes, http.MethodGet,
		controlBasePath+"/projects/clinic-b", local, nil), http.StatusOK)

	// Another Project, no — however the path names it.
	assertStatus(t, administer(t, routes, http.MethodGet,
		controlBasePath+"/projects/clinic-c", local, nil), http.StatusForbidden)

	// And a Project admin does not administer the install.
	assertStatus(t, administer(t, routes, http.MethodPost, controlBasePath+"/projects", local,
		createProjectRequest{ID: "clinic-d", Slug: "clinic-d", Name: "Clinic D"}), http.StatusForbidden)
}

// TestTheSuperProjectSlugIsNotTakeableOverHTTP. It is provisioned once, with the
// install record, so there is no second way to bring one into existence.
func TestTheSuperProjectSlugIsNotTakeableOverHTTP(t *testing.T) {
	routes, token, _ := administeredServer(t)

	answer := administer(t, routes, http.MethodPost, controlBasePath+"/projects", token,
		createProjectRequest{ID: "prj_second_super", Slug: superSlug, Name: "Another super"})

	if answer.Code == http.StatusCreated {
		t.Errorf("a second project took the super project's slug: %s", answer.Body.String())
	}
}
