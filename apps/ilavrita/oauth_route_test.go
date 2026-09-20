package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
		refreshes:    sqlite.NewRefreshStore(db),
		audits:       sqlite.NewAuditStore(db),
		attempts:     newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, nil),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	})

	registerApp(t, db, kind, "cli_ward", "Ward app")

	// A second, equally real registration. Without it, a test that redeems as
	// another client would be refused for naming a registration nobody made —
	// which is a different refusal from the one it means to prove.
	registerApp(t, db, project.ClientPublic, "cli_other", "Another app")

	return allRoutes(t), db
}

// registerApp writes one registration for the tests to authorize.
func registerApp(
	t *testing.T, db *sql.DB, kind project.ClientKind,
	id project.ClientApplicationID, name string,
) {
	t.Helper()

	addresses, err := project.NewRedirectURIs(appRedirect)
	if err != nil {
		t.Fatalf("NewRedirectURIs: %v", err)
	}

	app, err := project.NewClientApplication("clinic-a", project.ClientApplicationConfig{
		ID: id, Name: name, State: project.ServiceActive,
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

	// Both the describe and the approval are this server's own consent API. A
	// browser reaches neither: it reaches the authorization endpoint, which is
	// tested separately.
	path := oauthBasePath + consentPath + "?" + ask.Encode()

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

	// SMART makes issuer conditional on sso-openid-connect and says "otherwise,
	// omitted". This build issues no identity token, so publishing one would be
	// naming an OpenID Connect issuer that answers nothing.
	if held.Issuer != "" {
		t.Errorf("issuer is %q, and this build supports no OpenID Connect", held.Issuer)
	}

	// Every advertised scope must be one this server grants: SMART says a server
	// SHALL support all of them, so this list is a promise rather than a menu.
	for _, scope := range held.ScopesSupported {
		if slices.Contains(sessionScopes, scope) {
			continue
		}

		if _, err := authz.ParseScope(scope); err != nil {
			t.Errorf("discovery advertises the %q scope, which this build refuses: %v", scope, err)
		}
	}

	// And a closed enumeration is not somewhere to add entries.
	for _, held := range [][2][]string{
		{held.GrantTypes, {"authorization_code", "client_credentials"}},
		{held.TokenEndpointAuthWays, {"client_secret_post", "client_secret_basic", "private_key_jwt"}},
	} {
		for _, stated := range held[0] {
			if !slices.Contains(held[1], stated) {
				t.Errorf("discovery states %q, which is outside what SMART enumerates: %v",
					stated, held[1])
			}
		}
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

// refreshScope is an approval that asked to outlive its session.
const refreshScope = "user/Organization.read offline_access"

// tokensFrom runs the whole flow and returns the token response.
func tokensFrom(t *testing.T, routes http.Handler, person, scope string) tokenResponse {
	t.Helper()

	approving := authorizing(t, routes, http.MethodPost, person, askingFor(scope), "")
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

	return issued
}

// refreshing sends one refresh request.
func refreshing(t *testing.T, routes http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()

	return redeeming(t, routes, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {token},
		"client_id":     {"cli_ward"},
	})
}

// TestARefreshTokenIsIssuedOnlyWhenTheApprovalAskedToOutliveItsSession.
//
// An approval naming neither offline_access nor online_access is one the person
// agreed to for this session. Issuing a refresh token anyway would extend a
// grant nobody extended — and the client learns which it got from the response
// rather than an hour later.
func TestARefreshTokenIsIssuedOnlyWhenTheApprovalAskedToOutliveItsSession(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	if held := tokensFrom(t, routes, person, "user/Organization.read"); held.RefreshToken != "" {
		t.Error("an approval that asked for no refresh was given one")
	}

	if held := tokensFrom(t, routes, person, refreshScope); held.RefreshToken == "" {
		t.Error("an approval that asked to outlive its session was given no refresh token")
	}
}

// TestARefreshMintsANewSessionCarryingTheSameApproval, without re-running
// consent and without re-reading the scopes from the client.
func TestARefreshMintsANewSessionCarryingTheSameApproval(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	first := tokensFrom(t, routes, person, refreshScope)

	refreshed := refreshing(t, routes, first.RefreshToken)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", refreshed.Code, refreshed.Body)
	}

	var second tokenResponse
	if err := json.Unmarshal(refreshed.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode the refresh: %v", err)
	}

	if second.AccessToken == "" || second.AccessToken == first.AccessToken {
		t.Error("a refresh handed back the same access token")
	}

	if second.Scope != first.Scope {
		t.Errorf("a refresh changed the approval from %q to %q", first.Scope, second.Scope)
	}

	// The new access token carries the same narrowing, so what the app may reach
	// did not widen across the refresh.
	if wrote := organizationWrite(t, routes, second.AccessToken); wrote == http.StatusCreated {
		t.Error("a refreshed session could write, which the approval never granted")
	}
}

// TestARefreshTokenIsRotatedOnEveryUse, so a token read out of a log is already
// dead by the time anybody tries it.
func TestARefreshTokenIsRotatedOnEveryUse(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	first := tokensFrom(t, routes, person, refreshScope)

	refreshed := refreshing(t, routes, first.RefreshToken)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", refreshed.Code, refreshed.Body)
	}

	var second tokenResponse
	if err := json.Unmarshal(refreshed.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode the refresh: %v", err)
	}

	if second.RefreshToken == "" {
		t.Fatal("a refresh handed back no successor, so the grant is unusable")
	}

	if second.RefreshToken == first.RefreshToken {
		t.Error("a refresh handed back the same token, so nothing rotated")
	}
}

// TestReplayingASpentRefreshTokenKillsTheWholeGrant.
//
// A spent token presented again is a copy somebody else is holding. Stopping the
// next refresh is only half a response: whatever the replayer already obtained
// would otherwise live out its hour, so the sessions that grant minted die too.
func TestReplayingASpentRefreshTokenKillsTheWholeGrant(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	first := tokensFrom(t, routes, person, refreshScope)

	refreshed := refreshing(t, routes, first.RefreshToken)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", refreshed.Code, refreshed.Body)
	}

	var second tokenResponse
	if err := json.Unmarshal(refreshed.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode the refresh: %v", err)
	}

	// The access token the legitimate refresh produced works right up to the
	// replay, so what follows is the replay's doing and not a broken session.
	if reached := organizationRead(t, routes, second.AccessToken); reached == http.StatusUnauthorized {
		t.Fatalf("the refreshed session was already dead: %d", reached)
	}

	// Somebody presents the spent token.
	if replayed := refreshing(t, routes, first.RefreshToken); replayed.Code == http.StatusOK {
		t.Fatalf("a spent refresh token was honoured: %s", replayed.Body)
	}

	// The successor is dead too, so the replayer gains nothing by holding it.
	if after := refreshing(t, routes, second.RefreshToken); after.Code == http.StatusOK {
		t.Errorf("the grant survived a replay: %s", after.Body)
	}

	// And so is the access token that grant already minted.
	if reached := organizationRead(t, routes, second.AccessToken); reached != http.StatusUnauthorized {
		t.Errorf("a session the replayed grant minted still answers: %d", reached)
	}

	// Nothing of the chain is left to present.
	var left int

	if err := db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM refresh_tokens").Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}

	if left != 0 {
		t.Errorf("%d rotations survived the revocation", left)
	}
}

