package project

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// issuedAt is a fixed instant, so a credential's whole life is stated rather
// than measured against a clock the test cannot hold still.
var issuedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// entropy is a reader that never runs short, which is what an issuance needs and
// what a deliberately short one is not.
func entropy(fill byte) *bytes.Reader {
	return bytes.NewReader(bytes.Repeat([]byte{fill}, clientSecretBytes*4))
}

func activeApplication(t *testing.T) ClientApplication {
	t.Helper()

	app, err := NewClientApplication("prj_a", ClientApplicationConfig{
		ID: "cli_loader", Name: "Nightly loader", State: ServiceActive, Kind: ClientConfidential,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	return app
}

func issue(t *testing.T, app ClientApplication, fill byte) (Credential, ClientSecret) {
	t.Helper()

	credential, secret, err := app.IssueCredential(CredentialConfig{
		ID: "cac_one", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(30 * 24 * time.Hour),
	}, entropy(fill))
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	return credential, secret
}

// TestAClientSecretIsOnlyEverMinted is what makes the single unsalted SHA-256
// sound: the type admits nothing but the bytes mintClientSecret drew, so there
// is no operator-chosen secret whose entropy the digest would have to survive.
func TestAClientSecretIsOnlyEverMinted(t *testing.T) {
	secret := reflect.TypeOf(ClientSecret{})

	if secret.Kind() != reflect.Struct {
		t.Fatalf("ClientSecret is a %s, so a caller can convert any string into one", secret.Kind())
	}

	for index := range secret.NumField() {
		if field := secret.Field(index); field.IsExported() {
			t.Errorf("ClientSecret.%s is exported, so a caller can choose the secret", field.Name)
		}
	}
}

// TestNoRenderingOfACredentialCarriesItsSecret is the leak test the redacting
// String alone does not pass: fmt cannot call a method on an unexported field,
// so every value that holds one has to redact itself.
func TestNoRenderingOfACredentialCarriesItsSecret(t *testing.T) {
	app := activeApplication(t)
	credential, secret := issue(t, app, 'k')

	plaintext := secret.Reveal()
	if plaintext == "" {
		t.Fatal("the minted secret is empty, so this test proves nothing")
	}

	digest := string(credential.StoredHash())
	if digest == "" {
		t.Fatal("the credential holds no digest, so this test proves nothing")
	}

	values := map[string]any{
		"ClientSecret": secret, "*ClientSecret": &secret,
		"CredentialHash":  credential.StoredHash(),
		"Credential":      credential,
		"*Credential":     &credential,
		"CredentialSlice": []Credential{credential},
	}

	for name, value := range values {
		for verb, rendered := range renderings(t, value) {
			if strings.Contains(rendered, plaintext) {
				t.Errorf("%s rendered by %s leaks its secret: %s", name, verb, rendered)
			}

			if strings.Contains(rendered, digest) {
				t.Errorf("%s rendered by %s leaks its hash: %s", name, verb, rendered)
			}
		}
	}
}

// TestASecretIsMintedWholeOrNotAtAll refuses a short read rather than returning
// the bytes it did get, which would be a secret with a fraction of the entropy
// its single hash is chosen for.
func TestASecretIsMintedWholeOrNotAtAll(t *testing.T) {
	short := bytes.NewReader(bytes.Repeat([]byte{'x'}, clientSecretBytes-1))

	secret, err := mintClientSecret(short)
	if err == nil {
		t.Fatal("a short read minted a secret")
	}

	if secret.isSet() {
		t.Error("a failed mint returned usable material")
	}
}

// TestASecretIsComparedInConstantTime reads its own source, because a timing
// difference is not observable from a unit test on a loaded machine: what can be
// checked is that the comparison is the one that does not leak a prefix.
func TestASecretIsComparedInConstantTime(t *testing.T) {
	source, err := os.ReadFile("credential.go")
	if err != nil {
		t.Fatalf("read credential.go: %v", err)
	}

	if !strings.Contains(string(source), "subtle.ConstantTimeCompare") {
		t.Error("Matches no longer compares in constant time, so a wrong guess can be narrowed byte by byte")
	}
}

// TestAnExpiredCredentialMatchesNothing keeps liveness inside the comparison, so
// no caller can check the secret and forget to check the clock.
func TestAnExpiredCredentialMatchesNothing(t *testing.T) {
	app := activeApplication(t)
	credential, secret := issue(t, app, 'k')

	if !credential.Matches(secret, issuedAt.Add(time.Hour)) {
		t.Fatal("a live credential refused its own secret")
	}

	if credential.Matches(secret, credential.ExpiresAt()) {
		t.Error("a credential matched at the instant it expired")
	}

	if credential.Matches(secret, credential.ExpiresAt().Add(time.Hour)) {
		t.Error("an expired credential matched its own secret")
	}
}

// TestACredentialMatchesNoOtherSecret, so the comparison is a comparison and not
// a check that some material is on file.
func TestACredentialMatchesNoOtherSecret(t *testing.T) {
	app := activeApplication(t)
	credential, _ := issue(t, app, 'k')

	other, err := mintClientSecret(entropy('z'))
	if err != nil {
		t.Fatalf("mintClientSecret: %v", err)
	}

	if credential.Matches(other, issuedAt.Add(time.Hour)) {
		t.Error("a credential matched a secret it was not issued for")
	}

	if credential.Matches(ClientSecret{}, issuedAt.Add(time.Hour)) {
		t.Error("a credential matched the zero secret")
	}
}

// TestRevocationDestroysTheSecretItStoodFor is the difference between a state
// something must remember to read and material that is simply gone.
func TestRevocationDestroysTheSecretItStoodFor(t *testing.T) {
	app := activeApplication(t)
	credential, secret := issue(t, app, 'k')

	revoked, err := credential.Revoke(issuedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if revoked.HasSecret() {
		t.Error("a revoked credential still holds material")
	}

	if revoked.Matches(secret, issuedAt.Add(2*time.Hour)) {
		t.Error("a revoked credential matched the secret it stood for")
	}

	if _, err := revoked.Revoke(issuedAt.Add(3 * time.Hour)); !errors.Is(err, ErrCredentialRevoked) {
		t.Errorf("revoking a spent credential: got %v, want ErrCredentialRevoked", err)
	}
}

// TestACredentialNeverOutlivesItsCeiling refuses the lifetime that makes "every
// secret has a death date" true and meaningless.
func TestACredentialNeverOutlivesItsCeiling(t *testing.T) {
	app := activeApplication(t)

	cases := map[string]time.Time{
		"one second past the ceiling": issuedAt.Add(maxCredentialLifetime + time.Second),
		"the year 3000":               time.Date(3000, time.January, 1, 0, 0, 0, 0, time.UTC),
		"before it was issued":        issuedAt.Add(-time.Hour),
		"the instant it was issued":   issuedAt,
	}

	for name, expires := range cases {
		_, _, err := app.IssueCredential(
			CredentialConfig{ID: "cac_one", CreatedAt: issuedAt, ExpiresAt: expires}, entropy('k'))
		if !errors.Is(err, ErrInvalidCredentialExpiry) {
			t.Errorf("%s: got %v, want ErrInvalidCredentialExpiry", name, err)
		}
	}

	if _, _, err := app.IssueCredential(CredentialConfig{
		ID: "cac_one", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(maxCredentialLifetime),
	}, entropy('k')); err != nil {
		t.Errorf("a credential at exactly the ceiling was refused: %v", err)
	}
}

// TestASuspendedRegistrationIssuesNoCredential, because a suspension that still
// mints keys suspends nothing.
func TestASuspendedRegistrationIssuesNoCredential(t *testing.T) {
	for _, state := range []ServiceState{ServiceSuspended, ServiceRevoked} {
		app, err := NewClientApplication("prj_a", ClientApplicationConfig{
			ID: "cli_loader", Name: "Nightly loader", State: state, Kind: ClientConfidential,
		})
		if err != nil {
			t.Fatalf("NewClientApplication(%s): %v", state, err)
		}

		_, secret, err := app.IssueCredential(CredentialConfig{
			ID: "cac_one", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour),
		}, entropy('k'))

		if !errors.Is(err, ErrServiceNotActive) {
			t.Errorf("%s issued a credential: %v", state, err)
		}

		if secret.isSet() {
			t.Errorf("%s was handed usable material", state)
		}
	}
}

// TestASupersededCredentialMatchesUntilItsWindowEnds is the whole point of the
// outgoing half: a rotation that stopped answering immediately would be an
// outage, and one that never stopped would be no rotation at all.
func TestASupersededCredentialMatchesUntilItsWindowEnds(t *testing.T) {
	app := activeApplication(t)
	credential, secret := issue(t, app, 'k')

	closes := issuedAt.Add(time.Hour)

	outgoing, err := credential.Supersede(closes)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	if outgoing.State() != CredentialSuperseded {
		t.Errorf("state is %s, want superseded", outgoing.State())
	}

	if !outgoing.Matches(secret, closes.Add(-time.Minute)) {
		t.Error("the outgoing secret stopped answering inside its own window")
	}

	if outgoing.Matches(secret, closes) {
		t.Error("the outgoing secret answered past its window")
	}
}

// TestASupersedeNeverExtendsTheSecretItRetires, so a rotation cannot be used to
// give an outgoing secret a longer life than it was issued with.
func TestASupersedeNeverExtendsTheSecretItRetires(t *testing.T) {
	app := activeApplication(t)
	credential, _ := issue(t, app, 'k')

	_, err := credential.Supersede(credential.ExpiresAt().Add(time.Hour))
	if !errors.Is(err, ErrInvalidCredentialExpiry) {
		t.Errorf("superseding past the original expiry: got %v, want ErrInvalidCredentialExpiry", err)
	}
}

// TestASupersededCredentialNeverReturnsToActive, because a rotation runs one way
// and a revoked credential is terminal.
func TestASupersededCredentialNeverReturnsToActive(t *testing.T) {
	every := []CredentialState{CredentialActive, CredentialSuperseded, CredentialRevoked}

	allowed := map[CredentialState][]CredentialState{
		CredentialActive:     {CredentialSuperseded, CredentialRevoked},
		CredentialSuperseded: {CredentialRevoked},
		CredentialRevoked:    nil,
	}

	for from, permitted := range allowed {
		for _, to := range every {
			want := slicesContains(permitted, to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s to %s: got %t, want %t", from, to, got, want)
			}
		}
	}
}

// slicesContains keeps the transition table readable without asking the test to
// depend on the ordering of the states it is checking.
func slicesContains(states []CredentialState, want CredentialState) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}

	return false
}

