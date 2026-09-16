package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func storedHash() PasswordHash {
	return PasswordHash("argon2id$v=19$m=65536,t=3,p=4$c2FsdHk$aGFzaGVk")
}

func mustEmail(t *testing.T, raw string) Email {
	t.Helper()

	email, err := NormaliseEmail(raw)
	if err != nil {
		t.Fatalf("NormaliseEmail(%q): %v", raw, err)
	}

	return email
}

func activeConfig(t *testing.T, id UserID, raw string) UserConfig {
	t.Helper()

	return UserConfig{
		ID: id, Email: mustEmail(t, raw),
		PasswordHash: storedHash(), State: UserActive,
	}
}

func mustServerUser(t *testing.T, cfg UserConfig) User {
	t.Helper()

	user, err := NewServerUser(cfg)
	if err != nil {
		t.Fatalf("NewServerUser: %v", err)
	}

	return user
}

func mustProjectUser(t *testing.T, home ID, cfg UserConfig) User {
	t.Helper()

	user, err := NewProjectUser(home, cfg)
	if err != nil {
		t.Fatalf("NewProjectUser: %v", err)
	}

	return user
}

func TestNormaliseEmailWorkedCases(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		normalized string
		display    string
	}{
		{
			name: "case folds and surrounding whitespace goes",
			raw:  "  Alice@Example.COM  ", normalized: "alice@example.com", display: "Alice@Example.COM",
		},
		{
			name: "plus tag survives",
			raw:  "alice+billing@corp.example", normalized: "alice+billing@corp.example", display: "alice+billing@corp.example",
		},
		{
			name: "interior dot survives",
			raw:  "alice.smith@corp.example", normalized: "alice.smith@corp.example", display: "alice.smith@corp.example",
		},
		{
			// The spec's worked table lowercases Display on this row alone; its
			// prose and its first row both say Display is never case-folded, so
			// the case is preserved here and only the root dot is dropped.
			name: "trailing dns dot goes from both forms",
			raw:  "alice@Example.com.", normalized: "alice@example.com", display: "alice@Example.com",
		},
		{
			name: "unicode domain encodes, display stays readable",
			raw:  "alice@café.example", normalized: "alice@xn--caf-dma.example", display: "alice@café.example",
		},
		{
			name: "punycode domain is already its own normal form",
			raw:  "alice@xn--caf-dma.example", normalized: "alice@xn--caf-dma.example", display: "alice@xn--caf-dma.example",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			email, err := NormaliseEmail(tc.raw)
			if err != nil {
				t.Fatalf("NormaliseEmail(%q) = %v", tc.raw, err)
			}

			if email.Normalized() != tc.normalized {
				t.Fatalf("Normalized = %q, want %q", email.Normalized(), tc.normalized)
			}

			if email.Display() != tc.display {
				t.Fatalf("Display = %q, want %q", email.Display(), tc.display)
			}
		})
	}
}

func TestNormaliseEmailRejectsWhatItCannotBeSureOf(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "no at sign", raw: "alice.corp.example"},
		{name: "two at signs", raw: "alice@corp@example"},
		{name: "empty local part", raw: "@corp.example"},
		{name: "empty domain", raw: "alice@"},
		{name: "trailing dot in local part", raw: "alice.@corp.example"},
		{name: "leading dot in local part", raw: ".alice@corp.example"},
		{name: "consecutive dots in local part", raw: "alice..smith@corp.example"},
		{name: "quoted local part", raw: `"john smith"@corp.example`},
		{name: "full width local part", raw: "Ａlice@corp.example"},
		{name: "ligature in local part", raw: "ﬁnance@corp.example"},
		{name: "interior space", raw: "ali ce@corp.example"},
		{name: "control character", raw: "alice\x00@corp.example"},
		{name: "non breaking space", raw: "alice\u00a0smith@corp.example"},
		{name: "bare tld", raw: "alice@localhost"},
		{name: "empty domain label", raw: "alice@corp..example"},
		{name: "leading domain dot", raw: "alice@.corp.example"},
		{name: "underscore in domain", raw: "alice@corp_net.example"},
		{name: "leading hyphen in domain label", raw: "alice@-corp.example"},
		{name: "local part over 64 octets", raw: strings.Repeat("a", 65) + "@corp.example"},
		{name: "domain label over 63 octets", raw: "alice@" + strings.Repeat("a", 64) + ".example"},
		{
			name: "domain over 255 octets",
			raw:  "alice@" + strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example",
		},
		{
			name: "address over 254 octets",
			raw: strings.Repeat("a", 64) + "@" + strings.Repeat("b", 63) + "." +
				strings.Repeat("c", 63) + "." + strings.Repeat("d", 62),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			email, err := NormaliseEmail(tc.raw)
			if !errors.Is(err, ErrInvalidEmail) {
				t.Fatalf("NormaliseEmail(%q) = %v, want ErrInvalidEmail", tc.raw, err)
			}

			if !email.IsZero() {
				t.Fatalf("NormaliseEmail(%q) returned %q with an error", tc.raw, email.Normalized())
			}
		})
	}
}