// organizationRead attempts a read and reports the status.
func organizationRead(t *testing.T, routes http.Handler, token string) int {
	t.Helper()

	sent := httptest.NewRequest(http.MethodGet, fhir.BasePath+"/Organization/org-1", nil)
	sent.Host = testHost
	sent.Header.Set(authorizationField, bearerPrefix+token)

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder.Code
}

// TestARefreshTokenIsRedeemedOnlyByTheClientItWasIssuedTo.
func TestARefreshTokenIsRedeemedOnlyByTheClientItWasIssuedTo(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	first := tokensFrom(t, routes, person, refreshScope)

	for name, form := range map[string]url.Values{
		"as another client": {
			"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken},
			"client_id": {"cli_other"},
		},
		"with no token at all": {
			"grant_type": {"refresh_token"}, "client_id": {"cli_ward"},
		},
		"with a token nobody issued": {
			"grant_type": {"refresh_token"}, "refresh_token": {"invented"},
			"client_id": {"cli_ward"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if recorder := redeeming(t, routes, form); recorder.Code == http.StatusOK {
				t.Errorf("a refresh token was redeemed wrongly: %s", recorder.Body)
			}
		})
	}
}

// TestEveryRouteThisSurfaceServesIsDescribed.
//
// scripts/verify-openapi.sh walks the description and probes the server, so it
// catches a documented route that drifted. It cannot catch the opposite — a
// route the server serves and the description never mentions — because it has no
// list of what is served to compare against.
//
// That gap is how the whole SMART surface shipped undocumented while the
// verifier passed. This closes it for these routes by naming the same constants
// the router registers: a path that changes fails here, and the description has
// to change with it.
//
// It does not close it in general. A route added to a file this test does not
// name is still invisible, and the honest fix for that is a router that can
// enumerate itself.
func TestEveryRouteThisSurfaceServesIsDescribed(t *testing.T) {
	described, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the description: %v", err)
	}

	for name, path := range map[string]string{
		"the authorization endpoint": oauthBasePath + authorizePath,
		"the consent description":    oauthBasePath + consentPath,
		"the token endpoint":         oauthBasePath + tokenPath,
		"the identity key set":       oauthBasePath + identityKeysPath,
		"OpenID Connect discovery":   openIDConfigurationPath,
		"the discovery document":     smartConfigurationPath,
		"login":                      authBasePath + loginPath,
		"logout":                     authBasePath + logoutPath,
		"the session description":    authBasePath + sessionPath,
		"the install claim":          authBasePath + claimPath,
	} {
		t.Run(name, func(t *testing.T) {
			// Anchored to the start of a line and followed by a colon, so a path
			// merely mentioned in prose does not count as described.
			if !strings.Contains(string(described), "\n  "+path+":\n") {
				t.Errorf("%s is served at %q and the description does not declare it", name, path)
			}
		})
	}
}

