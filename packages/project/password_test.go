package project

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

// TestAPasswordVerifiesAgainstItsOwnHashAndNothingElse is the whole of what a
// login rests on.
func TestAPasswordVerifiesAgainstItsOwnHashAndNothingElse(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !hash.Matches("correct horse battery staple") {
		t.Error("a password did not verify against its own hash")
	}

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple"} {
		if hash.Matches(wrong) {
			t.Errorf("%q verified against another password's hash", wrong)
		}
	}
}

// TestTwoIdenticalPasswordsHashDifferently, because the salt is drawn per
// password: without it one stolen hash would answer for every account sharing it.
func TestTwoIdenticalPasswordsHashDifferently(t *testing.T) {
	first, err := HashPassword("same password", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	second, err := HashPassword("same password", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if first == second {
		t.Fatal("two hashes of one password are identical, so the salt is fixed")
	}

	// Both still verify, which is what makes the salt free.
	if !first.Matches("same password") || !second.Matches("same password") {
		t.Error("a salted hash stopped verifying")
	}
}

// TestAStoredHashCarriesTheCostItWasProducedAt, so raising the cost later does
// not make every existing password unreadable.
func TestAStoredHashCarriesTheCostItWasProducedAt(t *testing.T) {
	hash, err := HashPassword("password", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(string(hash), "argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("the encoded hash reads %q", hash)
	}

	if hash.NeedsRehash() {
		t.Error("a hash produced at the current cost claims it needs rehashing")
	}
}

// TestAHashProducedAtALowerCostNeedsRehashing, so a correct password can be
// upgraded on the way past rather than everyone being forced to reset.
func TestAHashProducedAtALowerCostNeedsRehashing(t *testing.T) {
	hash, err := HashPassword("password", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	weakened := PasswordHash(strings.Replace(string(hash), "m=65536,t=3,p=4", "m=1024,t=1,p=1", 1))
	if !weakened.NeedsRehash() {
		t.Error("a hash produced at a lower cost does not ask to be rehashed")
	}
}

// TestAnUnreadableHashMatchesNothing. A hash this server cannot read denies
// rather than falling back to a weaker comparison.
func TestAnUnreadableHashMatchesNothing(t *testing.T) {
	unreadable := []PasswordHash{
		"", "not a hash", "argon2id$v=19$m=65536,t=3,p=4$onlyfourfields",
		"bcrypt$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"argon2id$v=13$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
		"argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",
	}

	for _, hash := range unreadable {
		if hash.Matches("password") {
			t.Errorf("%q matched a password", hash)
		}

		if !hash.NeedsRehash() {
			t.Errorf("%q does not ask to be rehashed", hash)
		}
	}
}

// TestAHashOfTheWrongKeyLengthIsRefused. A key of another length is one this
// server did not produce, so it is refused rather than compared at whatever
// length it happens to carry.
func TestAHashOfTheWrongKeyLengthIsRefused(t *testing.T) {
	truncated := PasswordHash("argon2id$v=19$m=65536,t=3,p=4$c2FsdHlzYWx0eXNhbHQ$c2hvcnQ")

	if truncated.Matches("password") {
		t.Error("a hash carrying a short key matched a password")
	}
}

// TestAHashIsDerivedWholeOrNotAtAll refuses a short read rather than falling back
// to a predictable salt.
func TestAHashIsDerivedWholeOrNotAtAll(t *testing.T) {
	short := strings.NewReader("too short")

	if hash, err := HashPassword("password", short); err == nil || hash != "" {
		t.Errorf("a short read produced %q with error %v", hash, err)
	}

	if _, err := HashPassword("", rand.Reader); !errors.Is(err, ErrInvalidCredentialState) {
		t.Errorf("hashing an empty password: got %v, want ErrInvalidCredentialState", err)
	}
}

// TestNoRenderingOfAHashCarriesIt. PasswordHash redacts, and a derived hash is
// the one value a log line must never carry.
func TestNoRenderingOfAHashCarriesIt(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	for verb, rendered := range renderings(t, hash) {
		if strings.Contains(rendered, string(hash)) {
			t.Errorf("%s leaks the hash: %s", verb, rendered)
		}
	}
}
