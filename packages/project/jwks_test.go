package project

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// rsaSet is a JWK Set holding one RSA public key of the given size.
func rsaSet(t *testing.T, bits int, id string) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	return setOf(t, map[string]string{
		"kty": "RSA",
		"kid": id,
		"alg": "RS384",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	})
}

// ecSet is a JWK Set holding one P-384 public key.
func ecSet(t *testing.T, id string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	return setOf(t, map[string]string{
		"kty": "EC",
		"kid": id,
		"alg": "ES384",
		"crv": "P-384",
		"x":   base64.RawURLEncoding.EncodeToString(key.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(key.Y.Bytes()),
	})
}

// setOf renders one key as a JWK Set.
func setOf(t *testing.T, key map[string]string) string {
	t.Helper()

	encoded, err := json.Marshal(map[string]any{"keys": []map[string]string{key}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	return string(encoded)
}

// TestAKeySetThisServerCannotVerifyAgainstIsRefused.
//
// Every one of these would otherwise be stored and fail only at the moment an
// assertion arrived — which is the moment a backend service finds out its
// registration never worked.
func TestAKeySetThisServerCannotVerifyAgainstIsRefused(t *testing.T) {
	for name, stated := range map[string]string{
		"not json":        "{",
		"naming no key":   `{"keys":[]}`,
		"no keys member":  `{}`,
		"an unknown type": `{"keys":[{"kty":"oct","k":"c2VjcmV0"}]}`,

		"an rsa key missing its exponent":        `{"keys":[{"kty":"RSA","n":"AAAA"}]}`,
		"a member that is not base64url":         `{"keys":[{"kty":"RSA","n":"!!!!","e":"AQAB"}]}`,
		"a P-384 coordinate of the wrong length": `{"keys":[{"kty":"EC","crv":"P-384","x":"AQAB","y":"AQAB"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJWKS(stated); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("error: got %v, want ErrInvalidJWKS", err)
			}
		})
	}
}

// TestACurveThisServerDoesNotVerifyAgainstIsRefused.
//
// The key is a genuine, well-formed P-256 key. That matters: a malformed one
// would be refused by the member checks whether or not the curve were examined,
// and the test would pass while proving nothing about curves.
func TestACurveThisServerDoesNotVerifyAgainstIsRefused(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	stated := setOf(t, map[string]string{
		"kty": "EC", "kid": "p256", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(key.X.Bytes()),
		"y": base64.RawURLEncoding.EncodeToString(key.Y.Bytes()),
	})

	if _, err := ParseJWKS(stated); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error: got %v, want ErrInvalidJWKS", err)
	}
}

// TestAKeyTooShortToSignWithIsRefused, rather than accepted because it parses.
func TestAKeyTooShortToSignWithIsRefused(t *testing.T) {
	if _, err := ParseJWKS(rsaSet(t, 1024, "small")); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error: got %v, want ErrInvalidJWKS", err)
	}

	if _, err := ParseJWKS(rsaSet(t, 2048, "fine")); err != nil {
		t.Errorf("a 2048-bit key was refused: %v", err)
	}
}

// TestAKeySetCarryingPrivateMaterialIsRefused.
//
// A private key in a set a client registered is one its owner has already
// published. Storing it would make this database the place that leaked it, and
// nothing this server does afterwards can unpublish it.
func TestAKeySetCarryingPrivateMaterialIsRefused(t *testing.T) {
	// Everything else about this key is valid — a real P-384 point on the right
	// curve — so the only thing that can refuse it is the private member. A
	// malformed key would be refused anyway, and would prove nothing.
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	member := map[string]string{
		"kty": "EC", "kid": "leaked", "crv": "P-384",
		"x": base64.RawURLEncoding.EncodeToString(key.X.Bytes()),
		"y": base64.RawURLEncoding.EncodeToString(key.Y.Bytes()),
	}

	// Without the private member it is a key this server accepts, which is what
	// makes the refusal below about the private member and nothing else.
	if _, err := ParseJWKS(setOf(t, member)); err != nil {
		t.Fatalf("the same key without private material was refused: %v", err)
	}

	private, err := key.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	member["d"] = base64.RawURLEncoding.EncodeToString(private)

	if _, err := ParseJWKS(setOf(t, member)); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error: got %v, want ErrInvalidJWKS", err)
	}
}

// TestASetIsRefusedWholeRatherThanInPart, because a registration that silently
// kept half its keys is one whose owner believes it registered both.
func TestASetIsRefusedWholeRatherThanInPart(t *testing.T) {
	good := rsaSet(t, 2048, "good")

	var document map[string]any
	if err := json.Unmarshal([]byte(good), &document); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	keys, _ := document["keys"].([]any)
	document["keys"] = append(keys,
		map[string]string{"kty": "EC", "crv": "P-256", "x": "AA", "y": "AA"})

	mixed, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := ParseJWKS(string(mixed)); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error: got %v, want ErrInvalidJWKS", err)
	}
}

// TestARegistrationStatingNoKeySetIsNotAnError, because a browser app has no key
// to state and refusing one would make it unregistrable.
func TestARegistrationStatingNoKeySetIsNotAnError(t *testing.T) {
	for _, stated := range []string{"", "   "} {
		held, err := ParseJWKS(stated)
		if err != nil {
			t.Fatalf("ParseJWKS(%q): %v", stated, err)
		}

		if !held.IsZero() || held.Len() != 0 {
			t.Errorf("a registration stating nothing held %d keys", held.Len())
		}

		// And it verifies against nothing, so "stated none" never reads as
		// "accepts any".
		if len(held.KeysFor("")) != 0 {
			t.Error("a registration stating no key offered one to verify against")
		}
	}
}

// TestANamedKeyIsTheOnlyOneTried.
//
// A client naming a key is saying which one signed. Trying the others would make
// the id decorative, and would let a signature made with a key the client
// retired verify against one it had not.
func TestANamedKeyIsTheOnlyOneTried(t *testing.T) {
	var a, b map[string]any

	if err := json.Unmarshal([]byte(rsaSet(t, 2048, "one")), &a); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if err := json.Unmarshal([]byte(ecSet(t, "two")), &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	keysA, _ := a["keys"].([]any)
	keysB, _ := b["keys"].([]any)
	a["keys"] = append(keysA, keysB...)

	both, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	held, err := ParseJWKS(string(both))
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	if held.Len() != 2 {
		t.Fatalf("the set holds %d keys, want 2", held.Len())
	}

	if got := len(held.KeysFor("one")); got != 1 {
		t.Errorf("naming one key offered %d to verify against, want 1", got)
	}

	if got := len(held.KeysFor("two")); got != 1 {
		t.Errorf("naming the other key offered %d, want 1", got)
	}

	if got := len(held.KeysFor("neither")); got != 0 {
		t.Errorf("naming a key nobody registered offered %d, want none", got)
	}

	// An assertion naming no key is tried against the whole set, which is what a
	// single-key registration looks like.
	if got := len(held.KeysFor("")); got != 2 {
		t.Errorf("naming no key offered %d, want the whole set", got)
	}
}

// TestASetIsHeldExactlyAsItWasRegistered, so a read hands back what was written
// rather than a re-serialisation that might differ.
func TestASetIsHeldExactlyAsItWasRegistered(t *testing.T) {
	stated := rsaSet(t, 2048, "one")

	held, err := ParseJWKS("  " + stated + "  ")
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	if held.Document() != stated {
		t.Errorf("held %q, want the document as registered", held.Document())
	}
}

// TestOnlyTheAlgorithmsSmartNamesAreOffered. A verifier constrains itself to
// this list rather than reading the algorithm out of the token it is checking,
// which is the whole of the alg-confusion family.
func TestOnlyTheAlgorithmsSmartNamesAreOffered(t *testing.T) {
	held := SigningAlgorithms()

	if len(held) != 2 || held[0] != "RS384" || held[1] != "ES384" {
		t.Fatalf("algorithms are %v, want RS384 and ES384", held)
	}

	for _, refused := range []string{"none", "HS256", "RS256"} {
		if strings.Contains(strings.Join(held, " "), refused) {
			t.Errorf("%q is offered", refused)
		}
	}

	// The slice is a copy, so a caller cannot widen what this server accepts by
	// writing to what it was handed.
	held[0] = "none"

	if SigningAlgorithms()[0] != "RS384" {
		t.Error("a caller widened the accepted algorithms by writing to the returned slice")
	}
}