func TestNormaliseEmailConfusables(t *testing.T) {
	tests := []struct {
		name     string
		left     string
		right    string
		collapse bool
	}{
		{name: "case folds", left: "Alice@Example.com", right: "alice@example.com", collapse: true},
		{name: "surrounding whitespace is hygiene", left: "\t alice@corp.example\n", right: "alice@corp.example", collapse: true},
		{name: "trailing domain dot is the dns root", left: "alice@corp.example.", right: "alice@corp.example", collapse: true},
		{name: "unicode and punycode name one domain", left: "alice@café.example", right: "alice@xn--caf-dma.example", collapse: true},
		{name: "plus tag is never stripped", left: "alice+billing@corp.example", right: "alice@corp.example", collapse: false},
		{name: "interior dots are never folded", left: "alice.smith@corp.example", right: "alicesmith@corp.example", collapse: false},
		{name: "cyrillic homoglyph is a different domain", left: "alice@apple.com", right: "alice@аpple.com", collapse: false},
		{name: "the domain is the only field idna maps", left: "alice@ＣORP.example", right: "alice@corp.example", collapse: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left, right := mustEmail(t, tc.left), mustEmail(t, tc.right)
			if (left.Normalized() == right.Normalized()) != tc.collapse {
				t.Fatalf("%q and %q normalise to %q and %q, want collapse=%v",
					tc.left, tc.right, left.Normalized(), right.Normalized(), tc.collapse)
			}
		})
	}
}

func TestNormaliseEmailNeverReturnsAHalfResult(t *testing.T) {
	inputs := []string{
		"alice@corp.example", "  Alice@Example.COM  ", "alice@café.example",
		"", "alice@localhost", "alice..smith@corp.example", `"john smith"@corp.example`,
	}

	for _, raw := range inputs {
		email, err := NormaliseEmail(raw)
		if err != nil && !email.IsZero() {
			t.Fatalf("NormaliseEmail(%q) returned both %q and %v", raw, email.Normalized(), err)
		}

		if err == nil && email.IsZero() {
			t.Fatalf("NormaliseEmail(%q) accepted an empty normalised form", raw)
		}
	}
}

