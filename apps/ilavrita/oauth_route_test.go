package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// What every case below authorizes with.
const (
	appVerifier = "a-verifier-of-exactly-the-length-rfc7636-wants"
	appRedirect = "https://app.example.test/callback"
	appAudience = "http://" + testHost + fhir.BasePath
)

// appChallenge is the challenge a client derives from appVerifier.
func appChallenge() string {
	sum := sha256.Sum256([]byte(appVerifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// launchingServer wires the stores the SMART surface needs, over the same
// Project, identity, membership and policy the login tests use. The policy
// grants Organization read and write, so a scope narrowing to the read alone is
// visibly narrower than the person themselves.
func launchingServer(t *testing.T, kind project.ClientKind) (http.Handler, *sql.DB) {
	t.Helper()

	db := preparedDatabase(t)
	seedLoginFixtures(t, db)

	serve(t, &backend{
		resources:    sqlite.NewResourceStore(db),
		users:        sqlite.NewUserStore(db),
		projects:     sqlite.NewProjectStore(db),
		sessions:     sqlite.NewSessionStore(db),
		memberships:  sqlite.NewMembershipStore(db),
		applications: sqlite.NewClientApplicationStore(db),
		codes:        sqlite.NewAuthorizationCodeStore(db),
		audits:       sqlite.NewAuditStore(db),
		attempts:     newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, nil),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	})

	registerApp(t, db, kind)

	return allRoutes(t), db
}

// registerApp writes one registration for the tests to authorize.
func registerApp(t *testing.T, db *sql.DB, kind project.ClientKind) {
	t.Helper()

	addresses, err := project.NewRedirectURIs(appRedirect)
	if err != nil {
		t.Fatalf("NewRedirectURIs: %v", err)
	}

	app, err := project.NewClientApplication("clinic-a", project.ClientApplicationConfig{
		ID: "cli_ward", Name: "Ward app", State: project.ServiceActive,
		Kind: kind, RedirectURIs: addresses,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	if _, err := sqlite.NewClientApplicationStore(db).Create(context.Background(), app); err != nil {
		t.Fatalf("register the app: %v", err)
	}
}

// issueSecretFor gives a confidential registration the secret it authenticates
// with, and returns it.
//
// A confidential client that holds no credential at all is refused for the wrong
// reason — there is nothing to compare against — so a test about presenting the
// secret has to be run against one that has it.
func issueSecretFor(t *testing.T, db *sql.DB) string {
	t.Helper()

	ctx := context.Background()
	store := sqlite.NewClientApplicationStore(db)

	app, _, found, err := store.ByID(ctx, "clinic-a", "cli_ward")
	if err != nil || !found {
		t.Fatalf("read the registration: found %v, err %v", found, err)
	}

	issuedAt := time.Now().UTC()

	credential, secret, err := app.IssueCredential(project.CredentialConfig{
		ID: "cac_ward", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(30 * 24 * time.Hour),
	}, rand.Reader)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := store.IssueCredential(ctx, credential); err != nil {
		t.Fatalf("write the credential: %v", err)
	}

	return secret.Reveal()
}

// signedIn logs the nurse in and returns their session token.
func signedIn(t *testing.T, routes http.Handler) string {
	t.Helper()

	recorder := logInAs(t, routes, loginSlug, loginAddress, loginPassword)
	if recorder.Code != http.StatusOK {
		t.Fatalf("log in: %d %s", recorder.Code, recorder.Body)
	}

	var body struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the login: %v", err)
	}

	return body.Token
}

// askingFor is the authorization request a client makes.
func askingFor(scope string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {"cli_ward"},
		"redirect_uri":          {appRedirect},
		"scope":                 {scope},
		"state":                 {"a-state-the-client-chose"},
		"aud":                   {appAudience},
		"code_challenge":        {appChallenge()},
		"code_challenge_method": {"S256"},
	}
}

// authorizing sends one request to the authorization endpoint as the person.
func authorizing(
	t *testing.T, routes http.Handler, method, token string, ask url.Values, body string,
) *httptest.ResponseRecorder {
	t.Helper()

	path := oauthBasePath + authorizePath + "?" + ask.Encode()

	sent := httptest.NewRequest(method, path, strings.NewReader(body))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/json")

	if token != "" {
		sent.Header.Set(authorizationField, bearerPrefix+token)
	}

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// redeeming sends one request to the token endpoint.
func redeeming(t *testing.T, routes http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(
		http.MethodPost, oauthBasePath+tokenPath, strings.NewReader(form.Encode()))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// codeFrom pulls the authorization code out of the redirect the server built.
func codeFrom(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()

	var approved approvedResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &approved); err != nil {
		t.Fatalf("decode the approval: %v", err)
	}

	parsed, err := url.Parse(approved.Redirect)
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}

	if got := parsed.Query().Get("state"); got != "a-state-the-client-chose" {
		t.Errorf("the redirect carried state %q, want the one the client sent", got)
	}

	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatalf("the redirect carried no code: %s", approved.Redirect)
	}

	return code
}

// TestAnAppReachesLessThanThePersonWhoApprovedIt.
//
// This is the whole flow, and the whole point of it. The nurse may read and
// write Organizations. Their app is granted the read alone, so the write it
// never asked for must be refused — while the same nurse, on their own session,
// still holds it.
func TestAnAppReachesLessThanThePersonWhoApprovedIt(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	approving := authorizing(
		t, routes, http.MethodPost, person, askingFor("user/Organization.read"), "")
	if approving.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approving.Code, approving.Body)
	}

	redeemed := redeeming(t, routes, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, approving)},
		"redirect_uri":  {appRedirect},
		"client_id":     {"cli_ward"},
		"code_verifier": {appVerifier},
	})
	if redeemed.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", redeemed.Code, redeemed.Body)
	}

	var issued tokenResponse
	if err := json.Unmarshal(redeemed.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode the token: %v", err)
	}

	if issued.TokenType != "Bearer" || issued.AccessToken == "" {
		t.Fatalf("the token response carried %+v", issued)
	}

	if issued.Scope != "user/Organization.read" {
		t.Errorf("granted %q, want the scope that was approved", issued.Scope)
	}

	// The app may not write, though the person it acts for may.
	appWrites := organizationWrite(t, routes, issued.AccessToken)
	if appWrites == http.StatusCreated || appWrites == http.StatusOK {
		t.Errorf("an app granted only a read wrote anyway: %d", appWrites)
	}

	personWrites := organizationWrite(t, routes, person)
	if personWrites != http.StatusCreated && personWrites != http.StatusOK {
		t.Errorf("the person themselves was refused a write (%d), so the test above proves nothing",
			personWrites)
	}
}

