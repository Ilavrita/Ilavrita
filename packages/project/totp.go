package project

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 names SHA-1, and authenticator apps implement that.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrMalformedSecret reports a second-factor secret this server cannot read.
	ErrMalformedSecret = errors.New("project: the second-factor secret cannot be read")

	// ErrMalformedCode reports something that is not a six-digit code.
	ErrMalformedCode = errors.New("project: a second-factor code is six digits")

	// ErrCodeRefused reports a code that does not match, has expired, or was
	// already used. The three are one answer: which of them it was is not
	// something a caller may learn by asking.
	ErrCodeRefused = errors.New("project: the second-factor code was refused")
)

// The shape of the codes this server accepts. Six digits over a thirty-second
// step is what every authenticator app produces by default, and interoperating
// with the apps people already have is worth more here than a longer code.
const (
	totpDigits = 6
	totpStep   = 30 * time.Second

	// totpSkew is how many steps either side of now are accepted, for the
	// clock drift between a phone and a server. One step is thirty seconds
	// each way; more would widen the window an intercepted code is usable in.
	totpSkew = 1

	// totpSecretBytes is the shared secret's length. RFC 4226 requires at least
	// 128 bits and recommends 160, which is what this is.
	totpSecretBytes = 20
)

// TOTPSecret is one identity's second-factor secret.
//
// Its field is unexported and it has no String method, so it cannot be logged
// or interpolated by accident. Unlike a password, this secret has to be kept
// rather than hashed: the server computes the same code the phone does, so
// whatever holds it holds the factor.
type TOTPSecret struct {
	raw []byte
}

// MintTOTPSecret draws a new shared secret.
func MintTOTPSecret(random io.Reader) (TOTPSecret, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return TOTPSecret{}, fmt.Errorf("project: cannot mint a second-factor secret: %w", err)
	}

	return TOTPSecret{raw: raw}, nil
}

// ParseTOTPSecret reads a secret back from its base32 encoding, which is how
// authenticator apps and this server's own storage both spell it.
func ParseTOTPSecret(encoded string) (TOTPSecret, error) {
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.ReplaceAll(encoded, " ", "")))
	if err != nil {
		return TOTPSecret{}, fmt.Errorf("%w: %w", ErrMalformedSecret, err)
	}

	if len(raw) < totpSecretBytes {
		return TOTPSecret{}, fmt.Errorf("%w: %d bytes, want at least %d",
			ErrMalformedSecret, len(raw), totpSecretBytes)
	}

	return TOTPSecret{raw: raw}, nil
}

// Encoded renders the secret the way an authenticator app reads it. It is the
// one way the material leaves this type, and it exists for exactly two moments:
// showing an enrolling person their secret, and writing it down.
func (s TOTPSecret) Encoded() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(s.raw)
}

// IsZero reports whether this is a secret nobody minted.
func (s TOTPSecret) IsZero() bool { return len(s.raw) == 0 }

// ProvisioningURI is the otpauth:// URL an authenticator app scans. The issuer
// appears twice because apps read it from either place and disagree about
// which.
func (s TOTPSecret) ProvisioningURI(issuer, account string) string {
	query := url.Values{}
	query.Set("secret", s.Encoded())
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", fmt.Sprint(totpDigits))
	query.Set("period", fmt.Sprint(int(totpStep.Seconds())))

	label := url.PathEscape(issuer + ":" + account)

	return "otpauth://totp/" + label + "?" + query.Encode()
}

// Step is the counter a code belongs to. It is stored after a successful
// verification so the same code cannot be presented twice: a code watched over
// someone's shoulder is good for thirty seconds otherwise.
func Step(at time.Time) int64 {
	return at.Unix() / int64(totpStep.Seconds())
}

// Code computes the code for one step. It is exported so a test can prove this
// implementation against RFC 6238's own vectors rather than against itself.
func (s TOTPSecret) Code(step int64) string {
	counter := make([]byte, 8)
	binary.BigEndian.PutUint64(counter, uint64(step)) //nolint:gosec // a step is never negative.

	mac := hmac.New(sha1.New, s.raw)
	mac.Write(counter)
	digest := mac.Sum(nil)

	// RFC 4226 dynamic truncation: the low nibble of the last byte picks where
	// to read four bytes from, and the top bit is cleared so the result is
	// positive on every implementation.
	offset := digest[len(digest)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff

	return fmt.Sprintf("%0*d", totpDigits, truncated%pow10(totpDigits))
}

// pow10 is ten to the n, for the digits a code is truncated to.
func pow10(digits int) uint32 {
	held := uint32(1)
	for range digits {
		held *= 10
	}

	return held
}

// Verify checks a presented code and reports the step it belonged to.
//
// A code is refused when it does not match, when it belongs to a step already
// used, or when it is malformed — one answer for all three, because which it
// was is not something a caller may learn by asking.
func (s TOTPSecret) Verify(presented string, at time.Time, lastUsed int64) (int64, error) {
	if s.IsZero() {
		return 0, ErrCodeRefused
	}

	if err := validateCode(presented); err != nil {
		return 0, ErrCodeRefused
	}

	now := Step(at)

	// Every candidate is checked even after one matches, so the time this takes
	// does not say which step a code belonged to.
	matched, found := int64(0), false

	for step := now - totpSkew; step <= now+totpSkew; step++ {
		if subtle.ConstantTimeCompare([]byte(s.Code(step)), []byte(presented)) == 1 {
			matched, found = step, true
		}
	}

	// A step at or before the last one used is a replay. A code watched over
	// someone's shoulder is good for thirty seconds without this.
	if !found || matched <= lastUsed {
		return 0, ErrCodeRefused
	}

	return matched, nil
}

// validateCode refuses anything that is not six digits, before it is compared.
func validateCode(presented string) error {
	if len(presented) != totpDigits {
		return ErrMalformedCode
	}

	for _, digit := range presented {
		if digit < '0' || digit > '9' {
			return ErrMalformedCode
		}
	}

	return nil
}
