package project

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// aGrant mints the first token of a chain, with everything valid, so each test
// below changes exactly one thing.
func aGrant(t *testing.T) (RefreshGrant, RefreshToken) {
	t.Helper()

	launch, err := NewLaunchContext("pat_7", "user/Observation.read offline_access")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	grant, token, err := IssueRefreshToken(
		"prj_a", "rft_1", "rch_1", "cli_app", "usr_1", "pm_1", launch,
		launchedAt, 7*24*time.Hour, rand.Reader)
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}

	return grant, token
}

// TestOnlyAnApprovalThatAskedToOutliveItsSessionGetsOne.
//
// An approval naming neither scope is one the person agreed to for this session.
// Issuing a refresh token anyway would extend a grant nobody extended.
func TestOnlyAnApprovalThatAskedToOutliveItsSessionGetsOne(t *testing.T) {
	for scopes, want := range map[string]bool{
		"user/Observation.read":                 false,
		"user/Observation.read offline_access":  true,
		"user/Observation.read online_access":   true,
		"offline_access":                        true,
		"user/Observation.read openid fhirUser": false,
	} {
		t.Run(scopes, func(t *testing.T) {
			launch, err := NewLaunchContext("", scopes)
			if err != nil {
				t.Fatalf("NewLaunchContext: %v", err)
			}

			if got := RefreshRequested(launch); got != want {
				t.Errorf("RefreshRequested(%q) is %v, want %v", scopes, got, want)
			}
		})
	}
}

// TestRefreshingDoesNotExtendTheGrant.
//
// The ceiling travels across rotations rather than restarting. Without that, an
// app refreshing hourly would never expire, and the thirty-day limit would apply
// only to apps nobody used — which is precisely backwards.
func TestRefreshingDoesNotExtendTheGrant(t *testing.T) {
	grant, _ := aGrant(t)

	rotated, _, err := grant.Rotate("rft_2", launchedAt.Add(6*24*time.Hour), rand.Reader)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if !rotated.ExpiresAt().Equal(grant.ExpiresAt()) {
		t.Errorf("rotating moved the expiry from %s to %s",
			grant.ExpiresAt().Format(time.RFC3339), rotated.ExpiresAt().Format(time.RFC3339))
	}

	if rotated.Chain() != grant.Chain() {
		t.Errorf("rotating left the chain, %s to %s", grant.Chain(), rotated.Chain())
	}

	if rotated.Launch() != grant.Launch() {
		t.Error("rotating changed what the grant may be exchanged for")
	}
}

// TestAnExpiredGrantRotatesIntoNothing, so a grant nobody refreshed for a month
// sends the person back through consent rather than renewing itself.
func TestAnExpiredGrantRotatesIntoNothing(t *testing.T) {
	grant, _ := aGrant(t)

	for name, at := range map[string]time.Time{
		"after expiry":      launchedAt.Add(7*24*time.Hour + time.Second),
		"exactly at expiry": launchedAt.Add(7 * 24 * time.Hour),
		"long after expiry": launchedAt.Add(365 * 24 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := grant.Rotate("rft_2", at, rand.Reader); !errors.Is(
				err, ErrInvalidRefreshToken) {
				t.Fatalf("error: got %v, want ErrInvalidRefreshToken", err)
			}
		})
	}
}

// TestASpentTokenRotatesIntoNothing, because the successor already exists: a
// second rotation of one token would fork the chain, and two live tokens is the
// state rotation exists to prevent.
func TestASpentTokenRotatesIntoNothing(t *testing.T) {
	grant, _ := aGrant(t)

	spent := grant
	spent.state = RefreshSpent

	if _, _, err := spent.Rotate("rft_3", launchedAt.Add(2*time.Hour), rand.Reader); !errors.Is(
		err, ErrInvalidRefreshToken) {
		t.Fatalf("error: got %v, want ErrInvalidRefreshToken", err)
	}
}