// consentedAt points the authorization endpoint at a page for the duration of
// one test.
func consentedAt(t *testing.T, page string) {
	t.Helper()

	held := consentPage
	consentPage = page

	t.Cleanup(func() { consentPage = held })
}

// beginning sends one browser request to the authorization endpoint.
func beginning(t *testing.T, routes http.Handler, ask url.Values) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(
		http.MethodGet, oauthBasePath+authorizePath+"?"+ask.Encode(), nil)
	sent.Host = testHost

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// TestTheAuthorizationEndpointSendsABrowserSomewhereItCanApprove.
//
// SMART's authorization endpoint is a browser endpoint: an app sends a person
// there and expects them back at the redirect address with a code. A JSON API
// at that path is not that endpoint, however correct its contents — no app and
// no conformance suite can drive one.
func TestTheAuthorizationEndpointSendsABrowserSomewhereItCanApprove(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	consentedAt(t, "https://console.example.test/approve")

	ask := askingFor("user/Organization.read")

	recorder := beginning(t, routes, ask)
	if recorder.Code != http.StatusFound {
		t.Fatalf("authorize answered %d, want a redirect: %s", recorder.Code, recorder.Body)
	}

	sent, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}

	if sent.Host != "console.example.test" || sent.Path != "/approve" {
		t.Errorf("a person was sent to %q, not the configured consent page", sent)
	}

	// Every parameter travels, so the page can describe what is being approved
	// without inventing any of it.
	for _, name := range []string{
		"response_type", "client_id", "redirect_uri", "scope",
		"state", "aud", "code_challenge", "code_challenge_method",
	} {
		if sent.Query().Get(name) != ask.Get(name) {
			t.Errorf("%s reached the consent page as %q, want %q",
				name, sent.Query().Get(name), ask.Get(name))
		}
	}
}

// TestTheAuthorizationEndpointNeedsNobodySignedIn, because the person being sent
// to sign in is the point of it.
func TestTheAuthorizationEndpointNeedsNobodySignedIn(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	consentedAt(t, "https://console.example.test/approve")

	if recorder := beginning(t, routes, askingFor("user/Organization.read")); recorder.Code != http.StatusFound {
		t.Fatalf("an unauthenticated browser got %d, want a redirect: %s", recorder.Code, recorder.Body)
	}
}

