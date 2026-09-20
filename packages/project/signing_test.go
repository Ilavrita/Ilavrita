package project_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// aSigningKey mints one for a test.
func aSigningKey(t *testing.T) project.SigningKey {
	t.Helper()

	key, err := project.NewSigningKey("identity", rand.Reader)
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}

	return key
}

// someClaims are a complete, valid claim set.
func someClaims() project.IdentityClaims {
	now := time.Now().UTC()

	return project.IdentityClaims{
		Issuer:    "https://ilavrita.example.test",
		Subject:   "usr_ward",
		Audience:  "cli_ward",
		FHIRUser:  "https://ilavrita.example.test/fhir/R4/Practitioner/ward",
		IssuedAt:  now,
		ExpiresAt: now.Add(5 * time.Minute),
	}
}

// publishedKey reads the one public key out of a published set.
func publishedKey(t *testing.T, document string) (*rsa.PublicKey, map[string]string) {
	t.Helper()

	var held struct {
		Keys []map[string]string `json:"keys"`
	}

	if err := json.Unmarshal([]byte(document), &held); err != nil {
		t.Fatalf("the published set is not JSON: %v", err)
	}

	if len(held.Keys) != 1 {
		t.Fatalf("the published set carries %d keys, want 1", len(held.Keys))
	}

	entry := held.Keys[0]

	modulus, err := base64.RawURLEncoding.DecodeString(entry["n"])
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}

	exponent, err := base64.RawURLEncoding.DecodeString(entry["e"])
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(modulus),
		E: int(new(big.Int).SetBytes(exponent).Int64()),
	}, entry
}

// TestASignedIdentityTokenVerifiesAgainstThePublishedKey.
//
// This is the whole contract: a client reads the key set at `jwks_uri`, finds
// the key the token's `kid` names, and checks the signature with it. A token
// that does not verify that way is one every conformant client refuses, and
// nothing else in this file matters if this does not hold.
func TestASignedIdentityTokenVerifiesAgainstThePublishedKey(t *testing.T) {
	key := aSigningKey(t)

	token, err := key.Sign(someClaims())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a compact JWS has three parts, this has %d", len(parts))
	}

	document, err := key.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	public, entry := publishedKey(t, document)

	// The kid has to resolve, because that is how a reader picks the key.
	if entry["kid"] != key.ID() {
		t.Errorf("the published key is %q, the key is %q", entry["kid"], key.ID())
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode the signature: %v", err)
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the published key does not verify the token it signed: %v", err)
	}
}

// TestAnIdentityTokenIsSignedWithRS256, which SMART requires by name for this
// token and which is not the algorithm a client assertion uses.
func TestAnIdentityTokenIsSignedWithRS256(t *testing.T) {
	key := aSigningKey(t)

	token, err := key.Sign(someClaims())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatalf("decode the header: %v", err)
	}

	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}

	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatalf("the header is not JSON: %v", err)
	}

	if header.Algorithm != "RS256" {
		t.Errorf("the token is signed with %q, want RS256", header.Algorithm)
	}

	if header.Type != "JWT" {
		t.Errorf("the header says typ %q, want JWT", header.Type)
	}

	if header.KeyID != key.ID() {
		t.Errorf("the header names kid %q, want %q", header.KeyID, key.ID())
	}

	// The published set has to agree, or a reader checking the advertised
	// algorithm before verifying refuses a token that would have verified.
	document, err := key.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	if _, entry := publishedKey(t, document); entry["alg"] != "RS256" {
		t.Errorf("the published key advertises %q, want RS256", entry["alg"])
	}
}

// TestThePublishedKeySetCarriesNoPrivateMaterial.
//
// The document goes to anybody who asks. A private member reaching it hands
// over the key that signs every identity this server asserts.
func TestThePublishedKeySetCarriesNoPrivateMaterial(t *testing.T) {
	document, err := aSigningKey(t).PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	var held struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}

	if err := json.Unmarshal([]byte(document), &held); err != nil {
		t.Fatalf("the published set is not JSON: %v", err)
	}

	// Every private RSA member RFC 7518 names, not just the obvious one.
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi", "oth"} {
		if _, found := held.Keys[0][private]; found {
			t.Errorf("the published set carries %q, which is private material", private)
		}
	}
}