// organizationWrite attempts a create and reports the status.
func organizationWrite(t *testing.T, routes http.Handler, token string) int {
	t.Helper()

	body := `{"resourceType":"Organization","name":"Ward"}`

	sent := httptest.NewRequest(
		http.MethodPost, fhir.BasePath+"/Organization", strings.NewReader(body))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, fhir.ContentType)
	sent.Header.Set(authorizationField, bearerPrefix+token)

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder.Code
}

// TestAnAuthorizationSaysWhatItWouldGrantAndWhatItRefuses, so a consent UI can
// show both and an app is never told it holds something it does not.
func TestAnAuthorizationSaysWhatItWouldGrantAndWhatItRefuses(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	// One scope this build grants, one it refuses outright, and one that is not
	// a scope at all.
	ask := askingFor("user/Organization.read system/Patient.read nonsense")

	described := authorizing(t, routes, http.MethodGet, person, ask, "")
	if described.Code != http.StatusOK {
		t.Fatalf("describe: %d %s", described.Code, described.Body)
	}

	var view authorizationView
	if err := json.Unmarshal(described.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode the view: %v", err)
	}

	if len(view.Grantable) != 1 || view.Grantable[0] != "user/Organization.read" {
		t.Errorf("grantable is %v, want the one scope this server grants", view.Grantable)
	}

	if len(view.Refused) != 2 {
		t.Fatalf("refused is %v, want the two this server will not grant", view.Refused)
	}

	for _, held := range view.Refused {
		if held.Reason == "" {
			t.Errorf("%q was refused with no reason", held.Scope)
		}
	}
}

