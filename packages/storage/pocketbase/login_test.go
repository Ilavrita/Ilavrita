package pocketbase

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// thePassword is what every case below logs in with.
const thePassword = "correct horse battery staple"

func derive(t *testing.T, password string) project.PasswordHash {
	t.Helper()

	hash, err := project.HashPassword(password, rand.Reader)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	return hash
}

// credentialled invites an identity and accepts the invitation, which is the only
// path to an identity that can log in.
func credentialled(t *testing.T, store *UserStore, id project.UserID, raw string) project.Email {
	t.Helper()

	user := invitedProjectUser(t, "prj_a", id, raw)

	version, err := store.Create(t.Context(), user)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}

	if _, err := store.AcceptInvitation(t.Context(), id, derive(t, thePassword), version); err != nil {
		t.Fatalf("accept the invitation for %s: %v", id, err)
	}

	return user.Email()
}

// TestAnInvitedIdentityCannotLogInUntilItAcceptsItsInvitation. Nothing turned a
// request into a principal before this, so the acceptance is the whole of what
// makes an identity usable.
func TestAnInvitedIdentityCannotLogInUntilItAcceptsItsInvitation(t *testing.T) {
	store, _ := newUserStore(t)

	user := invitedProjectUser(t, "prj_a", "usr_nurse", "nurse@example.test")

	version, err := store.Create(t.Context(), user)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, found, err := store.Authenticate(
		t.Context(), "prj_a", user.Email(), thePassword); err != nil || found {
		t.Fatalf("an invitation authenticated: found %t, err %v", found, err)
	}

	if _, err := store.AcceptInvitation(t.Context(), "usr_nurse", derive(t, thePassword), version); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}

	authenticated, found, err := store.Authenticate(t.Context(), "prj_a", user.Email(), thePassword)
	if err != nil || !found {
		t.Fatalf("login after acceptance: found %t, err %v", found, err)
	}

	if authenticated.ID() != "usr_nurse" || authenticated.State() != project.UserActive {
		t.Errorf("logged in as %v", authenticated)
	}
}

// TestAWrongPasswordAndAnUnknownAddressAreTheSameAnswer, because telling them
// apart tells an attacker which addresses exist.
func TestAWrongPasswordAndAnUnknownAddressAreTheSameAnswer(t *testing.T) {
	store, _ := newUserStore(t)
	known := credentialled(t, store, "usr_nurse", "nurse@example.test")

	wrong, found, err := store.Authenticate(t.Context(), "prj_a", known, "not the password")
	if err != nil || found {
		t.Errorf("a wrong password: found %t, err %v", found, err)
	}

	stranger, found, err := store.Authenticate(
		t.Context(), "prj_a", address(t, "stranger@example.test"), thePassword)
	if err != nil || found {
		t.Errorf("an unknown address: found %t, err %v", found, err)
	}

	if wrong.ID() != "" || stranger.ID() != "" {
		t.Error("a refused login returned an identity")
	}
}

// TestALoginNeverCrossesARealm. A project-scoped identity answers only in its own
// Project, which is what keeps one Project's users out of another's login.
func TestALoginNeverCrossesARealm(t *testing.T) {
	store, _ := newUserStore(t)
	known := credentialled(t, store, "usr_nurse", "nurse@example.test")

	for _, realm := range []project.IdentityRealm{"prj_b", project.SystemRealm} {
		if _, found, err := store.Authenticate(t.Context(), realm, known, thePassword); err != nil || found {
			t.Errorf("a login in %s reached another realm: found %t, err %v", realm, found, err)
		}
	}
}