// TestAMalformedAuthorizationIsAnsweredRatherThanRedirected.
//
// Nothing has been verified at this point — no client resolved, no address
// checked — so there is nowhere a refusal could safely be sent. It is answered
// directly, which is also what stops the endpoint becoming a way to bounce a
// browser anywhere a query parameter names.
func TestAMalformedAuthorizationIsAnsweredRatherThanRedirected(t *testing.T) {
	for name, change := range map[string]func(url.Values){
		"no client":            func(v url.Values) { v.Del("client_id") },
		"no address":           func(v url.Values) { v.Del("redirect_uri") },
		"no audience":          func(v url.Values) { v.Del("aud") },
		"no challenge":         func(v url.Values) { v.Del("code_challenge") },
		"no scope":             func(v url.Values) { v.Del("scope") },
		"an implicit response": func(v url.Values) { v.Set("response_type", "token") },
	} {
		t.Run(name, func(t *testing.T) {
			routes, _ := launchingServer(t, project.ClientPublic)
			consentedAt(t, "https://console.example.test/approve")

			ask := askingFor("user/Organization.read")
			change(ask)

			recorder := beginning(t, routes, ask)
			if recorder.Code == http.StatusFound {
				t.Errorf("a malformed request was redirected to %q",
					recorder.Header().Get("Location"))
			}
		})
	}
}

// TestADeploymentWithNoConsentPageAuthorizesNobody, rather than redirecting to
// an empty address and leaving a browser somewhere nobody chose.
func TestADeploymentWithNoConsentPageAuthorizesNobody(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	consentedAt(t, "")

	recorder := beginning(t, routes, askingFor("user/Organization.read"))
	if recorder.Code == http.StatusFound {
		t.Errorf("a deployment with no consent page redirected to %q",
			recorder.Header().Get("Location"))
	}
}

// TestAConsentPageThisServerCannotRedirectToIsRefusedAtStartup, so a deployment
// learns when it starts rather than when somebody tries to launch an app.
func TestAConsentPageThisServerCannotRedirectToIsRefusedAtStartup(t *testing.T) {
	for name, stated := range map[string]string{
		"relative":            "/approve",
		"carrying a fragment": "https://console.example.test/approve#here",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(consentURLVariable, stated)

			if _, err := configuredConsentPage(); !errors.Is(err, errNoConsentPage) {
				t.Fatalf("error: got %v, want errNoConsentPage", err)
			}
		})
	}

	t.Setenv(consentURLVariable, "https://console.example.test/approve")

	held, err := configuredConsentPage()
	if err != nil {
		t.Fatalf("a usable consent page was refused: %v", err)
	}

	if held != "https://console.example.test/approve" {
		t.Errorf("read %q", held)
	}
}

// TestBothPublicDiscoveryEndpointsAreReadableCrossOrigin.
//
// SMART requires it of a server supporting browser apps, and this one declares
// client-public. A browser app reads both before it holds anything to protect,
// so refusing the origin would make it unable to start.
//
// The rest of the FHIR surface deliberately carries no cross-origin headers at
// all, which is what the second half asserts: this is an exception for two
// public documents, not a relaxation.
func TestBothPublicDiscoveryEndpointsAreReadableCrossOrigin(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)

	for name, path := range map[string]string{
		"the discovery document":  smartConfigurationPath,
		"the CapabilityStatement": fhir.BasePath + metadataPath,
	} {
		t.Run(name, func(t *testing.T) {
			sent := httptest.NewRequest(http.MethodGet, path, nil)
			sent.Host = testHost
			sent.Header.Set("Origin", "https://app.example.test")

			recorder := httptest.NewRecorder()
			routes.ServeHTTP(recorder, sent)

			if recorder.Code != http.StatusOK {
				t.Fatalf("%s answered %d", name, recorder.Code)
			}

			if recorder.Header().Get("Access-Control-Allow-Origin") == "" {
				t.Errorf("%s carries no Access-Control-Allow-Origin, so no browser app can read it", name)
			}
		})
	}

	// What separates these two from a resource route is not the origin policy —
	// the whole FHIR surface carries one now — but that neither needs a
	// credential to read. That the resource routes still do is asserted where
	// authentication is, not here.
}

