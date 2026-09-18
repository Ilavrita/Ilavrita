package project_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

func aKeyer(t *testing.T) project.AttemptKeys {
	t.Helper()

	return project.NewAttemptKeys(mustKey(t))
}

// TestAnAttemptNameCannotBeGuessedBackFromTheTable. An email address and an
// IPv4 address are both drawn from a space small enough to walk, so an unkeyed
// digest of one hides nothing from whoever holds the table. This is the whole
// claim the table makes about itself.
func TestAnAttemptNameCannotBeGuessedBackFromTheTable(t *testing.T) {
	names := aKeyer(t)

	const (
		slug    = "clinic-a"
		address = "nurse@example.test"
		host    = "10.0.0.1"
	)

	// What somebody holding the table would compute, having guessed right.
	unkeyed := func(parts ...string) project.AttemptKey {
		digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))

		return project.AttemptKey(hex.EncodeToString(digest[:]))
	}

	held := map[string]project.AttemptKey{
		"identity": names.Identity(slug, address),
		"address":  names.Address(host),
	}

	for kind, name := range held {
		for _, guess := range []project.AttemptKey{
			unkeyed(kind, slug, address), unkeyed(kind, host),
			unkeyed(slug, address), unkeyed(host), unkeyed(address),
		} {
			if name == guess {
				t.Errorf("the %s name is an unkeyed digest of what it names", kind)
			}
		}
	}
}

// TestTwoInstallsNameTheSameFailureDifferently, so one install's table says
// nothing about another's — and a key rotated after a leak costs at most the
// counts inside one fifteen-minute window.
func TestTwoInstallsNameTheSameFailureDifferently(t *testing.T) {
	first, second := aKeyer(t), aKeyer(t)

	if first.Identity("clinic-a", "nurse@example.test") ==
		second.Identity("clinic-a", "nurse@example.test") {
		t.Error("two deployments name the same identity the same way")
	}

	if first.Address("10.0.0.1") == second.Address("10.0.0.1") {
		t.Error("two deployments name the same host the same way")
	}
}

// TestAKeyerWithNoKeySaysSo. A deployment that configured no secret still
// throttles, and the table it writes hides nothing — which is worth reporting
// rather than leaving to be discovered.
func TestAKeyerWithNoKeySaysSo(t *testing.T) {
	if project.NewAttemptKeys(project.SealingKey{}).Keyed() {
		t.Error("a keyer built from no secret reports that it is keyed")
	}

	if !aKeyer(t).Keyed() {
		t.Error("a keyer built from a real secret reports that it is not")
	}
}

// TestAnAttemptNameIsStableAndDistinct. The throttle only ever asks whether two
// attempts are the same one, so folding has to hold and nothing else may
// collide: an identity with a host, one Project's user with another's, or two
// sets of parts spelled into one key.
func TestAnAttemptNameIsStableAndDistinct(t *testing.T) {
	names := aKeyer(t)

	same := names.Identity("clinic-a", "nurse@example.test")

	for _, spelling := range []string{"NURSE@example.test", " nurse@example.test ", "Nurse@Example.Test"} {
		if names.Identity("clinic-a", spelling) != same {
			t.Errorf("%q names a different identity", spelling)
		}
	}

	distinct := map[project.AttemptKey]string{}

	for described, name := range map[string]project.AttemptKey{
		"the identity":             same,
		"another Project's":        names.Identity("clinic-b", "nurse@example.test"),
		"a host":                   names.Address("10.0.0.1"),
		"another host":             names.Address("10.0.0.2"),
		"a host named like a user": names.Address("clinic-a\x00nurse@example.test"),
	} {
		if already, seen := distinct[name]; seen {
			t.Errorf("%s and %s are one key", described, already)
		}

		distinct[name] = described
	}
}