// TestADisabledIdentityCannotLogIn. Disabling is the lever an operator pulls, and
// it takes effect on the very next login rather than when a session expires.
func TestADisabledIdentityCannotLogIn(t *testing.T) {
	store, _ := newUserStore(t)
	known := credentialled(t, store, "usr_nurse", "nurse@example.test")

	_, version, found, err := store.ByID(t.Context(), "usr_nurse")
	if err != nil || !found {
		t.Fatalf("read back: found %t, err %v", found, err)
	}

	if _, err := store.UpdateState(t.Context(), "usr_nurse", project.UserDisabled, version); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, found, err := store.Authenticate(t.Context(), "prj_a", known, thePassword); err != nil || found {
		t.Errorf("a disabled identity logged in: found %t, err %v", found, err)
	}
}

// TestAnAcceptedInvitationCannotBeReplayed. The statement requires the identity to
// still be invited and to hold no credential, so a replay is not a password reset.
func TestAnAcceptedInvitationCannotBeReplayed(t *testing.T) {
	store, _ := newUserStore(t)

	user := invitedProjectUser(t, "prj_a", "usr_nurse", "nurse@example.test")

	version, err := store.Create(t.Context(), user)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.AcceptInvitation(t.Context(), "usr_nurse", derive(t, thePassword), version); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}

	// A second acceptance at the original version matches nothing.
	if _, err := store.AcceptInvitation(
		t.Context(), "usr_nurse", derive(t, "a different password"), version); err == nil {
		t.Fatal("an accepted invitation was replayed")
	}

	// The original password still works, so nothing was overwritten.
	if _, found, err := store.Authenticate(
		t.Context(), "prj_a", user.Email(), thePassword); err != nil || !found {
		t.Errorf("the original password stopped working: found %t, err %v", found, err)
	}
}

// TestAnIdentityCannotBeCredentialledWithARebuiltHash. A User a read rebuilt
// carries the sentinel, and writing it back would lock the identity out forever.
func TestAnIdentityCannotBeCredentialledWithARebuiltHash(t *testing.T) {
	store, _ := newUserStore(t)

	user := invitedProjectUser(t, "prj_a", "usr_nurse", "nurse@example.test")

	version, err := store.Create(t.Context(), user)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, hash := range []project.PasswordHash{"", credentialOnFile} {
		if _, err := store.AcceptInvitation(t.Context(), "usr_nurse", hash, version); !errors.Is(
			err, ErrCredentialNotMinted) {
			t.Errorf("accepting with %q: got %v, want ErrCredentialNotMinted", hash, err)
		}
	}
}

// TestAnAuthenticatedIdentityCarriesNoCredential. Authenticate reads the stored
// hash, and the identity it returns must not carry it back out.
func TestAnAuthenticatedIdentityCarriesNoCredential(t *testing.T) {
	store, _ := newUserStore(t)
	known := credentialled(t, store, "usr_nurse", "nurse@example.test")

	authenticated, found, err := store.Authenticate(t.Context(), "prj_a", known, thePassword)
	if err != nil || !found {
		t.Fatalf("login: found %t, err %v", found, err)
	}

	if !authenticated.HasCredential() {
		t.Error("an authenticated identity reports no credential on file")
	}

	rendered := map[string]string{
		"%v":  fmt.Sprintf("%v", authenticated),
		"%+v": fmt.Sprintf("%+v", authenticated),
		"%#v": fmt.Sprintf("%#v", authenticated),
	}

	for verb, text := range rendered {
		if strings.Contains(text, "argon2id") {
			t.Errorf("%s renders the stored hash: %s", verb, text)
		}
	}
}

// TestOnlyOneStatementInThisPackageReadsAStoredPasswordHash. It is the line
// between an identity registry and an authenticator, and it should stay one line.
func TestOnlyOneStatementInThisPackageReadsAStoredPasswordHash(t *testing.T) {
	var reads int

	for _, fragment := range strings.Split(sourceOf(t, "user.go"), `"`) {
		if !strings.Contains(strings.ToUpper(fragment), "SELECT") {
			continue
		}

		if strings.Contains(fragment, "password_hash") &&
			!strings.Contains(fragment, "COALESCE(password_hash, '') <>") {
			reads++
		}
	}

	if reads != 1 {
		t.Errorf("%d statements read a stored password hash, want exactly 1", reads)
	}
}
