package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// The resource the seeded identity is, for the fhirUser claim.
const (
	nurseProfileType = "Practitioner"
	nurseProfileID   = "prac_nurse"
)

// identityScope asks for everything an identity token carries, plus something
// to reach with the access token so the approval is not identity alone.
const identityScope = "openid fhirUser user/Organization.read"

// signingServer is a launching server that can actually sign an identity token,
// for an identity whose membership names the resource they are.
//
// Without both, every identity scope is refused with a reason — which is the
// correct behaviour and is what the deployment tests below assert, but it also
// means a test that forgot this would pass while proving nothing.
func signingServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()

	routes, db := launchingServer(t, project.ClientPublic)

	encoded, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("mint a sealing key: %v", err)
	}

	key, err := project.ParseSealingKey(encoded)
	if err != nil {
		t.Fatalf("parse a sealing key: %v", err)
	}

	serving.signingKeys = sqlite.NewSigningKeyStore(db, key)

	// The resource has to exist before a membership can name it: the schema
	// foreign-keys the profile into this Project's own resources, so a claim
	// pointing at nothing is unrepresentable rather than merely avoided.
	if _, err := db.ExecContext(t.Context(),
		"INSERT INTO fhir_resource"+
			" (project_id, res_type, res_id, version_id, version_seq, last_updated, deleted, content)"+
			" VALUES ('clinic-a', ?, ?, '1', 1, 0, 0, ?)",
		nurseProfileType, nurseProfileID,
		`{"resourceType":"`+nurseProfileType+`","id":"`+nurseProfileID+`"}`); err != nil {
		t.Fatalf("write the resource this person is: %v", err)
	}

	if _, err := db.ExecContext(t.Context(),
		"UPDATE project_memberships SET profile_type = ?, profile_id = ? WHERE id = 'pm_nurse'",
		nurseProfileType, nurseProfileID); err != nil {
		t.Fatalf("name the resource this person is: %v", err)
	}

	return routes, db
}

// fetching reads one published document.
func fetching(t *testing.T, routes http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	sent := httptest.NewRequest(http.MethodGet, path, nil)
	sent.Host = testHost

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

// decoded reads one part of a compact JWS.
func decoded(t *testing.T, part string) map[string]any {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode a token part: %v", err)
	}

	var held map[string]any
	if err := json.Unmarshal(raw, &held); err != nil {
		t.Fatalf("a token part is not JSON: %v", err)
	}

	return held
}

// identityFrom drives a whole launch and returns what the token endpoint gave.
func identityFrom(t *testing.T, routes http.Handler, scope string) tokenResponse {
	t.Helper()

	person := signedIn(t, routes)

	approved := authorizing(t, routes, http.MethodPost, person, askingFor(scope), "")
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
	if redeemed.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", redeemed.Code, redeemed.Body)
	}

	var issued tokenResponse
	if err := json.Unmarshal(redeemed.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode the token response: %v", err)
	}

	return issued
}

// TestAnIdentityTokenVerifiesAgainstThePublishedKeySet.
//
// This is what a client does with an identity token, and nothing else matters
// if it fails: read the issuer out of the token, fetch the configuration there,
// take jwks_uri from it, find the key the `kid` names, and check the signature.
// A token that does not survive that round trip is one every conformant client
// refuses.
func TestAnIdentityTokenVerifiesAgainstThePublishedKeySet(t *testing.T) {
	routes, _ := signingServer(t)

	issued := identityFrom(t, routes, identityScope)
	if issued.IDToken == "" {
		t.Fatal("an approval that granted openid produced no identity token")
	}

	parts := strings.Split(issued.IDToken, ".")
	if len(parts) != 3 {
		t.Fatalf("a compact JWS has three parts, this has %d", len(parts))
	}

	header, claims := decoded(t, parts[0]), decoded(t, parts[1])

	// A client follows the issuer rather than a path it guessed at.
	issuer, _ := claims["iss"].(string)
	if issuer == "" {
		t.Fatal("the token names no issuer, so a client cannot find the configuration")
	}

	document := fetching(t, routes, localPath(issuer)+openIDConfigurationPath)
	if document.Code != http.StatusOK {
		t.Fatalf("the configuration at the issuer answered %d: %s", document.Code, document.Body)
	}

	var configuration openIDConfiguration
	if err := json.Unmarshal(document.Body.Bytes(), &configuration); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}

	if configuration.Issuer != issuer {
		t.Errorf("the configuration issues as %q, the token as %q", configuration.Issuer, issuer)
	}

	keys := fetching(t, routes, localPath(configuration.JWKSURI))
	if keys.Code != http.StatusOK {
		t.Fatalf("jwks_uri answered %d: %s", keys.Code, keys.Body)
	}

	var published struct {
		Keys []map[string]string `json:"keys"`
	}

	if err := json.Unmarshal(keys.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode the key set: %v", err)
	}

	named, _ := header["kid"].(string)

	index := slices.IndexFunc(published.Keys, func(one map[string]string) bool {
		return one["kid"] == named
	})
	if index < 0 {
		t.Fatalf("the token names kid %q and the published set holds none", named)
	}

	verifyWith(t, published.Keys[index], parts)
}

