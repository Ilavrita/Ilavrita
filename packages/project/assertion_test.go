package project

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// assertionAudience is what an assertion must be addressed to: this server's
// token endpoint.
//
// Named for the claim rather than the endpoint, because a constant pairing
// "token" with a URL reads to gosec as a credential in the source. It is an
// address, and a public one.
const assertionAudience = "https://ilavrita.example.test/oauth2/token"

// assertedAt is the instant every assertion here is judged at.
var assertedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// signingKey is one client's key pair, with the JWK Set its registration holds.
type signingKey struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	set string
}

// anRSAKey generates a key and the set a registration would hold for it.
func anRSAKey(t *testing.T, id string) signingKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	return signingKey{rsa: key, set: setOf(t, map[string]string{
		"kty": "RSA", "kid": id, "alg": "RS384",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	})}
}

// anECKey generates a P-384 key and the set a registration would hold for it.
func anECKey(t *testing.T, id string) signingKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	return signingKey{ec: key, set: setOf(t, map[string]string{
		"kty": "EC", "kid": id, "alg": "ES384", "crv": "P-384",
		"x": base64.RawURLEncoding.EncodeToString(padded(key.X, p384Coordinate)),
		"y": base64.RawURLEncoding.EncodeToString(padded(key.Y, p384Coordinate)),
	})}
}

// padded renders a coordinate at the fixed width a JWK states it in.
func padded(value *big.Int, width int) []byte {
	held := make([]byte, width)
	value.FillBytes(held)

	return held
}

// registeredWith is a client application holding one key set.
func registeredWith(t *testing.T, set string) ClientApplication {
	t.Helper()

	keys, err := ParseJWKS(set)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	app, err := NewClientApplication("prj_a", ClientApplicationConfig{
		ID: "cli_service", Name: "Nightly loader", State: ServiceActive,
		Kind: ClientConfidential, JWKS: keys,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	return app
}

// claimsFor is a well-formed set of claims, so each case changes one thing.
func claimsFor() map[string]any {
	return map[string]any{
		"iss": "cli_service",
		"sub": "cli_service",
		"aud": assertionAudience,
		"exp": assertedAt.Add(time.Minute).Unix(),
		"iat": assertedAt.Unix(),
		"jti": "a-jti-the-client-chose",
	}
}

// sign renders a compact JWS with the stated header and claims.
func sign(t *testing.T, key signingKey, header, claims map[string]any) string {
	t.Helper()

	encode := func(held map[string]any) string {
		raw, err := json.Marshal(held)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		return base64.RawURLEncoding.EncodeToString(raw)
	}

	signed := encode(header) + "." + encode(claims)
	digest := sha512.Sum384([]byte(signed))

	var (
		signature []byte
		err       error
	)

	switch {
	case key.rsa != nil:
		signature, err = rsa.SignPKCS1v15(rand.Reader, key.rsa, crypto.SHA384, digest[:])
	case key.ec != nil:
		var r, s *big.Int

		r, s, err = ecdsa.Sign(rand.Reader, key.ec, digest[:])
		if err == nil {
			signature = append(padded(r, p384Coordinate), padded(s, p384Coordinate)...)
		}
	default:
		t.Fatal("the key signs nothing")
	}

	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// TestAnAssertionProvesTheKeyItWasSignedWith, for both algorithms SMART names.
func TestAnAssertionProvesTheKeyItWasSignedWith(t *testing.T) {
	for name, key := range map[string]signingKey{
		"RS384": anRSAKey(t, "one"),
		"ES384": anECKey(t, "one"),
	} {
		t.Run(name, func(t *testing.T) {
			token := sign(t, key, map[string]any{"alg": name, "typ": "JWT", "kid": "one"}, claimsFor())

			held, err := VerifyClientAssertion(
				token, registeredWith(t, key.set), assertionAudience, assertedAt)
			if err != nil {
				t.Fatalf("VerifyClientAssertion: %v", err)
			}

			if held.Client() != "cli_service" {
				t.Errorf("the assertion proved %s", held.Client())
			}

			if held.ID() != "a-jti-the-client-chose" {
				t.Errorf("the jti is %q, want the one the client chose", held.ID())
			}

			if !held.ExpiresAt().Equal(assertedAt.Add(time.Minute)) {
				t.Errorf("the expiry is %s, want the one the assertion stated", held.ExpiresAt())
			}
		})
	}
}

// TestASignatureAnotherKeyMadeIsRefused, which is the whole of what an assertion
// proves.
func TestASignatureAnotherKeyMadeIsRefused(t *testing.T) {
	registered := anRSAKey(t, "one")
	impostor := anRSAKey(t, "one")

	token := sign(t, impostor, map[string]any{"alg": "RS384", "kid": "one"}, claimsFor())

	_, err := VerifyClientAssertion(
		token, registeredWith(t, registered.set), assertionAudience, assertedAt)
	if !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
	}
}

// TestATamperedAssertionIsRefused. The signature covers the encoded parts as
// they arrived, so changing a claim after signing breaks it.
func TestATamperedAssertionIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claimsFor())
	parts := strings.Split(token, ".")

	forged := claimsFor()
	forged["jti"] = "another-jti"

	raw, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	parts[1] = base64.RawURLEncoding.EncodeToString(raw)

	if _, err := VerifyClientAssertion(
		strings.Join(parts, "."), registered, assertionAudience, assertedAt,
	); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
	}
}

