package project

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// maxJWKS bounds a registered key set. A backend service registers one or two
// keys and rotates them; a document longer than this is a registration being
// used as storage.
const maxJWKS = 16384

// maxRegisteredKeys bounds how many keys one registration names, for the same
// reason and so that verifying an assertion cannot be made expensive by
// registering a thousand keys to try it against.
const maxRegisteredKeys = 8

// minRSAModulus is the shortest RSA key this server verifies against, in bytes.
// A key below 2048 bits is one a client should not be signing with, and one this
// server should not accept merely because it parses.
const minRSAModulus = 256

// signingAlgorithms are the ones a client assertion may be signed with.
//
// SMART Backend Services requires RS384 and permits ES384. Both are listed
// explicitly rather than taken from the token's own header, because a verifier
// that trusts the header is one an attacker tells which algorithm to use — that
// is the whole of the "alg: none" and HMAC-confusion family.
var signingAlgorithms = []string{"RS384", "ES384"}

// SigningAlgorithms returns the algorithms a client assertion may use, for the
// discovery document to publish and for a verifier to constrain itself to.
func SigningAlgorithms() []string {
	return append([]string(nil), signingAlgorithms...)
}

// JWKS is the public keys one registration signs its assertions with.
//
// The zero value is a registration that stated none, which is what every client
// doing no backend-services flow is. That is not an error: a browser app has no
// key to state.
type JWKS struct {
	// raw is the document exactly as registered, kept so a read hands back what
	// was written rather than a re-serialisation that might differ.
	raw string

	keys []registeredKey
}

// registeredKey is one public key, already decoded into something that can
// verify.
type registeredKey struct {
	id     string
	public crypto.PublicKey
}

// jwkDocument is the JSON shape a JWK Set has.
type jwkDocument struct {
	Keys []jwkEntry `json:"keys"`
}

