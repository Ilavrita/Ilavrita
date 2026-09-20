package project

import (
	"errors"
	"strings"
	"testing"
)

// TestARedirectMatchesExactlyOrNotAtAll.
//
// This is the whole security argument for the type. An authorization code is
// handed to whatever address the client presents, so "close enough" is how a
// code reaches somebody else: a prefix match hands it to any path on the host,
// a host match hands it to any open redirect there, and a normalising compare
// hands it to whatever the normaliser and the browser disagree about.
func TestARedirectMatchesExactlyOrNotAtAll(t *testing.T) {
	registered, err := NewRedirectURIs("https://app.example.test/callback")
	if err != nil {
		t.Fatalf("NewRedirectURIs: %v", err)
	}

	if !registered.Allows("https://app.example.test/callback") {
		t.Fatal("the registered address was refused, so nothing below proves anything")
	}

	for name, presented := range map[string]string{
		"a longer path":        "https://app.example.test/callback/anything",
		"a path traversal":     "https://app.example.test/callback/../../elsewhere",
		"a query added":        "https://app.example.test/callback?next=https://evil.test",
		"another path":         "https://app.example.test/elsewhere",
		"the bare host":        "https://app.example.test",
		"a lookalike host":     "https://app.example.test.evil.test/callback",
		"a subdomain":          "https://evil.app.example.test/callback",
		"a userinfo prefix":    "https://app.example.test@evil.test/callback",
		"a different scheme":   "http://app.example.test/callback",
		"a trailing slash":     "https://app.example.test/callback/",
		"a case-changed path":  "https://app.example.test/Callback",
		"a percent-encoding":   "https://app.example.test/%63allback",
		"a default port added": "https://app.example.test:443/callback",
	} {
		t.Run(name, func(t *testing.T) {
			if registered.Allows(presented) {
				t.Errorf("a code would be handed to %q, which nobody registered", presented)
			}
		})
	}
}

// TestAnAddressACodeCannotSafelyReachIsRefused at registration, so the
// comparison above never has to decide it.
func TestAnAddressACodeCannotSafelyReachIsRefused(t *testing.T) {
	for name, stated := range map[string]string{
		"nothing at all":      "",
		"blank":               "   ",
		"relative":            "/callback",
		"scheme-relative":     "//app.example.test/callback",
		"carrying a fragment": "https://app.example.test/callback#token",
		"cleartext":           "http://app.example.test/callback",
		"javascript":          "javascript:alert(1)",
		"data":                "data:text/html,<script>fetch(location)</script>",
		"a file":              "file:///etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRedirectURI(stated); !errors.Is(err, ErrInvalidRedirectURI) {
				t.Fatalf("error: got %v, want ErrInvalidRedirectURI", err)
			}
		})
	}
}

// TestLoopbackAndPrivateSchemesAreHowNativeAppsAreCalledBack, so refusing them
// would push native apps onto a hosted redirect, which is worse than cleartext
// to a machine that is already this one.
func TestLoopbackAndPrivateSchemesAreHowNativeAppsAreCalledBack(t *testing.T) {
	for name, stated := range map[string]string{
		"loopback by address":    "http://127.0.0.1:8080/callback",
		"loopback by name":       "http://localhost:53535/callback",
		"loopback over ipv6":     "http://[::1]:8080/callback",
		"a private-use scheme":   "com.example.app:/oauth",
		"https on a nonstandard": "https://app.example.test:8443/callback",
		"https with a query":     "https://app.example.test/callback?tenant=a",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRedirectURI(stated); err != nil {
				t.Errorf("%q was refused: %v", stated, err)
			}
		})
	}
}

// TestARegistrationCannotStateOneAddressTwice, because two identical entries are
// one address, and a list tolerating them makes "how many did this client
// register" unanswerable.
func TestARegistrationCannotStateOneAddressTwice(t *testing.T) {
	_, err := NewRedirectURIs("https://app.example.test/callback", "https://app.example.test/callback")
	if !errors.Is(err, ErrInvalidRedirectURI) {
		t.Fatalf("error: got %v, want ErrInvalidRedirectURI", err)
	}
}

// TestARegistrationNamingTooManyAddressesIsRefused, which is a registration
// being used as storage rather than an app describing where it lives.
func TestARegistrationNamingTooManyAddressesIsRefused(t *testing.T) {
	many := make([]string, maxRedirectURIs+1)
	for i := range many {
		many[i] = "https://app.example.test/callback/" + strings.Repeat("a", i+1)
	}

	if _, err := NewRedirectURIs(many...); !errors.Is(err, ErrInvalidRedirectURI) {
		t.Fatalf("error: got %v, want ErrInvalidRedirectURI", err)
	}
}

// TestARegistrationNamingNoAddressIsNotAnError, because a backend service has no
// redirect to state and refusing one would make it unregistrable.
func TestARegistrationNamingNoAddressIsNotAnError(t *testing.T) {
	held, err := NewRedirectURIs()
	if err != nil {
		t.Fatalf("NewRedirectURIs: %v", err)
	}

	if !held.IsZero() || held.Len() != 0 {
		t.Errorf("a registration naming nothing reported %d addresses", held.Len())
	}

	// And it allows nothing, so "registered none" never reads as "allows any".
	if held.Allows("https://app.example.test/callback") {
		t.Error("a registration naming no address allowed one")
	}
}

// TestARegistrationSaysWhichProofItOffers.
//
// The kind is refused when unstated rather than defaulted, because the two
// answers demand different proof: guessing public would let a confidential
// client redeem a code presenting no secret, and guessing confidential would
// lock out every public app.
func TestARegistrationSaysWhichProofItOffers(t *testing.T) {
	for name, kind := range map[string]ClientKind{
		"unstated":     "",
		"unrecognised": "semi-confidential",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClientApplication("prj_a", ClientApplicationConfig{
				ID: "cli_loader", Name: "Nightly loader", State: ServiceActive, Kind: kind,
			})

			if !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("error: got %v, want ErrUnknownKind", err)
			}
		})
	}

	if !ClientConfidential.KeepsASecret() {
		t.Error("a confidential registration reported that it keeps no secret")
	}

	if ClientPublic.KeepsASecret() {
		t.Error("a public registration reported that it keeps a secret")
	}
}
