package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The identity every case below logs in as.
const (
	loginSlug     = "clinic-a"
	loginAddress  = "nurse@example.test"
	loginPassword = "correct horse battery staple"
)

// authenticatedServer wires the process's own stores against a real database and
// registers the routes it actually serves. Nothing here is a stub: what these
// tests assert is what a request reaches.
func authenticatedServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()

	db := preparedDatabase(t)
	seedLoginFixtures(t, db)

	serve(t, &backend{
		resources: sqlite.NewResourceStore(db),
		users:     sqlite.NewUserStore(db),
		projects:  sqlite.NewProjectStore(db),
		sessions:  sqlite.NewSessionStore(db),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	})

	return allRoutes(t), db
}

// seedLoginFixtures provisions a Project, a credentialled identity, the standing
// it holds and the policy that standing resolves to — through the real stores, so
// the fixture is the control plane rather than a hand-written row.
func seedLoginFixtures(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := context.Background()

	clinic, err := project.NewProject(project.Config{
		ID: "clinic-a", Slug: loginSlug, Name: "Clinic A", State: project.StateActive,
	})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}

	if _, err := sqlite.NewProjectStore(db).Create(ctx, clinic); err != nil {
		t.Fatalf("create the project: %v", err)
	}

	email, err := project.NormaliseEmail(loginAddress)
	if err != nil {
		t.Fatalf("NormaliseEmail: %v", err)
	}

	invited, err := project.NewProjectUser("clinic-a", project.UserConfig{
		ID: "usr_nurse", Email: email, State: project.UserInvited,
	})
	if err != nil {
		t.Fatalf("NewProjectUser: %v", err)
	}

	users := sqlite.NewUserStore(db)

	version, err := users.Create(ctx, invited)
	if err != nil {
		t.Fatalf("create the identity: %v", err)
	}

	hash, err := project.HashPassword(loginPassword, rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if _, err := users.AcceptInvitation(ctx, "usr_nurse", hash, version); err != nil {
		t.Fatalf("accept the invitation: %v", err)
	}

	seedLoginPolicy(t, db)

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_nurse", Project: "clinic-a", ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_nurse"},
		State:     project.MembershipActive, Source: project.SourceInvite,
		Policies: []project.PolicyAttachment{{Policy: "pol_ward"}},
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	if err := sqlite.NewMembershipStore(db).Create(ctx, member); err != nil {
		t.Fatalf("create the membership: %v", err)
	}
}

// seedLoginPolicy writes the one policy the membership binds: unrestricted over
// Organization, which is legal because Organization carries no patient data.
func seedLoginPolicy(t *testing.T, db *sql.DB) {
	t.Helper()

	statements := []string{
		"INSERT INTO access_policies (project_id, id, name, created_at, updated_at)" +
			" VALUES ('clinic-a', 'pol_ward', 'Ward', 0, 0)",
		"INSERT INTO access_policy_rules (project_id, policy_id, ordinal, kind, res_type, action," +
			" unrestricted, compartment_type, compartment_id, compartment_param) VALUES" +
			" ('clinic-a', 'pol_ward', 0, 'fhir', 'Organization', 'read', 1, NULL, NULL, NULL)," +
			" ('clinic-a', 'pol_ward', 1, 'fhir', 'Organization', 'write', 1, NULL, NULL, NULL)",
	}

	for _, statement := range statements {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("seed the policy: %v", err)
		}
	}
}

// allRoutes builds the mux the process serves, auth and FHIR together, so route
// matching is part of what these tests assert.
func allRoutes(t *testing.T) http.Handler {
	t.Helper()

	publishAt(t, testHost)

	routes := router.NewRouter(
		func(response http.ResponseWriter, request *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
			return &core.RequestEvent{Event: router.Event{Response: response, Request: request}}, nil
		})

	registerAuthRoutes(routes)
	registerFHIRRoutes(routes)

	mux, err := routes.BuildMux()
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}

	return mux
}

// logInOver performs one login against the wired routes.
func logInOver(t *testing.T, routes http.Handler, address, password string) *httptest.ResponseRecorder {
	t.Helper()

	body := `{"project":"` + loginSlug + `","email":"` + address + `","password":"` + password + `"}`

	sent := httptest.NewRequest(http.MethodPost, authBasePath+loginPath, strings.NewReader(body))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/json")

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// bearer sends one request carrying a session token, or none.
func bearer(t *testing.T, routes http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(method, path, strings.NewReader(body))
	sent.Host = testHost

	if body != "" {
		sent.Header.Set(contentTypeField, fhir.ContentType)
	}

	if token != "" {
		sent.Header.Set(authorizationField, bearerPrefix+token)
	}

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// tokenFrom reads the token a successful login returned.
func tokenFrom(t *testing.T, answer *httptest.ResponseRecorder) string {
	t.Helper()

	var body loginResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the login response: %v; body %s", err, answer.Body.String())
	}

	if body.Token == "" {
		t.Fatalf("the login returned no token: %s", answer.Body.String())
	}

	return body.Token
}

// TestALoginTurnsACredentialIntoAReachableRequest is the whole point of this
// step. Before it, the only principal came from an environment variable that
// checked no credential; after it, a password reaches storage.
func TestALoginTurnsACredentialIntoAReachableRequest(t *testing.T) {
	routes, _ := authenticatedServer(t)

	// Nothing is reachable without a token.
	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", "", ""),
		http.StatusUnauthorized)

	answer := logInOver(t, routes, loginAddress, loginPassword)
	assertStatus(t, answer, http.StatusOK)

	token := tokenFrom(t, answer)

	// The same request now authenticates. The Organization does not exist, so 404
	// is the right answer — and it is emphatically not 401.
	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
		http.StatusNotFound)

	// A write the bound policy permits goes all the way to storage.
	created := bearer(t, routes, http.MethodPost, "/fhir/R4/Organization", token,
		`{"resourceType":"Organization","name":"Ward"}`)
	assertStatus(t, created, http.StatusCreated)

	read := bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/"+resourceID(t, created), token, "")
	assertStatus(t, read, http.StatusOK)
}