// jwkEntry is one key in that set. Only the members needed to verify are read;
// anything else a client includes survives in raw and is ignored here.
type jwkEntry struct {
	Kind      string `json:"kty"`
	ID        string `json:"kid"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	Curve     string `json:"crv"`

	// RSA.
	Modulus  string `json:"n"`
	Exponent string `json:"e"`

	// EC.
	X string `json:"x"`
	Y string `json:"y"`

	// A private key member. Its presence is what this refuses: a client that
	// registered its private key has published it, and nothing this server does
	// afterwards can unpublish it.
	Private string `json:"d"`
}

// ParseJWKS reads the key set a registration states.
//
// It is the one way an outside value becomes a JWKS, so every stored set is one
// whose keys this server can actually verify against. A set containing a key it
// cannot is refused whole rather than in part: a registration that silently kept
// half its keys is one whose owner believes it registered both.
func ParseJWKS(stated string) (JWKS, error) {
	stated = strings.TrimSpace(stated)
	if stated == "" {
		return JWKS{}, nil
	}

	if len(stated) > maxJWKS {
		return JWKS{}, fmt.Errorf("%w: %d characters exceeds the %d a registration carries",
			ErrInvalidJWKS, len(stated), maxJWKS)
	}

	var document jwkDocument
	if err := json.Unmarshal([]byte(stated), &document); err != nil {
		return JWKS{}, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}

	if len(document.Keys) == 0 {
		return JWKS{}, fmt.Errorf("%w: the set names no key", ErrInvalidJWKS)
	}

	if len(document.Keys) > maxRegisteredKeys {
		return JWKS{}, fmt.Errorf("%w: %d keys exceeds the %d a registration names",
			ErrInvalidJWKS, len(document.Keys), maxRegisteredKeys)
	}

	held := make([]registeredKey, 0, len(document.Keys))

	for _, entry := range document.Keys {
		// A private key in a public set is a key its owner has published. This
		// server refuses to hold one, because accepting it would make this
		// database the place that leaked it.
		if entry.Private != "" {
			return JWKS{}, fmt.Errorf("%w: a key carries private material", ErrInvalidJWKS)
		}

		// A key stating what it is for must be for signing. One stating nothing
		// is permitted, because "use" is optional and most sets omit it.
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}

		key, err := entry.decode()
		if err != nil {
			return JWKS{}, err
		}

		held = append(held, key)
	}

	if len(held) == 0 {
		return JWKS{}, fmt.Errorf(
			"%w: the set names no key that can verify a signature", ErrInvalidJWKS)
	}

	return JWKS{raw: stated, keys: held}, nil
}

// decode turns one entry into a key that can verify.
func (e jwkEntry) decode() (registeredKey, error) {
	switch e.Kind {
	case "RSA":
		return e.decodeRSA()
	case "EC":
		return e.decodeEC()
	default:
		return registeredKey{}, fmt.Errorf(
			"%w: %q is not a key type this server verifies against", ErrInvalidJWKS, e.Kind)
	}
}

// decodeRSA reads an RSA public key.
func (e jwkEntry) decodeRSA() (registeredKey, error) {
	modulus, err := decodeJWKBytes(e.Modulus)
	if err != nil {
		return registeredKey{}, err
	}

	exponent, err := decodeJWKBytes(e.Exponent)
	if err != nil {
		return registeredKey{}, err
	}

	if len(modulus) < minRSAModulus {
		return registeredKey{}, fmt.Errorf(
			"%w: an RSA key of %d bits is below the 2048 this server accepts",
			ErrInvalidJWKS, len(modulus)*8)
	}

	if len(exponent) == 0 || len(exponent) > 8 {
		return registeredKey{}, fmt.Errorf(
			"%w: an RSA exponent this server cannot read", ErrInvalidJWKS)
	}

	held := 0
	for _, one := range exponent {
		held = held<<8 | int(one)
	}

	return registeredKey{
		id:     e.ID,
		public: &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: held},
	}, nil
}

// p384Coordinate is how many bytes each coordinate of a P-384 point occupies.
// RFC 7518 requires a JWK to state them at full length, zero-padded, so a
// shorter one is a key from another curve or a truncated member rather than a
// small number.
const p384Coordinate = 48

// decodeEC reads an elliptic-curve public key.
func (e jwkEntry) decodeEC() (registeredKey, error) {
	// P-384 alone, because ES384 is the only EC algorithm accepted above and a
	// key on another curve could never produce a signature this verifies.
	if e.Curve != "P-384" {
		return registeredKey{}, fmt.Errorf(
			"%w: %q is not a curve this server verifies against", ErrInvalidJWKS, e.Curve)
	}

	x, err := decodeJWKBytes(e.X)
	if err != nil {
		return registeredKey{}, err
	}

	y, err := decodeJWKBytes(e.Y)
	if err != nil {
		return registeredKey{}, err
	}

	if len(x) != p384Coordinate || len(y) != p384Coordinate {
		return registeredKey{}, fmt.Errorf(
			"%w: a P-384 coordinate is %d bytes, and this key states %d and %d",
			ErrInvalidJWKS, p384Coordinate, len(x), len(y))
	}

	// The SEC1 uncompressed encoding, which is what the standard-library parser
	// reads. It performs the on-curve check itself — a point that is not on the
	// curve is not a key, and verifying against one is undefined rather than
	// merely wrong — so the check belongs to the parser rather than to a
	// hand-written comparison beside it.
	uncompressed := make([]byte, 0, 1+2*p384Coordinate)
	uncompressed = append(uncompressed, 4)
	uncompressed = append(uncompressed, x...)
	uncompressed = append(uncompressed, y...)

	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P384(), uncompressed)
	if err != nil {
		return registeredKey{}, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}

	return registeredKey{id: e.ID, public: public}, nil
}

// decodeJWKBytes reads one base64url member.
func decodeJWKBytes(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("%w: a key is missing a member", ErrInvalidJWKS)
	}

	held, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}

	return held, nil
}

// Document returns the key set exactly as it was registered.
func (j JWKS) Document() string { return j.raw }

// IsZero reports whether this registration stated no key, which is what a client
// doing no backend-services flow looks like.
func (j JWKS) IsZero() bool { return len(j.keys) == 0 }

// Len returns how many verifying keys the set holds.
func (j JWKS) Len() int { return len(j.keys) }

// KeysFor returns the public keys a signature naming this key id may be verified
// against.
//
// A stated id selects the key with that id and nothing else: a client that names
// a key is a client saying which one signed, and trying the others would make
// the id decorative. An assertion naming no id is verified against every key in
// the set, which is what a single-key registration looks like.
func (j JWKS) KeysFor(id string) []crypto.PublicKey {
	held := make([]crypto.PublicKey, 0, len(j.keys))

	for _, key := range j.keys {
		if id != "" && key.id != "" && key.id != id {
			continue
		}

		held = append(held, key.public)
	}

	return held
}