// localPath strips the origin off a published address, so a test can ask this
// server for the document it just told a client to fetch.
func localPath(published string) string {
	parsed, err := url.Parse(published)
	if err != nil {
		return published
	}

	return parsed.Path
}

// verifyWith checks a token's signature against one published JWK.
func verifyWith(t *testing.T, entry map[string]string, parts []string) {
	t.Helper()

	modulus, err := base64.RawURLEncoding.DecodeString(entry["n"])
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}

	exponent, err := base64.RawURLEncoding.DecodeString(entry["e"])
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode the signature: %v", err)
	}

	public := &rsa.PublicKey{
		N: new(big.Int).SetBytes(modulus),
		E: int(new(big.Int).SetBytes(exponent).Int64()),
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the published key does not verify the identity token: %v", err)
	}
}

// TestAnIdentityTokenCarriesEveryClaimAClientChecks.
//
// A client verifies `iss`, `sub`, `aud`, `exp` and `iat` and refuses the token
// if any is missing or wrong. `aud` is the one worth being careful about: a
// token minted for one client and accepted by another is one any app that
// happened to receive it could use.
func TestAnIdentityTokenCarriesEveryClaimAClientChecks(t *testing.T) {
	routes, _ := signingServer(t)

	issued := identityFrom(t, routes, identityScope)
	claims := decoded(t, strings.Split(issued.IDToken, ".")[1])

	for _, name := range []string{"iss", "sub", "aud", "exp", "iat"} {
		if _, held := claims[name]; !held {
			t.Errorf("the identity token carries no %s", name)
		}
	}

	if audience, _ := claims["aud"].(string); audience != "cli_ward" {
		t.Errorf("the token is for %q, want the client that asked", audience)
	}

	subject, _ := claims["sub"].(string)
	if subject == "" || len(subject) > 255 {
		t.Errorf("the subject is %d characters, want between 1 and 255", len(subject))
	}

	expires, _ := claims["exp"].(float64)
	if time.Unix(int64(expires), 0).Before(time.Now()) {
		t.Error("the identity token is already expired")
	}
}

// TestTheFHIRUserClaimNamesTheResourceThisPersonIs.
//
// SMART requires the claim to be the URL of a Patient, Practitioner,
// RelatedPerson or Person. A client splits it to find the type and the id, so a
// relative reference or a type outside that set is one nothing can resolve.
func TestTheFHIRUserClaimNamesTheResourceThisPersonIs(t *testing.T) {
	routes, _ := signingServer(t)

	issued := identityFrom(t, routes, identityScope)
	claims := decoded(t, strings.Split(issued.IDToken, ".")[1])

	user, _ := claims["fhirUser"].(string)
	if user == "" {
		t.Fatal("an approval that granted fhirUser produced no claim")
	}

	// Absolute, because a client fetches it directly rather than resolving it
	// against a base it would have to have been told separately.
	parsed, err := url.Parse(user)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		t.Errorf("fhirUser is %q, which is not a URL a client can fetch", user)
	}

	segments := strings.Split(user, "/")
	if len(segments) < 2 {
		t.Fatalf("fhirUser is %q, which names no resource", user)
	}

	kind, id := segments[len(segments)-2], segments[len(segments)-1]

	if !slices.Contains([]string{"Patient", "Practitioner", "RelatedPerson", "Person"}, kind) {
		t.Errorf("fhirUser names a %s, which is not a resource type SMART allows here", kind)
	}

	if kind != nurseProfileType || id != nurseProfileID {
		t.Errorf("fhirUser names %s/%s, want %s/%s", kind, id, nurseProfileType, nurseProfileID)
	}
}

// TestAnApprovalThatDidNotAskForAnIdentityGetsNone.
//
// The identity token says who the person is. Handing one to an app that never
// asked would be volunteering that, and the app reading a response it did not
// ask about is not the place to decide it should not have been told.
func TestAnApprovalThatDidNotAskForAnIdentityGetsNone(t *testing.T) {
	routes, _ := signingServer(t)

	if issued := identityFrom(t, routes, "user/Organization.read"); issued.IDToken != "" {
		t.Error("an approval that asked for no identity was given one")
	}
}