// TestNewCredentialRefusesEveryRowTheSchemaRefuses re-checks on the way out what
// the table's mirrored CHECKs refuse on the way in, so a row written around the
// constructor fails the read rather than reaching a caller.
func TestNewCredentialRefusesEveryRowTheSchemaRefuses(t *testing.T) {
	live := CredentialRecord{
		ID: "cac_one", Client: "cli_loader", Hash: "abcdef", State: CredentialActive,
		CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour),
	}

	cases := map[string]struct {
		mutate func(CredentialRecord) CredentialRecord
		want   error
	}{
		"live but holding no secret": {
			func(r CredentialRecord) CredentialRecord { r.Hash = ""; return r }, ErrInvalidSecretState,
		},
		"live but already revoked": {
			func(r CredentialRecord) CredentialRecord { r.RevokedAt = issuedAt; return r }, ErrInvalidSecretState,
		},
		"revoked but still holding a secret": {
			func(r CredentialRecord) CredentialRecord {
				r.State, r.RevokedAt = CredentialRevoked, issuedAt

				return r
			}, ErrInvalidSecretState,
		},
		"revoked at no instant": {
			func(r CredentialRecord) CredentialRecord {
				r.State, r.Hash = CredentialRevoked, ""

				return r
			}, ErrInvalidSecretState,
		},
		"a state outside the enum": {
			func(r CredentialRecord) CredentialRecord { r.State = "retired"; return r }, ErrUnknownState,
		},
		"an id outside its namespace": {
			func(r CredentialRecord) CredentialRecord { r.ID = "cred_one"; return r }, ErrInvalidServiceID,
		},
		"a client outside its namespace": {
			func(r CredentialRecord) CredentialRecord { r.Client = "loader"; return r }, ErrInvalidServiceID,
		},
		"a lifetime past the ceiling": {
			func(r CredentialRecord) CredentialRecord {
				r.ExpiresAt = issuedAt.Add(maxCredentialLifetime + time.Hour)

				return r
			}, ErrInvalidCredentialExpiry,
		},
	}

	if _, err := NewCredential("prj_a", live); err != nil {
		t.Fatalf("a well-formed record was refused: %v", err)
	}

	for name, testCase := range cases {
		if _, err := NewCredential("prj_a", testCase.mutate(live)); !errors.Is(err, testCase.want) {
			t.Errorf("%s: got %v, want %v", name, err, testCase.want)
		}
	}
}

// TestARevokedCredentialRebuildsThoughItsExpiryHasPassed, because a revoked row
// keeps the expiry it died with and would otherwise become unreadable with time.
func TestARevokedCredentialRebuildsThoughItsExpiryHasPassed(t *testing.T) {
	_, err := NewCredential("prj_a", CredentialRecord{
		ID: "cac_one", Client: "cli_loader", State: CredentialRevoked,
		CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour), RevokedAt: issuedAt.Add(time.Minute),
	})
	if err != nil {
		t.Errorf("a revoked credential failed to rebuild: %v", err)
	}
}