func TestUserStateTransitions(t *testing.T) {
	tests := []struct {
		name  string
		from  UserState
		to    UserState
		allow bool
	}{
		{name: "invited activates", from: UserInvited, to: UserActive, allow: true},
		{name: "invited is revocable before it is accepted", from: UserInvited, to: UserDisabled, allow: true},
		{name: "invited does not transition to itself", from: UserInvited, to: UserInvited, allow: false},
		{name: "active disables", from: UserActive, to: UserDisabled, allow: true},
		{name: "active never returns to invited", from: UserActive, to: UserInvited, allow: false},
		{name: "active does not transition to itself", from: UserActive, to: UserActive, allow: false},
		{name: "disabled reactivates", from: UserDisabled, to: UserActive, allow: true},
		{name: "disabled never returns to invited", from: UserDisabled, to: UserInvited, allow: false},
		{name: "disabled does not transition to itself", from: UserDisabled, to: UserDisabled, allow: false},
		{name: "no unknown target", from: UserActive, to: "elevated", allow: false},
		{name: "unknown source fails closed", from: "elevated", to: UserActive, allow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.from.CanTransitionTo(tc.to) != tc.allow {
				t.Fatalf("CanTransitionTo(%s to %s) = %v, want %v", tc.from, tc.to, !tc.allow, tc.allow)
			}

			_, err := tc.from.TransitionTo(tc.to)
			if tc.allow != (err == nil) {
				t.Fatalf("TransitionTo(%s to %s) = %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestUserStateValidRejectsEverythingOutsideTheEnum(t *testing.T) {
	for _, state := range []UserState{UserInvited, UserActive, UserDisabled} {
		if !state.Valid() {
			t.Fatalf("%s is not valid", state)
		}
	}

	for _, state := range []UserState{"", "deleted", "Active", "super"} {
		if state.Valid() {
			t.Fatalf("%q is valid", state)
		}
	}
}

func TestServerUserOwnsNoProjectAndLivesInTheSystemRealm(t *testing.T) {
	user := mustServerUser(t, activeConfig(t, "usr_root", "root@corp.example"))

	if user.Scope() != ScopeServer {
		t.Fatalf("Scope = %s, want %s", user.Scope(), ScopeServer)
	}

	home, ok := user.HomeProject()
	if ok || home != "" {
		t.Fatalf("HomeProject = (%q, %v), want no Project", home, ok)
	}

	if user.IdentityRealm() != SystemRealm {
		t.Fatalf("IdentityRealm = %s, want %s", user.IdentityRealm(), SystemRealm)
	}
}

func TestProjectUserCarriesItsHomeProjectAsItsRealm(t *testing.T) {
	user := mustProjectUser(t, "prj_clinic", activeConfig(t, "usr_nurse", "nurse@clinic.example"))

	if user.Scope() != ScopeProject {
		t.Fatalf("Scope = %s, want %s", user.Scope(), ScopeProject)
	}

	home, ok := user.HomeProject()
	if !ok || home != "prj_clinic" {
		t.Fatalf("HomeProject = (%q, %v), want prj_clinic", home, ok)
	}

	if user.IdentityRealm() != IdentityRealm("prj_clinic") {
		t.Fatalf("IdentityRealm = %s, want prj_clinic", user.IdentityRealm())
	}
}

func TestProjectUserIsNotConstructibleWithoutAProject(t *testing.T) {
	tests := []struct {
		name string
		home ID
	}{
		{name: "no project", home: ""},
		{name: "wildcard project", home: "*"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProjectUser(tc.home, activeConfig(t, "usr_nurse", "nurse@clinic.example"))
			if !errors.Is(err, ErrInvalidProjectID) {
				t.Fatalf("NewProjectUser(%q) = %v, want ErrInvalidProjectID", tc.home, err)
			}
		})
	}
}

func TestIdentityRealmMatchesTheGeneratedColumn(t *testing.T) {
	tests := []struct {
		name string
		home ID
		want IdentityRealm
	}{
		{name: "null home project coalesces to system", home: "", want: SystemRealm},
		{name: "a home project is its own realm", home: "prj_clinic", want: "prj_clinic"},
		{name: "another project is another realm", home: "prj_hospital", want: "prj_hospital"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveRealm(tc.home); got != tc.want {
				t.Fatalf("DeriveRealm(%q) = %s, want %s", tc.home, got, tc.want)
			}
		})
	}
}

func TestOneAddressIsADifferentIdentityInEveryRealm(t *testing.T) {
	shared := "clinician@corp.example"
	clinic := mustProjectUser(t, "prj_clinic", activeConfig(t, "usr_a", shared))
	hospital := mustProjectUser(t, "prj_hospital", activeConfig(t, "usr_b", shared))
	server := mustServerUser(t, activeConfig(t, "usr_c", shared))

	if clinic.Email().Normalized() != hospital.Email().Normalized() {
		t.Fatal("the same address normalised differently in two Projects")
	}

	realms := map[IdentityRealm]bool{
		clinic.IdentityRealm(): true, hospital.IdentityRealm(): true, server.IdentityRealm(): true,
	}
	if len(realms) != 3 {
		t.Fatalf("three identities shared %d realms, want 3", len(realms))
	}
}

func TestNewUserRefusesEveryStateAndCredentialMismatch(t *testing.T) {
	tests := []struct {
		name  string
		state UserState
		hash  PasswordHash
		want  error
	}{
		{name: "invited holds no credential", state: UserInvited, hash: "", want: nil},
		{name: "active holds one", state: UserActive, hash: storedHash(), want: nil},
		{name: "disabled keeps its own", state: UserDisabled, hash: storedHash(), want: nil},
		{name: "invited with a credential", state: UserInvited, hash: storedHash(), want: ErrInvalidCredentialState},
		{name: "active without one", state: UserActive, hash: "", want: ErrInvalidCredentialState},
		{name: "disabled after a revoked invitation", state: UserDisabled, hash: "", want: nil},
		{name: "unknown state", state: "elevated", hash: storedHash(), want: ErrUnknownState},
		{name: "empty state", state: "", hash: "", want: ErrUnknownState},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := UserConfig{
				ID: "usr_1", Email: mustEmail(t, "alice@corp.example"),
				PasswordHash: tc.hash, State: tc.state,
			}

			user, err := NewServerUser(cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewServerUser = %v, want %v", err, tc.want)
			}

			if tc.want != nil && user.State() != "" {
				t.Fatalf("NewServerUser returned %s with an error", user.State())
			}
		})
	}
}