// TestFHIRUserIsWithheldFromAnApprovalThatOnlyAskedForOpenID.
//
// openid says "tell me who signed in"; fhirUser says "tell me which record they
// are". A server answering the second when only the first was asked is one
// telling an app more about a person than it was granted.
func TestFHIRUserIsWithheldFromAnApprovalThatOnlyAskedForOpenID(t *testing.T) {
	routes, _ := signingServer(t)

	issued := identityFrom(t, routes, "openid user/Organization.read")
	if issued.IDToken == "" {
		t.Fatal("an approval that granted openid produced no identity token")
	}

	claims := decoded(t, strings.Split(issued.IDToken, ".")[1])
	if user, held := claims["fhirUser"]; held {
		t.Errorf("a token that was not granted fhirUser carries %v", user)
	}
}

// TestARefreshReissuesTheIdentityToken.
//
// An identity token outlives its usefulness in minutes and an access token in
// an hour. A refresh carrying the old one forward would hand the client a token
// that expired long before, and one that dropped it would tell an app still
// holding the openid grant that it no longer has it.
func TestARefreshReissuesTheIdentityToken(t *testing.T) {
	routes, _ := signingServer(t)

	first := identityFrom(t, routes, identityScope+" offline_access")
	if first.RefreshToken == "" {
		t.Fatal("no refresh token to refresh with")
	}

	refreshed := refreshing(t, routes, first.RefreshToken)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", refreshed.Code, refreshed.Body)
	}

	var second tokenResponse
	if err := json.Unmarshal(refreshed.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode the refresh: %v", err)
	}

	if second.IDToken == "" {
		t.Fatal("a refresh of a grant carrying openid produced no identity token")
	}

	claims := decoded(t, strings.Split(second.IDToken, ".")[1])
	if user, _ := claims["fhirUser"].(string); user == "" {
		t.Error("the reissued token dropped the fhirUser claim the approval still grants")
	}
}

// TestTheOpenIDConfigurationCarriesEveryRequiredField.
//
// OpenID Connect Discovery names them, and a client reads the document before
// it has anything else to go on. A missing field is one it cannot work around.
func TestTheOpenIDConfigurationCarriesEveryRequiredField(t *testing.T) {
	routes, _ := signingServer(t)

	answer := fetching(t, routes, openIDConfigurationPath)
	if answer.Code != http.StatusOK {
		t.Fatalf("the configuration answered %d: %s", answer.Code, answer.Body)
	}

	if held := answer.Header().Get(contentTypeField); !strings.Contains(held, "application/json") {
		t.Errorf("the configuration is served as %q, want JSON", held)
	}

	var held map[string]any
	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}

	for _, field := range []string{
		"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri",
		"response_types_supported", "subject_types_supported",
		"id_token_signing_alg_values_supported",
	} {
		if _, found := held[field]; !found {
			t.Errorf("the configuration carries no %s", field)
		}
	}

	var configuration openIDConfiguration
	if err := json.Unmarshal(answer.Body.Bytes(), &configuration); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}

	// SMART requires RSA SHA-256 by name, so a document not offering it would be
	// one no SMART client could use whatever else it said.
	if !slices.Contains(configuration.SigningAlgorithms, "RS256") {
		t.Errorf("the configuration offers %v, and SMART requires RS256",
			configuration.SigningAlgorithms)
	}
}

// TestTheTwoDiscoveryDocumentsAgreeAboutTheIssuer.
//
// A client may arrive at either. SMART makes `issuer` conditional on the
// sso-openid-connect capability, so the capability and the field have to be
// published together or a client acts on one and finds the other missing.
func TestTheTwoDiscoveryDocumentsAgreeAboutTheIssuer(t *testing.T) {
	routes, _ := signingServer(t)

	smart := fetching(t, routes, smartConfigurationPath)
	if smart.Code != http.StatusOK {
		t.Fatalf("the SMART configuration answered %d: %s", smart.Code, smart.Body)
	}

	var advertised smartConfiguration
	if err := json.Unmarshal(smart.Body.Bytes(), &advertised); err != nil {
		t.Fatalf("decode the SMART configuration: %v", err)
	}

	if !slices.Contains(advertised.Capabilities, "sso-openid-connect") {
		t.Error("a server issuing identity tokens does not claim sso-openid-connect")
	}

	if advertised.Issuer == "" {
		t.Fatal("the SMART configuration claims the capability and names no issuer")
	}

	for _, scope := range []string{scopeOpenID, scopeFHIRUser} {
		if !slices.Contains(advertised.ScopesSupported, scope) {
			t.Errorf("the SMART configuration does not offer %s", scope)
		}
	}

	openID := fetching(t, routes, openIDConfigurationPath)
	if openID.Code != http.StatusOK {
		t.Fatalf("the OpenID configuration answered %d", openID.Code)
	}

	var configuration openIDConfiguration
	if err := json.Unmarshal(openID.Body.Bytes(), &configuration); err != nil {
		t.Fatalf("decode the OpenID configuration: %v", err)
	}

	if advertised.Issuer != configuration.Issuer {
		t.Errorf("SMART issues as %q and OpenID Connect as %q",
			advertised.Issuer, configuration.Issuer)
	}

	if advertised.JWKSURI != configuration.JWKSURI {
		t.Errorf("SMART publishes keys at %q and OpenID Connect at %q",
			advertised.JWKSURI, configuration.JWKSURI)
	}
}

