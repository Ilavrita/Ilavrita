package project

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// verifier is one PKCE verifier, and challengeFor is the challenge a client
// derives from it.
const verifier = "a-verifier-of-exactly-the-length-rfc7636-wants"

func challengeFor(t *testing.T, held string) CodeChallenge {
	t.Helper()

	sum := sha256.Sum256([]byte(held))

	challenge, err := ParseCodeChallenge(base64.RawURLEncoding.EncodeToString(sum[:]), "S256")
	if err != nil {
		t.Fatalf("ParseCodeChallenge: %v", err)
	}

	return challenge
}

// approvedCode mints one code for an approval, with everything valid, so each
// test below changes exactly one thing.
func approvedCode(t *testing.T) (AuthorizationCode, AuthorizationCodeToken) {
	t.Helper()

	launch, err := NewLaunchContext("pat_7", "patient/Observation.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	code, token, err := IssueAuthorizationCode(
		"prj_a", "acd_1", "cli_app", "usr_1", "pm_1",
		"https://app.example.test/callback", challengeFor(t, verifier), launch,
		launchedAt, 30*time.Second, rand.Reader)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}

	return code, token
}

// TestACodeIsRedeemedOnlyByWhatItWasIssuedTo.
//
// Every binding is checked together, because there is no safe order in which to
// check them apart: a redemption that compared the code and forgot the client is
// a code exchanged by somebody it was not issued to, and one that forgot the
// verifier is PKCE switched off.
func TestACodeIsRedeemedOnlyByWhatItWasIssuedTo(t *testing.T) {
	code, token := approvedCode(t)
	now := launchedAt.Add(time.Second)

	if !code.Redeemable(token, "cli_app", "https://app.example.test/callback", verifier, now) {
		t.Fatal("the code it was issued for was refused, so nothing below proves anything")
	}

	_, otherToken := approvedCode(t)

	for name, held := range map[string]struct {
		token    AuthorizationCodeToken
		client   ClientApplicationID
		redirect string
		verifier string
		now      time.Time
	}{
		"another client": {
			token, "cli_other", "https://app.example.test/callback", verifier, now,
		},
		"another address": {
			token, "cli_app", "https://app.example.test/callback2", verifier, now,
		},
		"another code entirely": {
			otherToken, "cli_app", "https://app.example.test/callback", verifier, now,
		},
		"no code at all": {
			AuthorizationCodeToken{}, "cli_app", "https://app.example.test/callback", verifier, now,
		},
		"the wrong verifier": {
			token, "cli_app", "https://app.example.test/callback",
			"a-different-verifier-of-the-very-same-length-ok", now,
		},
		"no verifier": {
			token, "cli_app", "https://app.example.test/callback", "", now,
		},
		"the challenge as the verifier": {
			token, "cli_app", "https://app.example.test/callback",
			code.Challenge().Value(), now,
		},
		"after it expired": {
			token, "cli_app", "https://app.example.test/callback", verifier,
			launchedAt.Add(31 * time.Second),
		},
		"exactly at expiry": {
			token, "cli_app", "https://app.example.test/callback", verifier,
			launchedAt.Add(30 * time.Second),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if code.Redeemable(held.token, held.client, held.redirect, held.verifier, held.now) {
				t.Error("a code was redeemable by something it was not issued to")
			}
		})
	}
}

// TestPlainPkceIsNotRepresentable. A challenge equal to its verifier protects
// against nobody who intercepted the code, so the method is refused rather than
// carried as a weaker option some client can select.
func TestPlainPkceIsNotRepresentable(t *testing.T) {
	sum := sha256.Sum256([]byte(verifier))
	valid := base64.RawURLEncoding.EncodeToString(sum[:])

	for name, method := range map[string]string{
		"plain":      "plain",
		"unstated":   "",
		"lowercased": "s256",
		"invented":   "S512",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodeChallenge(valid, method); !errors.Is(err, ErrInvalidCodeChallenge) {
				t.Fatalf("error: got %v, want ErrInvalidCodeChallenge", err)
			}
		})
	}
}

// TestAChallengeThatIsNotOneIsRefused, so Verifies never has to decide it.
func TestAChallengeThatIsNotOneIsRefused(t *testing.T) {
	for name, value := range map[string]string{
		"empty":         "",
		"too short":     "abc",
		"too long":      strings.Repeat("a", 64),
		"not base64url": strings.Repeat("!", 43),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodeChallenge(value, "S256"); !errors.Is(err, ErrInvalidCodeChallenge) {
				t.Fatalf("error: got %v, want ErrInvalidCodeChallenge", err)
			}
		})
	}
}

// TestTheZeroChallengeVerifiesNothing, which is what a row missing its challenge
// would rebuild as.
//
// This asserts the property rather than the line that carries it: the explicit
// guard and the constant-time comparison's own length check both refuse, so
// removing either leaves the property standing. That is the point — what must
// hold is that no verifier satisfies a challenge nobody set.
func TestTheZeroChallengeVerifiesNothing(t *testing.T) {
	var none CodeChallenge

	if !none.IsZero() {
		t.Fatal("the zero challenge did not report itself as one")
	}

	for _, attempt := range []string{"", verifier, strings.Repeat("a", 43)} {
		if none.Verifies(attempt) {
			t.Errorf("the zero challenge verified %q", attempt)
		}
	}
}