// TestTheAlgorithmIsThisServersChoiceAndNotTheTokens.
//
// A verifier that reads the algorithm out of the header is one an attacker tells
// which algorithm to use. "none" verifies everything, and an HMAC verifies
// against a public key anybody can read — both are the same bug, which is
// trusting the token about how to check the token.
func TestTheAlgorithmIsThisServersChoiceAndNotTheTokens(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	for _, algorithm := range []string{"none", "HS256", "RS256", "ES256", ""} {
		t.Run(algorithm, func(t *testing.T) {
			token := sign(t, key, map[string]any{"alg": algorithm, "kid": "one"}, claimsFor())

			if _, err := VerifyClientAssertion(
				token, registered, assertionAudience, assertedAt,
			); !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
			}
		})
	}
}

// TestAnAssertionAimedSomewhereElseIsRefused.
//
// Without this check, an assertion a client made for one server is one that
// server can present to another as if it were the client. It is the reason the
// audience claim exists.
func TestAnAssertionAimedSomewhereElseIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	for name, audience := range map[string]any{
		"another server":        "https://elsewhere.example.test/oauth2/token",
		"this server's base":    "https://ilavrita.example.test",
		"nothing at all":        nil,
		"an array without this": []any{"https://a.test", "https://b.test"},
		"a number":              1,
	} {
		t.Run(name, func(t *testing.T) {
			claims := claimsFor()
			claims["aud"] = audience

			token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claims)

			if _, err := VerifyClientAssertion(
				token, registered, assertionAudience, assertedAt,
			); !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
			}
		})
	}

	// An array naming this server among others is addressed to it, which is what
	// RFC 7519 says an audience list means.
	claims := claimsFor()
	claims["aud"] = []any{"https://a.test", assertionAudience}

	token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claims)

	if _, err := VerifyClientAssertion(token, registered, assertionAudience, assertedAt); err != nil {
		t.Errorf("an audience list naming this server was refused: %v", err)
	}
}

// TestAnAssertionClaimingAnotherClientIsRefused, in either claim: a client
// asserting somebody else's identity is asserting what it cannot prove.
func TestAnAssertionClaimingAnotherClientIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	for name, change := range map[string]func(map[string]any){
		"another issuer":  func(c map[string]any) { c["iss"] = "cli_other" },
		"another subject": func(c map[string]any) { c["sub"] = "cli_other" },
		"no issuer":       func(c map[string]any) { delete(c, "iss") },
		"no subject":      func(c map[string]any) { delete(c, "sub") },
	} {
		t.Run(name, func(t *testing.T) {
			claims := claimsFor()
			change(claims)

			token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claims)

			if _, err := VerifyClientAssertion(
				token, registered, assertionAudience, assertedAt,
			); !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
			}
		})
	}
}

// TestAnAssertionWithNothingToSpendIsRefused, because a jti is what makes one
// assertion authenticate once rather than forever.
func TestAnAssertionWithNothingToSpendIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	claims := claimsFor()
	delete(claims, "jti")

	token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claims)

	if _, err := VerifyClientAssertion(
		token, registered, assertionAudience, assertedAt,
	); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
	}
}

// TestAnAssertionOutsideItsWindowIsRefused, at both ends: an expired one proves
// nothing now, and a long-lived one asks this server to remember its jti for as
// long as it lives.
func TestAnAssertionOutsideItsWindowIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	for name, expires := range map[string]int64{
		"expired":           assertedAt.Add(-time.Second).Unix(),
		"exactly expired":   assertedAt.Unix(),
		"living too long":   assertedAt.Add(maxAssertionLifetime + time.Minute).Unix(),
		"living for a year": assertedAt.Add(365 * 24 * time.Hour).Unix(),
		"stating none":      0,
	} {
		t.Run(name, func(t *testing.T) {
			claims := claimsFor()
			claims["exp"] = expires

			token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claims)

			if _, err := VerifyClientAssertion(
				token, registered, assertionAudience, assertedAt,
			); !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
			}
		})
	}
}

// TestAnAssertionNamingAKeyNobodyRegisteredIsRefused, rather than falling back
// to whatever else the registration holds.
func TestAnAssertionNamingAKeyNobodyRegisteredIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")

	token := sign(t, key, map[string]any{"alg": "RS384", "kid": "retired"}, claimsFor())

	if _, err := VerifyClientAssertion(
		token, registeredWith(t, key.set), assertionAudience, assertedAt,
	); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
	}
}

// TestARegistrationHoldingNoKeyProvesNothing, so a client that registered none
// cannot authenticate by presenting something shaped like an assertion.
func TestARegistrationHoldingNoKeyProvesNothing(t *testing.T) {
	key := anRSAKey(t, "one")

	app, err := NewClientApplication("prj_a", ClientApplicationConfig{
		ID: "cli_service", Name: "Nightly loader", State: ServiceActive,
		Kind: ClientConfidential,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	token := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claimsFor())

	if _, err := VerifyClientAssertion(
		token, app, assertionAudience, assertedAt,
	); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
	}
}

// TestSomethingThatIsNotAnAssertionIsRefused, so the verifier never has to
// decide what half a token means.
func TestSomethingThatIsNotAnAssertionIsRefused(t *testing.T) {
	key := anRSAKey(t, "one")
	registered := registeredWith(t, key.set)

	valid := sign(t, key, map[string]any{"alg": "RS384", "kid": "one"}, claimsFor())
	parts := strings.Split(valid, ".")

	for name, token := range map[string]string{
		"empty":      "",
		"two parts":  parts[0] + "." + parts[1],
		"four parts": valid + ".extra",
		"a header that is not json": base64.RawURLEncoding.EncodeToString([]byte("{")) +
			"." + parts[1] + "." + parts[2],
		"a body that is not base64url": parts[0] + ".!!!." + parts[2],
		"longer than one carries":      strings.Repeat("a", maxAssertion+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyClientAssertion(
				token, registered, assertionAudience, assertedAt,
			); !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error: got %v, want ErrInvalidAssertion", err)
			}
		})
	}
}