// TestDescribingAnAuthorizationWritesNothing, because a GET that created an
// approval would be one a link could trigger.
func TestDescribingAnAuthorizationWritesNothing(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	if recorder := authorizing(
		t, routes, http.MethodGet, person, askingFor("user/Organization.read"), "",
	); recorder.Code != http.StatusOK {
		t.Fatalf("describe: %d %s", recorder.Code, recorder.Body)
	}

	if held := codesHeld(t, db); held != 0 {
		t.Errorf("describing an authorization wrote %d codes", held)
	}
}

// codesHeld counts the approvals waiting to be redeemed.
func codesHeld(t *testing.T, db *sql.DB) int {
	t.Helper()

	var held int

	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM authorization_codes").Scan(&held); err != nil {
		t.Fatalf("count: %v", err)
	}

	return held
}

// TestAnAuthorizationRequestIsRefusedBeforeItCanBecomeARedirect.
//
// Each of these is checked before a code exists, and every one is a way an app
// could otherwise have a code delivered somewhere it should not go, or bound to
// nothing it must prove.
func TestAnAuthorizationRequestIsRefusedBeforeItCanBecomeARedirect(t *testing.T) {
	for name, change := range map[string]func(url.Values){
		"an address nobody registered": func(v url.Values) {
			v.Set("redirect_uri", "https://evil.test/callback")
		},
		"an address that merely starts the same": func(v url.Values) {
			v.Set("redirect_uri", appRedirect+"/../evil")
		},
		"an audience naming somebody else": func(v url.Values) {
			v.Set("aud", "https://another.example.test/fhir/R4")
		},
		"no audience at all": func(v url.Values) { v.Del("aud") },
		"no PKCE challenge":  func(v url.Values) { v.Del("code_challenge") },
		"PKCE downgraded to plain": func(v url.Values) {
			v.Set("code_challenge_method", "plain")
			v.Set("code_challenge", appVerifier)
		},
		"a registration nobody made": func(v url.Values) { v.Set("client_id", "cli_nobody") },
		"an implicit response":       func(v url.Values) { v.Set("response_type", "token") },
		"no scope this server grants": func(v url.Values) {
			v.Set("scope", "system/Patient.read")
		},
	} {
		t.Run(name, func(t *testing.T) {
			routes, db := launchingServer(t, project.ClientPublic)
			person := signedIn(t, routes)

			ask := askingFor("user/Organization.read")
			change(ask)

			recorder := authorizing(t, routes, http.MethodPost, person, ask, "")
			if recorder.Code == http.StatusOK {
				t.Fatalf("the request was approved: %s", recorder.Body)
			}

			if held := codesHeld(t, db); held != 0 {
				t.Errorf("a refused request still minted %d codes", held)
			}
		})
	}
}

// TestAnAuthorizationNeedsAPersonBehindIt, because a scope narrows somebody's
// standing and there is nobody's to narrow otherwise.
func TestAnAuthorizationNeedsAPersonBehindIt(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)

	recorder := authorizing(t, routes, http.MethodGet, "", askingFor("user/Organization.read"), "")
	if recorder.Code == http.StatusOK {
		t.Fatalf("an unauthenticated request was answered: %s", recorder.Body)
	}
}

// approvedCodeFor runs the flow up to a freshly minted code.
func approvedCodeFor(t *testing.T, routes http.Handler, person string) string {
	t.Helper()

	approving := authorizing(
		t, routes, http.MethodPost, person, askingFor("user/Organization.read"), "")
	if approving.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approving.Code, approving.Body)
	}

	return codeFrom(t, approving)
}