// TestAVerifierOutsideWhatRfc7636PermitsIsRefused, because a verifier some hop
// re-encodes is one that hashes to something else on arrival.
func TestAVerifierOutsideWhatRfc7636PermitsIsRefused(t *testing.T) {
	for name, held := range map[string]string{
		"too short":        strings.Repeat("a", minCodeVerifier-1),
		"too long":         strings.Repeat("a", maxCodeVerifier+1),
		"carrying a plus":  strings.Repeat("a", 42) + "+",
		"carrying a slash": strings.Repeat("a", 42) + "/",
		"carrying a space": strings.Repeat("a", 42) + " ",
	} {
		t.Run(name, func(t *testing.T) {
			// The challenge is derived from this very verifier, so the only thing
			// that can refuse it is the rule about what a verifier may be.
			if challengeFor(t, held).Verifies(held) {
				t.Error("a verifier outside the permitted set was accepted")
			}
		})
	}
}

// TestACodeIsNotIssuedWithoutWhatTheTokenEndpointMustRecheck.
func TestACodeIsNotIssuedWithoutWhatTheTokenEndpointMustRecheck(t *testing.T) {
	launch, err := NewLaunchContext("pat_7", "patient/Observation.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	valid := challengeFor(t, verifier)

	for name, held := range map[string]struct {
		redirect  RedirectURI
		challenge CodeChallenge
		launch    LaunchContext
		lifetime  time.Duration
		want      error
	}{
		"no address": {
			"", valid, launch, 30 * time.Second, ErrInvalidRedirectURI,
		},
		"no challenge": {
			"https://app.example.test/callback", CodeChallenge{}, launch,
			30 * time.Second, ErrInvalidCodeChallenge,
		},
		"nothing approved": {
			"https://app.example.test/callback", valid, LaunchContext{},
			30 * time.Second, ErrInvalidLaunch,
		},
		"a lifetime past the ceiling": {
			"https://app.example.test/callback", valid, launch,
			maxAuthorizationCodeLifetime + time.Second, ErrInvalidAuthorizationCode,
		},
		"no lifetime at all": {
			"https://app.example.test/callback", valid, launch, 0, ErrInvalidAuthorizationCode,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := IssueAuthorizationCode(
				"prj_a", "acd_1", "cli_app", "usr_1", "pm_1",
				held.redirect, held.challenge, held.launch,
				launchedAt, held.lifetime, rand.Reader)

			if !errors.Is(err, held.want) {
				t.Fatalf("error: got %v, want %v", err, held.want)
			}
		})
	}
}

// TestNoRenderingOfACodeCarriesIt, because a code that reaches a log line
// through ordinary formatting outlives the sixty seconds it was meant to have.
func TestNoRenderingOfACodeCarriesIt(t *testing.T) {
	code, token := approvedCode(t)

	marshalled, err := token.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	rendered := []string{
		token.String(), token.GoString(),
		code.String(), code.GoString(),
		string(marshalled),
	}

	for _, held := range rendered {
		if strings.Contains(held, token.Reveal()) {
			t.Errorf("a rendering carried the code: %s", held)
		}

		// And the digest never appears either, which is what a redemption
		// compares against.
		if strings.Contains(held, code.Digest()) {
			t.Errorf("a rendering carried the stored digest: %s", held)
		}
	}
}

// TestAPersistedCodeRebuildsThroughItsOwnConstructors, so a row nothing could
// have written is refused rather than redeemed.
func TestAPersistedCodeRebuildsThroughItsOwnConstructors(t *testing.T) {
	code, _ := approvedCode(t)

	complete := AuthorizationCodeRecord{
		ID: code.ID(), Client: code.Client(), User: code.User(), Membership: code.Membership(),
		RedirectURI: string(code.RedirectURI()), CodeChallenge: code.Challenge().Value(),
		LaunchPatient: "pat_7", GrantedScopes: "patient/Observation.read",
		Digest: code.Digest(), CreatedAt: code.CreatedAt(), ExpiresAt: code.ExpiresAt(),
	}

	if _, err := NewAuthorizationCode("prj_a", complete); err != nil {
		t.Fatalf("a row this server wrote was refused: %v", err)
	}

	for name, mutate := range map[string]func(AuthorizationCodeRecord) AuthorizationCodeRecord{
		"holding nothing to compare against": func(r AuthorizationCodeRecord) AuthorizationCodeRecord {
			r.Digest = ""

			return r
		},
		"with no challenge": func(r AuthorizationCodeRecord) AuthorizationCodeRecord {
			r.CodeChallenge = ""

			return r
		},
		"returned at an address a code cannot reach": func(r AuthorizationCodeRecord) AuthorizationCodeRecord {
			r.RedirectURI = "javascript:alert(1)"

			return r
		},
		"granting nothing": func(r AuthorizationCodeRecord) AuthorizationCodeRecord {
			r.GrantedScopes = ""

			return r
		},
		"living past the ceiling": func(r AuthorizationCodeRecord) AuthorizationCodeRecord {
			r.ExpiresAt = r.CreatedAt.Add(maxAuthorizationCodeLifetime + time.Second)

			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAuthorizationCode("prj_a", mutate(complete)); err == nil {
				t.Error("a row nothing could have written was rebuilt")
			}
		})
	}
}