// TestADeploymentThatCannotSignSaysSoRatherThanPretending.
//
// Without a sealing key there is nothing to keep a signing key under, so no
// identity token can be issued. Every visible surface has to agree: the scopes
// are refused with the reason, the documents are absent, and the SMART document
// claims neither the capability nor an issuer.
func TestADeploymentThatCannotSignSaysSoRatherThanPretending(t *testing.T) {
	routes, _ := launchingServer(t, project.ClientPublic)

	view := describedScopes(t, routes, identityScope)

	for _, scope := range []string{scopeOpenID, scopeFHIRUser} {
		if slices.Contains(view.Grantable, scope) {
			t.Errorf("%s is offered by a deployment that cannot sign", scope)
		}

		if !slices.ContainsFunc(view.Refused, func(one refusedScope) bool {
			return one.Scope == scope && one.Reason != ""
		}) {
			t.Errorf("%s was dropped without a reason", scope)
		}
	}

	if answer := fetching(t, routes, openIDConfigurationPath); answer.Code == http.StatusOK {
		t.Errorf("a deployment that cannot sign published a configuration: %s", answer.Body)
	}

	if answer := fetching(t, routes, oauthBasePath+identityKeysPath); answer.Code == http.StatusOK {
		t.Errorf("a deployment that cannot sign published a key set: %s", answer.Body)
	}

	smart := fetching(t, routes, smartConfigurationPath)
	if smart.Code != http.StatusOK {
		t.Fatalf("the SMART configuration answered %d", smart.Code)
	}

	var advertised smartConfiguration
	if err := json.Unmarshal(smart.Body.Bytes(), &advertised); err != nil {
		t.Fatalf("decode the SMART configuration: %v", err)
	}

	if advertised.Issuer != "" {
		t.Errorf("a deployment issuing no identity token names itself issuer %q", advertised.Issuer)
	}

	if slices.Contains(advertised.Capabilities, "sso-openid-connect") {
		t.Error("a deployment issuing no identity token claims sso-openid-connect")
	}
}

// describedScopes reads what this server would grant for one ask.
func describedScopes(t *testing.T, routes http.Handler, scope string) authorizationView {
	t.Helper()

	described := authorizing(t, routes, http.MethodGet, signedIn(t, routes), askingFor(scope), "")
	if described.Code != http.StatusOK {
		t.Fatalf("describe: %d %s", described.Code, described.Body)
	}

	var view authorizationView
	if err := json.Unmarshal(described.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode the description: %v", err)
	}

	return view
}

// TestAnIdentityIsRefusedForSomebodyWhoIsNoResource.
//
// fhirUser names the record the person is. An account whose membership names
// none cannot have the claim, and saying so at consent is what stops an app
// being told it holds something the token will not carry.
func TestAnIdentityIsRefusedForSomebodyWhoIsNoResource(t *testing.T) {
	routes, db := signingServer(t)

	if _, err := db.ExecContext(t.Context(),
		"UPDATE project_memberships SET profile_type = NULL, profile_id = NULL"+
			" WHERE id = 'pm_nurse'"); err != nil {
		t.Fatalf("unname the resource: %v", err)
	}

	view := describedScopes(t, routes, identityScope)

	if slices.Contains(view.Grantable, scopeFHIRUser) {
		t.Error("fhirUser is offered to somebody who is no resource")
	}

	// openid still is: this server knows who signed in, it simply cannot say
	// which record they are.
	if !slices.Contains(view.Grantable, scopeOpenID) {
		t.Error("openid was refused for a reason that only concerns fhirUser")
	}
}

// TestProfileIsRefusedRatherThanGuessedAt.
//
// SMART calls `profile` a deprecated synonym for fhirUser; OpenID Connect gives
// it a claim set of its own. Granting it would be answering one reading and
// disappointing the other, and the app that asked cannot tell which happened.
func TestProfileIsRefusedRatherThanGuessedAt(t *testing.T) {
	routes, _ := signingServer(t)

	view := describedScopes(t, routes, "openid profile user/Organization.read")

	if slices.Contains(view.Grantable, scopeProfile) {
		t.Error("profile was granted, and this server answers only one of its two meanings")
	}

	if !slices.ContainsFunc(view.Refused, func(one refusedScope) bool {
		return one.Scope == scopeProfile && strings.Contains(one.Reason, scopeFHIRUser)
	}) {
		t.Error("profile was refused without pointing at the scope this server does answer")
	}
}