// TestACodeIsRedeemedOnlyOnTheTermsItWasIssuedOn.
func TestACodeIsRedeemedOnlyOnTheTermsItWasIssuedOn(t *testing.T) {
	for name, change := range map[string]func(url.Values){
		"without the verifier": func(v url.Values) { v.Del("code_verifier") },
		"with the wrong verifier": func(v url.Values) {
			v.Set("code_verifier", strings.Repeat("a", 50))
		},
		"with the challenge as the verifier": func(v url.Values) {
			v.Set("code_verifier", appChallenge())
		},
		"as another client": func(v url.Values) { v.Set("client_id", "cli_other") },
		"at a different address": func(v url.Values) {
			v.Set("redirect_uri", "https://evil.test/callback")
		},
		"with no code at all":     func(v url.Values) { v.Del("code") },
		"under an invented grant": func(v url.Values) { v.Set("grant_type", "password") },
	} {
		t.Run(name, func(t *testing.T) {
			routes, _ := launchingServer(t, project.ClientPublic)
			person := signedIn(t, routes)

			form := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {approvedCodeFor(t, routes, person)},
				"redirect_uri":  {appRedirect},
				"client_id":     {"cli_ward"},
				"code_verifier": {appVerifier},
			}
			change(form)

			if recorder := redeeming(t, routes, form); recorder.Code == http.StatusOK {
				t.Errorf("a code was redeemed on terms it was not issued on: %s", recorder.Body)
			}
		})
	}
}

// TestACodeIsSpentByItsFirstRedemption, so an intercepted code is worthless
// after the app it was issued to has used it.
func TestACodeIsSpentByItsFirstRedemption(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {approvedCodeFor(t, routes, person)},
		"redirect_uri":  {appRedirect},
		"client_id":     {"cli_ward"},
		"code_verifier": {appVerifier},
	}

	if first := redeeming(t, routes, form); first.Code != http.StatusOK {
		t.Fatalf("first redemption: %d %s", first.Code, first.Body)
	}

	if second := redeeming(t, routes, form); second.Code == http.StatusOK {
		t.Errorf("a code was redeemed twice: %s", second.Body)
	}
}

// TestAConfidentialClientCannotRedeemWithoutItsSecret.
//
// The registered kind decides which proof is demanded, not whether one happened
// to be presented — otherwise a confidential client's stolen code would redeem
// on PKCE alone.
func TestAConfidentialClientCannotRedeemWithoutItsSecret(t *testing.T) {
	routes, db := launchingServer(t, project.ClientConfidential)
	secret := issueSecretFor(t, db)
	person := signedIn(t, routes)

	// The registration holds a live secret throughout, so every refusal below is
	// the missing or wrong proof rather than a client with nothing to prove.
	for name, present := range map[string]func(*http.Request){
		"presenting none":      func(*http.Request) {},
		"presenting a guess":   func(r *http.Request) { r.SetBasicAuth("cli_ward", "not the secret") },
		"presenting another's": func(r *http.Request) { r.SetBasicAuth("cli_other", secret) },
	} {
		t.Run(name, func(t *testing.T) {
			if redeemed := redeemingAs(t, routes, url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {approvedCodeFor(t, routes, person)},
				"redirect_uri":  {appRedirect},
				"client_id":     {"cli_ward"},
				"code_verifier": {appVerifier},
			}, present); redeemed.Code == http.StatusOK {
				t.Errorf("a confidential client redeemed %s: %s", name, redeemed.Body)
			}
		})
	}

	// And with the secret it was issued, it redeems — so the refusals above are
	// the proof being wrong rather than the flow being broken.
	redeemed := redeemingAs(t, routes, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {approvedCodeFor(t, routes, person)},
		"redirect_uri":  {appRedirect},
		"code_verifier": {appVerifier},
	}, func(r *http.Request) { r.SetBasicAuth("cli_ward", secret) })

	if redeemed.Code != http.StatusOK {
		t.Errorf("a confidential client presenting its own secret was refused: %d %s",
			redeemed.Code, redeemed.Body)
	}
}

