package project_test

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// rfcSecret is RFC 6238's own test key: the ASCII "12345678901234567890".
func rfcSecret(t *testing.T) project.TOTPSecret {
	t.Helper()

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))

	secret, err := project.ParseTOTPSecret(encoded)
	if err != nil {
		t.Fatalf("parse the RFC secret: %v", err)
	}

	return secret
}

// TestCodesMatchRFC6238. The vectors come from the specification rather than
// from this implementation, so this proves the algorithm rather than proving it
// agrees with itself.
//
// RFC 6238 Appendix B publishes eight-digit values; a six-digit code is the
// same truncation taken modulo a smaller power of ten, which is its last six
// digits.
func TestCodesMatchRFC6238(t *testing.T) {
	secret := rfcSecret(t)

	vectors := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}

	for seconds, want := range vectors {
		at := time.Unix(seconds, 0).UTC()

		if got := secret.Code(project.Step(at)); got != want {
			t.Errorf("at %d the code is %s, want %s", seconds, got, want)
		}
	}
}

// TestACodeIsAcceptedWithinTheSkewAndNotOutsideIt. A phone and a server
// disagree about the time by seconds, not by minutes, and every extra step is
// another thirty seconds an intercepted code stays usable.
func TestACodeIsAcceptedWithinTheSkewAndNotOutsideIt(t *testing.T) {
	secret := rfcSecret(t)
	at := time.Unix(1111111109, 0).UTC()
	now := project.Step(at)

	for _, offset := range []int64{-1, 0, 1} {
		code := secret.Code(now + offset)

		if _, err := secret.Verify(code, at, 0); err != nil {
			t.Errorf("a code %d steps away was refused: %v", offset, err)
		}
	}

	for _, offset := range []int64{-2, 2, 10, -10} {
		code := secret.Code(now + offset)

		if _, err := secret.Verify(code, at, 0); !errors.Is(err, project.ErrCodeRefused) {
			t.Errorf("a code %d steps away was accepted", offset)
		}
	}
}

// TestACodeCannotBeUsedTwice. A code watched over someone's shoulder is good
// for thirty seconds without this.
func TestACodeCannotBeUsedTwice(t *testing.T) {
	secret := rfcSecret(t)
	at := time.Unix(1111111109, 0).UTC()

	used, err := secret.Verify(secret.Code(project.Step(at)), at, 0)
	if err != nil {
		t.Fatalf("the first use was refused: %v", err)
	}

	if _, err := secret.Verify(secret.Code(project.Step(at)), at, used); !errors.Is(err, project.ErrCodeRefused) {
		t.Error("the same code was accepted twice")
	}

	// An older code is refused too, so a replay cannot walk backwards into the
	// skew window.
	older := secret.Code(project.Step(at) - 1)
	if _, err := secret.Verify(older, at, used); !errors.Is(err, project.ErrCodeRefused) {
		t.Error("a code from an earlier step was accepted after a later one")
	}

	// The next step is accepted, so the replay guard does not lock the factor.
	later := at.Add(30 * time.Second)
	if _, err := secret.Verify(secret.Code(project.Step(later)), later, used); err != nil {
		t.Errorf("the next step's code was refused: %v", err)
	}
}

// TestEveryWrongCodeIsRefusedTheSameWay. Which of the three reasons it was is
// not something a caller may learn by asking.
func TestEveryWrongCodeIsRefusedTheSameWay(t *testing.T) {
	secret := rfcSecret(t)
	at := time.Unix(1111111109, 0).UTC()

	for name, presented := range map[string]string{
		"a wrong code":    "000000",
		"an empty code":   "",
		"too few digits":  "12345",
		"too many digits": "1234567",
		"letters":         "abcdef",
		"a signed number": "-12345",
		"spaces":          "123 456",
	} {
		if _, err := secret.Verify(presented, at, 0); !errors.Is(err, project.ErrCodeRefused) {
			t.Errorf("%s: err = %v, want %v", name, err, project.ErrCodeRefused)
		}
	}
}

// TestASecretNobodyMintedVerifiesNothing, so an identity with no factor
// enrolled cannot be let in by presenting anything at all.
func TestASecretNobodyMintedVerifiesNothing(t *testing.T) {
	var zero project.TOTPSecret

	if !zero.IsZero() {
		t.Error("the zero secret does not report itself as one")
	}

	if _, err := zero.Verify("000000", time.Now(), 0); !errors.Is(err, project.ErrCodeRefused) {
		t.Error("the zero secret accepted a code")
	}
}

// TestASecretSurvivesItsEncoding, which is how it reaches an authenticator app
// and how it is written down.
func TestASecretSurvivesItsEncoding(t *testing.T) {
	secret, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	again, err := project.ParseTOTPSecret(secret.Encoded())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	at := time.Now()
	if secret.Code(project.Step(at)) != again.Code(project.Step(at)) {
		t.Error("a secret read back from its encoding computes different codes")
	}

	// Apps show the secret in spaced, lower-case groups; a person typing it back
	// should not have to reproduce the formatting.
	spaced := strings.ToLower(secret.Encoded()[:4] + " " + secret.Encoded()[4:])

	typed, err := project.ParseTOTPSecret(spaced)
	if err != nil {
		t.Fatalf("parse a typed secret: %v", err)
	}

	if typed.Code(project.Step(at)) != secret.Code(project.Step(at)) {
		t.Error("a secret typed back with spaces computes different codes")
	}
}

// TestASecretTooShortToBeOneIsRefused. RFC 4226 requires at least 128 bits, and
// a short secret is a factor that is not one.
func TestASecretTooShortToBeOneIsRefused(t *testing.T) {
	short := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("tooshort"))

	if _, err := project.ParseTOTPSecret(short); !errors.Is(err, project.ErrMalformedSecret) {
		t.Errorf("err = %v, want %v", err, project.ErrMalformedSecret)
	}

	if _, err := project.ParseTOTPSecret("not base32 at all!"); !errors.Is(err, project.ErrMalformedSecret) {
		t.Error("something that is not base32 was read as a secret")
	}
}

// TestTwoSecretsDiffer, so nothing mints the same factor twice.
func TestTwoSecretsDiffer(t *testing.T) {
	first, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	second, err := project.MintTOTPSecret(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if first.Encoded() == second.Encoded() {
		t.Error("two secrets were minted the same")
	}
}

// TestTheProvisioningURICarriesWhatAnAppNeeds.
func TestTheProvisioningURICarriesWhatAnAppNeeds(t *testing.T) {
	secret := rfcSecret(t)
	uri := secret.ProvisioningURI("Ilavrita", "nurse@example.test")

	for _, wanted := range []string{
		"otpauth://totp/", "secret=" + secret.Encoded(),
		"issuer=Ilavrita", "algorithm=SHA1", "digits=6", "period=30",
	} {
		if !strings.Contains(uri, wanted) {
			t.Errorf("the provisioning URI does not carry %q: %s", wanted, uri)
		}
	}
}