// serviceKey is one backend service's key pair and the set its registration
// holds.
type serviceKey struct {
	private *rsa.PrivateKey
	set     string
}

// aServiceKey generates one.
func aServiceKey(t *testing.T) serviceKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	set, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "svc", "alg": "RS384",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	return serviceKey{private: key, set: string(set)}
}

// signedAssertion builds one client assertion, letting a caller change any claim
// so each refusal is about exactly one thing.
func signedAssertion(t *testing.T, key serviceKey, change func(map[string]any)) string {
	t.Helper()

	claims := map[string]any{
		"iss": "cli_service",
		"sub": "cli_service",
		"aud": "http://" + testHost + oauthBasePath + tokenPath,
		"exp": time.Now().Add(2 * time.Minute).Unix(),
		"jti": "jti-" + time.Now().Format(time.RFC3339Nano),
	}

	if change != nil {
		change(claims)
	}

	encode := func(held map[string]any) string {
		raw, err := json.Marshal(held)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(raw)
	}

	signed := encode(map[string]any{"alg": "RS384", "typ": "JWT", "kid": "svc"}) + "." + encode(claims)
	digest := sha512.Sum384([]byte(signed))

	signature, err := rsa.SignPKCS1v15(rand.Reader, key.private, crypto.SHA384, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// registerService writes a backend service holding a key, standing and a policy
// that reaches Organization.
func registerService(t *testing.T, db *sql.DB, key serviceKey) {
	t.Helper()

	keys, err := project.ParseJWKS(key.set)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	app, err := project.NewClientApplication("clinic-a", project.ClientApplicationConfig{
		ID: "cli_service", Name: "Nightly service", State: project.ServiceActive,
		Kind: project.ClientConfidential, JWKS: keys,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	if _, err := sqlite.NewClientApplicationStore(db).Create(context.Background(), app); err != nil {
		t.Fatalf("register the service: %v", err)
	}

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_service", Project: "clinic-a", ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{
			Kind: project.PrincipalClientApplication, ID: "cli_service",
		},
		State: project.MembershipActive, Source: project.SourceAPI,
		Policies: []project.PolicyAttachment{{Policy: "pol_ward"}},
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	if err := sqlite.NewMembershipStore(db).Create(context.Background(), member); err != nil {
		t.Fatalf("give the service standing: %v", err)
	}
}

// asService sends one client-credentials request.
func asService(t *testing.T, routes http.Handler, assertion, scope string) *httptest.ResponseRecorder {
	t.Helper()

	return redeeming(t, routes, url.Values{
		"grant_type":            {"client_credentials"},
		"scope":                 {scope},
		"client_assertion_type": {assertionType},
		"client_assertion":      {assertion},
	})
}

// TestABackendServiceAuthenticatesByTheKeyItSignedWith.
//
// This is the whole of SMART Backend Services: no person, no consent, no
// secret — a signature over a short-lived assertion, against a key the
// registration published. What it then reaches is the service's own standing,
// narrowed to the system scopes it asked for.
func TestABackendServiceAuthenticatesByTheKeyItSignedWith(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	key := aServiceKey(t)
	registerService(t, db, key)

	answer := asService(t, routes, signedAssertion(t, key, nil), "system/Organization.read")
	if answer.Code != http.StatusOK {
		t.Fatalf("client credentials: %d %s", answer.Code, answer.Body)
	}

	var issued tokenResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode the token: %v", err)
	}

	if issued.TokenType != "Bearer" || issued.AccessToken == "" {
		t.Fatalf("the token response carried %+v", issued)
	}

	if issued.Scope != "system/Organization.read" {
		t.Errorf("granted %q, want the scope it asked for", issued.Scope)
	}

	// A service holds no consent, so nothing about it is a person's session.
	if issued.RefreshToken != "" || issued.Patient != "" {
		t.Errorf("a service was given a refresh token or a patient: %+v", issued)
	}

	// It reaches what it was granted, and not the write its standing also holds.
	if wrote := organizationWrite(t, routes, issued.AccessToken); wrote == http.StatusCreated {
		t.Error("a service granted only a read wrote anyway")
	}
}

// TestAnAssertionThisServerCannotBelieveAuthenticatesNobody.
func TestAnAssertionThisServerCannotBelieveAuthenticatesNobody(t *testing.T) {
	for name, held := range map[string]struct {
		change func(map[string]any)
		signer bool
	}{
		"signed by another key":     {nil, true},
		"claiming another client":   {func(c map[string]any) { c["iss"] = "cli_other"; c["sub"] = "cli_other" }, false},
		"aimed at another server":   {func(c map[string]any) { c["aud"] = "https://elsewhere.test/token" }, false},
		"already expired":           {func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, false},
		"living longer than it may": {func(c map[string]any) { c["exp"] = time.Now().Add(time.Hour).Unix() }, false},
		"carrying no jti":           {func(c map[string]any) { delete(c, "jti") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			routes, db := launchingServer(t, project.ClientPublic)
			key := aServiceKey(t)
			registerService(t, db, key)

			signing := key
			if held.signer {
				signing = aServiceKey(t)
			}

			answer := asService(t, routes, signedAssertion(t, signing, held.change), "system/Organization.read")
			if answer.Code == http.StatusOK {
				t.Errorf("an assertion this server cannot believe authenticated: %s", answer.Body)
			}
		})
	}
}

// TestOneAssertionAuthenticatesOnce, which is what the jti is for: a replayed
// assertion is one somebody other than the service is holding.
func TestOneAssertionAuthenticatesOnce(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	key := aServiceKey(t)
	registerService(t, db, key)

	assertion := signedAssertion(t, key, nil)

	if first := asService(t, routes, assertion, "system/Organization.read"); first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}

	if second := asService(t, routes, assertion, "system/Organization.read"); second.Code == http.StatusOK {
		t.Errorf("one assertion authenticated twice: %s", second.Body)
	}
}

// TestABackendServiceAsksForSystemScopesOrNothing.
//
// A service acts for nobody, so there is no standing a patient or user scope
// could narrow. Honouring one would mean deciding on a person's behalf which
// person that was.
func TestABackendServiceAsksForSystemScopesOrNothing(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	key := aServiceKey(t)
	registerService(t, db, key)

	for name, scope := range map[string]string{
		"a user scope":    "user/Organization.read",
		"a patient scope": "patient/Observation.read",
		"nothing at all":  "",
		"not a scope":     "nonsense",
	} {
		t.Run(name, func(t *testing.T) {
			if answer := asService(
				t, routes, signedAssertion(t, key, nil), scope,
			); answer.Code == http.StatusOK {
				t.Errorf("a service was granted %q: %s", scope, answer.Body)
			}
		})
	}
}

// TestAServiceWithNoStandingIsRefusedRatherThanGivenAUselessToken, so a
// registration nobody gave a policy looks like a registration problem now
// instead of a policy problem at its first request.
func TestAServiceWithNoStandingIsRefusedRatherThanGivenAUselessToken(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	key := aServiceKey(t)

	keys, err := project.ParseJWKS(key.set)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	app, err := project.NewClientApplication("clinic-a", project.ClientApplicationConfig{
		ID: "cli_service", Name: "Nightly service", State: project.ServiceActive,
		Kind: project.ClientConfidential, JWKS: keys,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	if _, err := sqlite.NewClientApplicationStore(db).Create(context.Background(), app); err != nil {
		t.Fatalf("register the service: %v", err)
	}

	answer := asService(t, routes, signedAssertion(t, key, nil), "system/Organization.read")
	if answer.Code == http.StatusOK {
		t.Errorf("a service holding no standing was given a token: %s", answer.Body)
	}
}

// TestTheAuthorizationEndpointReadsAFormAsWellAsAQuery.
//
// SMART App Launch: "Authorization Servers SHALL support the use of the HTTP GET
// and POST methods at the Authorization Endpoint." An app chooses which, and a
// server that reads only one is a server half the apps cannot launch against.
func TestTheAuthorizationEndpointReadsAFormAsWellAsAQuery(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	consentedAt(t, "https://console.example.test/approve")

	ask := askingFor("user/Organization.read")

	sent := httptest.NewRequest(
		http.MethodPost, oauthBasePath+authorizePath, strings.NewReader(ask.Encode()))
	sent.Host = testHost
	sent.Header.Set(contentTypeField, "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	// See Other, because the page being sent to is a GET and 302 only permits
	// that change of method rather than requiring it.
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("a form authorization answered %d, want %d: %s",
			recorder.Code, http.StatusSeeOther, recorder.Body)
	}

	where, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}

	if where.Host != "console.example.test" || where.Path != "/approve" {
		t.Fatalf("a form authorization sent the person to %q", where)
	}

	// The parameters came out of the body, so a server reading only the query
	// would have redirected with none of them.
	for _, name := range []string{
		"response_type", "client_id", "redirect_uri", "scope",
		"aud", "code_challenge", "code_challenge_method",
	} {
		if where.Query().Get(name) != ask.Get(name) {
			t.Errorf("%s reached the consent page as %q, want %q",
				name, where.Query().Get(name), ask.Get(name))
		}
	}
}

// TestNoResponseCarryingATokenMayBeCached.
//
// RFC 6749 section 5.1: a response holding a token must carry Cache-Control:
// no-store. A proxy or a browser that kept one would be holding somebody's
// credential ready for whoever asked next.
func TestNoResponseCarryingATokenMayBeCached(t *testing.T) {
	routes, db := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	approved := authorizing(t, routes, http.MethodPost, person, askingFor(refreshScope), "")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}

	redeemed := redeeming(t, routes, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, approved)},
		"redirect_uri":  {appRedirect},
		"client_id":     {"cli_ward"},
		"code_verifier": {appVerifier},
	})

	var issued tokenResponse
	if err := json.Unmarshal(redeemed.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode the token: %v", err)
	}

	key := aServiceKey(t)
	registerService(t, db, key)

	// Every grant, because each one answers from its own place and a header
	// added to two of three still leaves a token in somebody's cache.
	for name, recorder := range map[string]*httptest.ResponseRecorder{
		"an authorization code": redeemed,
		"a refresh":             refreshing(t, routes, issued.RefreshToken),
		"client credentials": asService(
			t, routes, signedAssertion(t, key, nil), "system/Organization.read"),
	} {
		t.Run(name, func(t *testing.T) {
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body)
			}

			if held := recorder.Header().Get("Cache-Control"); !strings.Contains(held, "no-store") {
				t.Errorf("%s answered Cache-Control %q, want no-store", name, held)
			}

			if held := recorder.Header().Get("Pragma"); held != "no-cache" {
				t.Errorf("%s answered Pragma %q, want no-cache", name, held)
			}
		})
	}
}