// redeemingAs sends one token request, letting the caller state how the client
// authenticates.
func redeemingAs(
	t *testing.T, routes http.Handler, form url.Values, present func(*http.Request),
) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(
		http.MethodPost, oauthBasePath+tokenPath, strings.NewReader(form.Encode()))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/x-www-form-urlencoded")

	present(sent)

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// TestAPublicClientPresentingASecretIsRefused.
//
// A public client is defined by holding none, so whatever it sent is something
// it should not have: a secret shipped inside a browser bundle, or one client's
// credential pasted into another's configuration. Accepting it would mean this
// server could not say which clients keep secrets and which do not.
func TestAPublicClientPresentingASecretIsRefused(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	redeemed := redeemingAs(t, routes, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {approvedCodeFor(t, routes, person)},
		"redirect_uri":  {appRedirect},
		"code_verifier": {appVerifier},
	}, func(r *http.Request) { r.SetBasicAuth("cli_ward", "a secret it should not hold") })

	if redeemed.Code == http.StatusOK {
		t.Errorf("a public client redeemed while presenting a secret: %s", redeemed.Body)
	}
}

// TestAnApprovalCannotWidenWhatWasOffered, which is the one thing the second
// call could otherwise be used for.
func TestAnApprovalCannotWidenWhatWasOffered(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	recorder := authorizing(t, routes, http.MethodPost, person,
		askingFor("user/Organization.read"), `{"approved":["user/Organization.write"]}`)

	if recorder.Code == http.StatusOK {
		t.Errorf("an approval named a scope the server never offered: %s", recorder.Body)
	}
}

// TestDiscoveryAdvertisesOnlyWhatThisServerDoes.
//
// A discovery document is what a conformance suite and every client library
// believe without checking. Advertising a grant the token endpoint refuses, or a
// challenge method it will not accept, turns this file into the thing that lies
// rather than the thing that describes.
func TestDiscoveryAdvertisesOnlyWhatThisServerDoes(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)

	sent := httptest.NewRequest(http.MethodGet, smartConfigurationPath, nil)
	sent.Host = testHost

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery: %d %s", recorder.Code, recorder.Body)
	}

	var held smartConfiguration
	if err := json.Unmarshal(recorder.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the document: %v", err)
	}

	if held.Issuer != appAudience {
		t.Errorf("issuer is %q, want the base an aud must name", held.Issuer)
	}

	// Every advertised challenge method must be one this build accepts.
	for _, method := range held.ChallengeMethods {
		if _, err := project.ParseCodeChallenge(appChallenge(), method); err != nil {
			t.Errorf("discovery advertises %q, which this build refuses", method)
		}
	}

	// Every advertised grant must be one the token endpoint answers to.
	for _, grant := range held.GrantTypes {
		recorder := redeeming(t, routes, url.Values{"grant_type": {grant}})

		var failure oauthFailure
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("decode the refusal: %v", err)
		}

		if failure.Code == "unsupported_grant_type" {
			t.Errorf("discovery advertises the %q grant, which the token endpoint does not issue", grant)
		}
	}

	// And the endpoints it names are the ones actually registered, each probed
	// with the method it serves: a token endpoint answering GET would be the
	// surprise, not a token endpoint refusing one.
	for name, held := range map[string]struct {
		endpoint string
		method   string
	}{
		"authorization_endpoint": {held.AuthorizationEndpoint, http.MethodGet},
		"token_endpoint":         {held.TokenEndpoint, http.MethodPost},
	} {
		parsed, err := url.Parse(held.endpoint)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		probe := httptest.NewRequest(held.method, parsed.Path, strings.NewReader(""))
		probe.Host = testHost
		probe.Header.Set(contentTypeField, "application/x-www-form-urlencoded")

		answered := httptest.NewRecorder()
		routes.ServeHTTP(answered, probe)

		if answered.Code == http.StatusNotFound {
			t.Errorf("discovery names %s at %q, where nothing answers %s",
				name, parsed.Path, held.method)
		}
	}
}
