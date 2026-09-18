package main

import (
	"net/http"
	"testing"
	"time"
)

// heldClock is a clock a test moves itself, so a window is stated rather than
// waited for.
type heldClock struct {
	at time.Time
}

func (h *heldClock) now() time.Time { return h.at }

func (h *heldClock) advance(by time.Duration) { h.at = h.at.Add(by) }

func stoppedLimiter() (*attemptLimiter, *heldClock) {
	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}

	return newAttemptLimiter(clock.now), clock
}

// TestAnIdentityIsThrottledBeforeItsPasswordIsProved. argon2id is expensive by
// design, so answering a guess is the work an attacker wants done: the limit is
// checked first, and a refused attempt costs a map lookup.
func TestAnIdentityIsThrottledBeforeItsPasswordIsProved(t *testing.T) {
	limiter, _ := stoppedLimiter()

	for attempt := range attemptsPerIdentity {
		if !limiter.permits("clinic-a|nurse@example.test", "10.0.0.1") {
			t.Fatalf("attempt %d was refused before the limit", attempt)
		}

		limiter.failed("clinic-a|nurse@example.test", "10.0.0.1")
	}

	if limiter.permits("clinic-a|nurse@example.test", "10.0.0.1") {
		t.Error("a sixth attempt was permitted")
	}

	// Another identity from the same host is unaffected until the address limit.
	if !limiter.permits("clinic-a|other@example.test", "10.0.0.1") {
		t.Error("one identity's failures locked out another")
	}
}

// TestTheWindowReopens, because a limit that never forgets is a permanent
// lockout an attacker can trigger on anyone's behalf.
func TestTheWindowReopens(t *testing.T) {
	limiter, clock := stoppedLimiter()

	for range attemptsPerIdentity {
		limiter.failed("clinic-a|nurse@example.test", "10.0.0.1")
	}

	if limiter.permits("clinic-a|nurse@example.test", "10.0.0.1") {
		t.Fatal("the limit did not apply")
	}

	clock.advance(attemptWindow + time.Second)

	if !limiter.permits("clinic-a|nurse@example.test", "10.0.0.1") {
		t.Error("the window never reopened")
	}
}

// TestSuccessClearsWhatCameBefore, so someone who mistyped their password four
// times is not locked out by their own success.
func TestSuccessClearsWhatCameBefore(t *testing.T) {
	limiter, _ := stoppedLimiter()

	for range attemptsPerIdentity - 1 {
		limiter.failed("clinic-a|nurse@example.test", "10.0.0.1")
	}

	limiter.succeeded("clinic-a|nurse@example.test")

	for attempt := range attemptsPerIdentity {
		if !limiter.permits("clinic-a|nurse@example.test", "10.0.0.1") {
			t.Fatalf("attempt %d was refused after a success cleared the count", attempt)
		}

		limiter.failed("clinic-a|nurse@example.test", "10.0.0.1")
	}
}

// TestOneHostIsLimitedAcrossIdentities. Spreading a guess across addresses from
// one host is the same attack wearing a hat.
func TestOneHostIsLimitedAcrossIdentities(t *testing.T) {
	limiter, _ := stoppedLimiter()

	for attempt := range attemptsPerAddress {
		identity := "clinic-a|" + string(rune('a'+attempt%26)) + "@example.test"
		limiter.failed(identity, "10.0.0.1")
	}

	if limiter.permits("clinic-a|fresh@example.test", "10.0.0.1") {
		t.Error("a host past its limit was permitted a fresh identity")
	}

	if !limiter.permits("clinic-a|fresh@example.test", "10.0.0.2") {
		t.Error("one host's failures locked out another")
	}
}

// TestTheLoginRouteThrottles is what the limiter exists for: the route refuses
// before it proves anything, and says the same thing whether or not the address
// exists, so the limit cannot be used to enumerate one.
func TestTheLoginRouteThrottles(t *testing.T) {
	routes, _ := authenticatedServer(t)

	for attempt := range attemptsPerIdentity {
		answer := logInOver(t, routes, loginAddress, "not the password")
		if answer.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", attempt, answer.Code)
		}
	}

	assertStatus(t, logInOver(t, routes, loginAddress, "not the password"), http.StatusTooManyRequests)

	// The correct password is refused too while the window holds: the limit is
	// about the attempt rate, not about whether this one would have worked.
	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusTooManyRequests)
}

// TestASuccessfulLoginIsNotThrottled, so the limit never stands between an
// ordinary person and their own account.
func TestASuccessfulLoginIsNotThrottled(t *testing.T) {
	routes, _ := authenticatedServer(t)

	for range attemptsPerIdentity - 1 {
		assertStatus(t, logInOver(t, routes, loginAddress, "not the password"), http.StatusUnauthorized)
	}

	assertStatus(t, logInOver(t, routes, loginAddress, loginPassword), http.StatusOK)

	// The success cleared the count, so a slip afterwards is answered on its own
	// terms rather than as the last of a run.
	assertStatus(t, logInOver(t, routes, loginAddress, "not the password"), http.StatusUnauthorized)
}