// TestAStoredSigningKeyComesBackSigningTheSameWay, so a restart does not
// invalidate a token this server minted before it.
func TestAStoredSigningKeyComesBackSigningTheSameWay(t *testing.T) {
	key := aSigningKey(t)

	encoded, err := key.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	restored, err := project.ParseSigningKey(key.ID(), encoded)
	if err != nil {
		t.Fatalf("ParseSigningKey: %v", err)
	}

	first, err := key.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	second, err := restored.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	if first != second {
		t.Error("a restored key publishes a different set, so tokens minted before a restart" +
			" would no longer verify")
	}
}

// TestASigningKeyNobodyMintedSignsNothing, rather than producing a token with
// no signature behind it.
func TestASigningKeyNobodyMintedSignsNothing(t *testing.T) {
	var none project.SigningKey

	if !none.IsZero() {
		t.Fatal("the zero value does not report itself as unminted")
	}

	if _, err := none.Sign(someClaims()); err == nil {
		t.Error("a key nobody minted signed a token")
	}

	if _, err := none.PublishedJWKS(); err == nil {
		t.Error("a key nobody minted published a key set")
	}

	if _, err := none.Marshal(); err == nil {
		t.Error("a key nobody minted marshalled")
	}
}

// TestAnIdentityTokenNeedsEveryClaimAReaderChecks.
//
// A reader verifies `iss`, `sub`, `aud` and `exp` and refuses the token if any
// is missing or wrong. Minting one anyway produces a token that fails at the
// client, where the reason is hardest to see.
func TestAnIdentityTokenNeedsEveryClaimAReaderChecks(t *testing.T) {
	key := aSigningKey(t)
	now := time.Now().UTC()

	for name, claims := range map[string]project.IdentityClaims{
		"no issuer":   {Subject: "s", Audience: "a", IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
		"no subject":  {Issuer: "i", Audience: "a", IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
		"no audience": {Issuer: "i", Subject: "s", IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
		"a blank issuer": {
			Issuer: "   ", Subject: "s", Audience: "a", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		},
		"expiring when it was issued": {
			Issuer: "i", Subject: "s", Audience: "a", IssuedAt: now, ExpiresAt: now,
		},
		"already expired": {
			Issuer: "i", Subject: "s", Audience: "a", IssuedAt: now, ExpiresAt: now.Add(-time.Minute),
		},
		"a subject too long for the field": {
			Issuer: "i", Subject: strings.Repeat("s", 256), Audience: "a",
			IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := key.Sign(claims); err == nil {
				t.Errorf("a token with %s was signed", name)
			}
		})
	}
}

// TestAKeyTooShortToSignWithIsRefusedOnTheWayIn, rather than at the client that
// will not accept what it produced.
func TestAKeyTooShortToSignWithIsRefusedOnTheWayIn(t *testing.T) {
	// Deliberately short: this test exists to prove such a key is refused, so
	// the weakness is the fixture rather than something the build would ship.
	short, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // the subject of the test
	if err != nil {
		t.Fatalf("generate a short key: %v", err)
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(short)
	if err != nil {
		t.Fatalf("marshal the short key: %v", err)
	}

	if _, err := project.ParseSigningKey("identity", encoded); !errors.Is(err, project.ErrInvalidJWKS) {
		t.Errorf("a 1024-bit key was accepted, err = %v", err)
	}
}

// TestASigningKeyNeedsAnIdentifier, because the kid is how a reader finds it in
// the published set, and a token naming none is one a multi-key set cannot
// resolve.
func TestASigningKeyNeedsAnIdentifier(t *testing.T) {
	if _, err := project.NewSigningKey("  ", rand.Reader); err == nil {
		t.Error("a key with no identifier was minted")
	}

	key := aSigningKey(t)

	encoded, err := key.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := project.ParseSigningKey("", encoded); err == nil {
		t.Error("a key with no identifier was restored")
	}
}

// TestSomethingThatIsNotAKeyIsRefused, so a corrupted or mistyped row fails
// where it is read rather than where it is used.
func TestSomethingThatIsNotAKeyIsRefused(t *testing.T) {
	for name, encoded := range map[string][]byte{
		"nothing":     nil,
		"not DER":     []byte("this is not a key"),
		"truncated":   {0x30, 0x82, 0x04},
		"empty bytes": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := project.ParseSigningKey("identity", encoded); err == nil {
				t.Errorf("%s was read as a signing key", name)
			}
		})
	}
}