// TestAGrantIsRedeemedOnlyByWhatItWasIssuedTo.
func TestAGrantIsRedeemedOnlyByWhatItWasIssuedTo(t *testing.T) {
	grant, token := aGrant(t)
	now := launchedAt.Add(time.Hour)

	if !grant.Redeemable(token, "cli_app", now) {
		t.Fatal("the grant refused the token it was issued for, so nothing below proves anything")
	}

	_, another := aGrant(t)

	for name, held := range map[string]struct {
		token  RefreshToken
		client ClientApplicationID
		when   time.Time
	}{
		"another client":    {token, "cli_other", now},
		"another token":     {another, "cli_app", now},
		"no token at all":   {RefreshToken{}, "cli_app", now},
		"after expiry":      {token, "cli_app", launchedAt.Add(7*24*time.Hour + time.Second)},
		"exactly at expiry": {token, "cli_app", launchedAt.Add(7 * 24 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			if grant.Redeemable(held.token, held.client, held.when) {
				t.Error("a grant was redeemable by something it was not issued to")
			}
		})
	}

	// And a spent rotation is never redeemable, however right everything else is.
	spent := grant
	spent.state = RefreshSpent

	if spent.Redeemable(token, "cli_app", now) {
		t.Error("a spent token was redeemable")
	}
}

// TestAGrantIsNotIssuedPastItsCeiling, which is the one bound on how long an
// approval outlives the moment it was given.
func TestAGrantIsNotIssuedPastItsCeiling(t *testing.T) {
	launch, err := NewLaunchContext("", "user/Observation.read offline_access")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	for name, lifetime := range map[string]time.Duration{
		"past the ceiling": maxRefreshLifetime + time.Second,
		"no lifetime":      0,
		"negative":         -time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := IssueRefreshToken(
				"prj_a", "rft_1", "rch_1", "cli_app", "usr_1", "pm_1", launch,
				launchedAt, lifetime, rand.Reader)

			if !errors.Is(err, ErrInvalidRefreshToken) {
				t.Fatalf("error: got %v, want ErrInvalidRefreshToken", err)
			}
		})
	}
}

// TestAGrantExchangeableForNothingIsRefused, because it would mint a session
// narrowed by nothing — the one thing the launch context exists to prevent.
func TestAGrantExchangeableForNothingIsRefused(t *testing.T) {
	_, _, err := IssueRefreshToken(
		"prj_a", "rft_1", "rch_1", "cli_app", "usr_1", "pm_1", LaunchContext{},
		launchedAt, time.Hour, rand.Reader)

	if !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("error: got %v, want ErrInvalidLaunch", err)
	}
}

// TestNoRenderingOfARefreshTokenCarriesIt. It is the longest-lived credential
// this server issues, so it is the one a log line must never hold.
func TestNoRenderingOfARefreshTokenCarriesIt(t *testing.T) {
	grant, token := aGrant(t)

	marshalled, err := token.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	for _, held := range []string{
		token.String(), token.GoString(), grant.String(), grant.GoString(), string(marshalled),
	} {
		if strings.Contains(held, token.Reveal()) {
			t.Errorf("a rendering carried the token: %s", held)
		}

		if strings.Contains(held, grant.Digest()) {
			t.Errorf("a rendering carried the stored digest: %s", held)
		}
	}
}

// TestASpentRowKeepsItsDigest, because that is what makes a replay recognisable
// rather than merely unknown — and unknown is what a guess answers too.
func TestASpentRowKeepsItsDigest(t *testing.T) {
	grant, _ := aGrant(t)

	spent, err := NewRefreshGrant("prj_a", RefreshRecord{
		ID: "rft_1", Chain: "rch_1", Client: "cli_app", User: "usr_1", Membership: "pm_1",
		LaunchPatient: "pat_7", GrantedScopes: "user/Observation.read offline_access",
		State: RefreshSpent, Digest: grant.Digest(),
		CreatedAt: grant.CreatedAt(), ExpiresAt: grant.ExpiresAt(),
	})
	if err != nil {
		t.Fatalf("NewRefreshGrant: %v", err)
	}

	if !spent.Spent() {
		t.Error("a spent row did not report itself as spent")
	}

	if spent.Digest() != grant.Digest() {
		t.Error("a spent row lost the digest a replay is recognised by")
	}

	// A row holding nothing to compare against could never be recognised.
	_, err = NewRefreshGrant("prj_a", RefreshRecord{
		ID: "rft_1", Chain: "rch_1", Client: "cli_app", User: "usr_1", Membership: "pm_1",
		GrantedScopes: "offline_access", State: RefreshSpent, Digest: "",
		CreatedAt: grant.CreatedAt(), ExpiresAt: grant.ExpiresAt(),
	})

	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("error: got %v, want ErrInvalidRefreshToken", err)
	}
}
