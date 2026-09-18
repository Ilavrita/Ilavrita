package project_test

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

func mustKey(t *testing.T) project.SealingKey {
	t.Helper()

	encoded, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("mint a key: %v", err)
	}

	key, err := project.ParseSealingKey(encoded)
	if err != nil {
		t.Fatalf("parse a key: %v", err)
	}

	return key
}

// TestSealedMaterialSurvivesARoundTrip.
func TestSealedMaterialSurvivesARoundTrip(t *testing.T) {
	key := mustKey(t)

	sealed, err := key.Seal([]byte("the shared secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if strings.Contains(sealed, "the shared secret") {
		t.Error("the sealed material carries the plaintext")
	}

	opened, err := key.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if string(opened) != "the shared secret" {
		t.Errorf("opened %q", opened)
	}
}

// TestSealingTheSameSecretTwiceDiffers, so two identities enrolling the same
// secret are not visibly the same row.
func TestSealingTheSameSecretTwiceDiffers(t *testing.T) {
	key := mustKey(t)

	first, err := key.Seal([]byte("the shared secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	second, err := key.Seal([]byte("the shared secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if first == second {
		t.Error("sealing the same secret twice produced the same material")
	}
}

// TestAnotherKeyOpensNothing, which is the whole point: a database read on its
// own does not hand over anyone's second factor.
func TestAnotherKeyOpensNothing(t *testing.T) {
	sealed, err := mustKey(t).Seal([]byte("the shared secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := mustKey(t).Open(sealed); !errors.Is(err, project.ErrUnsealable) {
		t.Errorf("err = %v, want %v", err, project.ErrUnsealable)
	}
}

// TestTamperedMaterialIsRefusedRatherThanReadAsSomethingElse. GCM authenticates
// as it decrypts, so a changed row fails instead of opening to other bytes.
func TestTamperedMaterialIsRefusedRatherThanReadAsSomethingElse(t *testing.T) {
	key := mustKey(t)

	sealed, err := key.Seal([]byte("the shared secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	changed := []byte(sealed)
	changed[len(changed)-2] ^= 'A' ^ 'B'

	if _, err := key.Open(string(changed)); !errors.Is(err, project.ErrUnsealable) {
		t.Errorf("tampered material opened: %v", err)
	}

	for name, broken := range map[string]string{
		"not base64": "not base64 at all!",
		"empty":      "",
		"too short":  "AAAA",
	} {
		if _, err := key.Open(broken); !errors.Is(err, project.ErrUnsealable) {
			t.Errorf("%s: err = %v, want %v", name, err, project.ErrUnsealable)
		}
	}
}

// TestAKeyThisServerChoseForItselfIsRefused. A default key is one an attacker
// can choose too, so a deployment that configured none seals nothing.
func TestAKeyThisServerChoseForItselfIsRefused(t *testing.T) {
	if _, err := project.ParseSealingKey(""); !errors.Is(err, project.ErrMissingSealingKey) {
		t.Errorf("err = %v, want %v", err, project.ErrMissingSealingKey)
	}

	var unconfigured project.SealingKey

	if !unconfigured.IsZero() {
		t.Error("the zero key does not report itself as one")
	}

	if _, err := unconfigured.Seal([]byte("x")); !errors.Is(err, project.ErrMissingSealingKey) {
		t.Error("an unconfigured key sealed something")
	}

	if _, err := unconfigured.Open("x"); !errors.Is(err, project.ErrMissingSealingKey) {
		t.Error("an unconfigured key opened something")
	}
}

// TestAKeyOfTheWrongLengthIsRefused, so a short or mistyped one is not read as
// a weaker cipher.
func TestAKeyOfTheWrongLengthIsRefused(t *testing.T) {
	for name, encoded := range map[string]string{
		"too short":  "c2hvcnQ=",
		"not base64": "not base64 at all!",
	} {
		if _, err := project.ParseSealingKey(encoded); !errors.Is(err, project.ErrMalformedSealingKey) {
			t.Errorf("%s: err = %v, want %v", name, err, project.ErrMalformedSealingKey)
		}
	}
}