// TestAWrongPasswordIsRefusedTheSameWayAsAnUnknownAddress, because telling them
// apart tells an attacker which addresses exist.
func TestAWrongPasswordIsRefusedTheSameWayAsAnUnknownAddress(t *testing.T) {
	routes, _ := authenticatedServer(t)

	wrong := logInOver(t, routes, loginAddress, "not the password")
	stranger := logInOver(t, routes, "stranger@example.test", loginPassword)

	assertStatus(t, wrong, http.StatusUnauthorized)
	assertStatus(t, stranger, http.StatusUnauthorized)

	if wrong.Body.String() != stranger.Body.String() {
		t.Errorf("the two refusals differ:\n  %s\n  %s", wrong.Body.String(), stranger.Body.String())
	}
}

// TestALoginReturnsNoCredentialMaterial. The token appears once; nothing else
// about the identity does.
func TestALoginReturnsNoCredentialMaterial(t *testing.T) {
	routes, _ := authenticatedServer(t)

	answer := logInOver(t, routes, loginAddress, loginPassword)
	assertStatus(t, answer, http.StatusOK)

	body := answer.Body.String()
	for _, forbidden := range []string{"argon2id", loginPassword} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the login response carries %q: %s", forbidden, body)
		}
	}
}

// TestLoggingOutStopsTheTokenReachingAnything. Revocation destroys the material,
// so the next request is refused rather than served from a cache that forgot.
func TestLoggingOutStopsTheTokenReachingAnything(t *testing.T) {
	routes, _ := authenticatedServer(t)

	token := tokenFrom(t, logInOver(t, routes, loginAddress, loginPassword))

	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
		http.StatusNotFound)

	assertStatus(t, bearer(t, routes, http.MethodPost, authBasePath+logoutPath, token, ""),
		http.StatusNoContent)

	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
		http.StatusUnauthorized)
}

// TestAnInventedTokenReachesNothing. A token is a bearer, so the only thing that
// makes one real is a row this server minted.
func TestAnInventedTokenReachesNothing(t *testing.T) {
	routes, _ := authenticatedServer(t)

	for _, token := range []string{"invented", "ses_invented", strings.Repeat("A", 43)} {
		assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
			http.StatusUnauthorized)
	}
}

// TestTheSessionRouteNamesWhoTheTokenIs, so a client holding one does not have to
// remember what it logged in as.
func TestTheSessionRouteNamesWhoTheTokenIs(t *testing.T) {
	routes, _ := authenticatedServer(t)

	token := tokenFrom(t, logInOver(t, routes, loginAddress, loginPassword))

	answer := bearer(t, routes, http.MethodGet, authBasePath+sessionPath, token, "")
	assertStatus(t, answer, http.StatusOK)

	var described sessionResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &described); err != nil {
		t.Fatalf("decode the session: %v", err)
	}

	if described.User != "usr_nurse" || described.Project != "clinic-a" || described.Membership != "pm_nurse" {
		t.Errorf("the session describes %+v", described)
	}

	if described.Admin || described.SuperAdmin {
		t.Errorf("an ordinary member is described as privileged: %+v", described)
	}
}

// TestADisabledIdentityCannotLogInOverHTTP. Disabling is the lever an operator
// pulls, and it must take effect at the door.
func TestADisabledIdentityCannotLogInOverHTTP(t *testing.T) {
	routes, db := authenticatedServer(t)

	users := sqlite.NewUserStore(db)

	_, version, found, err := users.ByID(context.Background(), "usr_nurse")
	if err != nil || !found {
		t.Fatalf("read the identity: found %t, err %v", found, err)
	}

	if _, err := users.UpdateState(context.Background(), "usr_nurse", project.UserDisabled, version); err != nil {
		t.Fatalf("disable: %v", err)
	}

	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusUnauthorized)
}

// TestARevokedMembershipStopsAnExistingSessionReaching. Standing is read on every
// decision, so withdrawing it takes hold without waiting for a token to expire.
func TestARevokedMembershipStopsAnExistingSessionReaching(t *testing.T) {
	routes, db := authenticatedServer(t)

	token := tokenFrom(t, logInOver(t, routes, loginAddress, loginPassword))

	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
		http.StatusNotFound)

	if _, err := sqlite.NewMembershipStore(db).UpdateState(
		context.Background(), "clinic-a", "pm_nurse", project.MembershipRevoked, 1); err != nil {
		t.Fatalf("revoke the membership: %v", err)
	}

	// The token is still live; the standing behind it is not.
	assertStatus(t, bearer(t, routes, http.MethodGet, "/fhir/R4/Organization/nothing", token, ""),
		http.StatusForbidden)
}