func TestNewUserRefusesAnIncompleteRecord(t *testing.T) {
	tests := []struct {
		name string
		cfg  UserConfig
		want error
	}{
		{
			name: "no id",
			cfg:  UserConfig{Email: mustEmail(t, "alice@corp.example"), State: UserInvited},
			want: ErrMissingID,
		},
		{
			name: "no email",
			cfg:  UserConfig{ID: "usr_1", State: UserInvited},
			want: ErrMissingEmail,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewServerUser(tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewServerUser = %v, want %v", err, tc.want)
			}

			if _, err := NewProjectUser("prj_clinic", tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewProjectUser = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAcceptInvitationIsTheOnlyWayIntoActive(t *testing.T) {
	invited := mustProjectUser(t, "prj_clinic", UserConfig{
		ID: "usr_nurse", Email: mustEmail(t, "nurse@clinic.example"), State: UserInvited,
	})

	if _, err := invited.TransitionTo(UserActive); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionTo(active) = %v, want ErrInvalidTransition", err)
	}

	accepted, err := invited.AcceptInvitation(storedHash())
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}

	if accepted.State() != UserActive || !accepted.HasCredential() {
		t.Fatalf("accepted = (%s, credential %v), want active with a credential", accepted.State(), accepted.HasCredential())
	}

	if invited.State() != UserInvited || invited.HasCredential() {
		t.Fatal("AcceptInvitation mutated the identity it was called on")
	}
}

func TestAcceptInvitationRefusesEveryOtherShape(t *testing.T) {
	invited := mustServerUser(t, UserConfig{
		ID: "usr_root", Email: mustEmail(t, "root@corp.example"), State: UserInvited,
	})
	active := mustServerUser(t, activeConfig(t, "usr_root", "root@corp.example"))

	if _, err := invited.AcceptInvitation(""); !errors.Is(err, ErrInvalidCredentialState) {
		t.Fatalf("AcceptInvitation(empty) = %v, want ErrInvalidCredentialState", err)
	}

	if _, err := active.AcceptInvitation(storedHash()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("AcceptInvitation on an active identity = %v, want ErrInvalidTransition", err)
	}

	disabled, err := active.TransitionTo(UserDisabled)
	if err != nil {
		t.Fatalf("TransitionTo(disabled): %v", err)
	}

	if _, err := disabled.AcceptInvitation(storedHash()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("AcceptInvitation on a disabled identity = %v, want ErrInvalidTransition", err)
	}
}

func TestUserTransitionsKeepTheCredentialAndThePolicy(t *testing.T) {
	cfg := activeConfig(t, "usr_nurse", "nurse@clinic.example")
	cfg.MFARequired = true
	active := mustProjectUser(t, "prj_clinic", cfg)

	disabled, err := active.TransitionTo(UserDisabled)
	if err != nil {
		t.Fatalf("TransitionTo(disabled): %v", err)
	}

	reactivated, err := disabled.TransitionTo(UserActive)
	if err != nil {
		t.Fatalf("TransitionTo(active): %v", err)
	}

	for _, user := range []User{disabled, reactivated} {
		if !user.HasCredential() {
			t.Fatalf("%s lost its credential", user.State())
		}

		if !user.MFARequired() {
			t.Fatalf("%s lost its second-factor policy", user.State())
		}

		if home, _ := user.HomeProject(); home != "prj_clinic" || user.Scope() != ScopeProject {
			t.Fatalf("%s moved realm to (%s, %q)", user.State(), user.Scope(), home)
		}
	}
}

func TestInvitedIdentityIsRevocableWithoutEverHoldingACredential(t *testing.T) {
	invited := mustServerUser(t, UserConfig{
		ID: "usr_root", Email: mustEmail(t, "root@corp.example"), State: UserInvited,
	})

	disabled, err := invited.TransitionTo(UserDisabled)
	if err != nil {
		t.Fatalf("TransitionTo(disabled): %v", err)
	}

	if disabled.HasCredential() {
		t.Fatal("a revoked invitation grew a credential")
	}

	if _, err := disabled.TransitionTo(UserInvited); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionTo(invited) = %v, want ErrInvalidTransition", err)
	}
}

func TestUserScopeValid(t *testing.T) {
	for _, scope := range []UserScope{ScopeServer, ScopeProject} {
		if !scope.Valid() {
			t.Fatalf("%s is not valid", scope)
		}
	}

	for _, scope := range []UserScope{"", "system", "Project"} {
		if scope.Valid() {
			t.Fatalf("%q is valid", scope)
		}
	}
}

func TestPasswordHashRedactsItself(t *testing.T) {
	hash := storedHash()
	if strings.Contains(hash.String(), string(hash)) {
		t.Fatalf("String() = %q, want the hash redacted", hash.String())
	}

	if PasswordHash("").String() != "" {
		t.Fatalf("an unset credential formatted as %q", PasswordHash("").String())
	}
}

// renderings is every way a value reaches an operator: the fmt verbs, the two
// slog handlers, and encoding/json. Each is a path a credential must not take.
func renderings(t *testing.T, value any) map[string]string {
	t.Helper()

	var text, structured bytes.Buffer

	slog.New(slog.NewTextHandler(&text, nil)).Info("rendered", "value", value)
	slog.New(slog.NewJSONHandler(&structured, nil)).Info("rendered", "value", value)

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	return map[string]string{
		"%v": fmt.Sprintf("%v", value), "%+v": fmt.Sprintf("%+v", value),
		"%#v": fmt.Sprintf("%#v", value), "json": string(encoded),
		"slog text": text.String(), "slog json": structured.String(),
	}
}

// TestNoRenderingOfAnIdentityCarriesItsCredential is the leak test the redacting
// String alone does not pass: fmt cannot call a method on an unexported field,
// so User has to redact itself rather than rely on PasswordHash doing it.
func TestNoRenderingOfAnIdentityCarriesItsCredential(t *testing.T) {
	hash := storedHash()
	cfg := UserConfig{
		ID: "usr_1", Email: mustEmail(t, "alice@corp.example"),
		PasswordHash: hash, State: UserActive,
	}

	user, err := NewServerUser(cfg)
	if err != nil {
		t.Fatalf("NewServerUser: %v", err)
	}

	for name, value := range map[string]any{"User": user, "UserConfig": cfg, "PasswordHash": hash} {
		for verb, rendered := range renderings(t, value) {
			if strings.Contains(rendered, string(hash)) {
				t.Errorf("%s rendered by %s leaks its credential: %s", name, verb, rendered)
			}
		}
	}
}