// TestAScopeThatNarrowsNothingDoesNotBreakEverythingElse.
//
// An approval carries scopes of two kinds: ones naming a resource, and ones
// describing the token — offline_access, launch/patient, openid. Only the first
// kind narrows anything. The second has to be carried and ignored, because a
// server that tried to read it as a resource scope would fail every request the
// token was granted for.
//
// The negative assertions elsewhere cannot catch this: "the write was refused"
// is satisfied by a server fault just as well as by a refusal.
func TestAScopeThatNarrowsNothingDoesNotBreakEverythingElse(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)
	person := signedIn(t, routes)

	// A write, because it is the one interaction the fixture policy grants
	// outright: a 201 says the request was authorized, where a read answering
	// 404 would leave open whether it was authorized and empty or refused.
	granted := "user/Organization.read user/Organization.write"

	for name, scope := range map[string]string{
		"a refresh was asked for":            granted + " offline_access",
		"the session was asked to stay live": granted + " online_access",
		"a patient context was asked for":    granted + " launch/patient",
	} {
		t.Run(name, func(t *testing.T) {
			issued := tokensFrom(t, routes, person, scope)

			if wrote := organizationWrite(t, routes, issued.AccessToken); wrote != http.StatusCreated {
				t.Fatalf("a write the approval granted answered %d", wrote)
			}
		})
	}
}
