package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// heldClock is a clock a test moves itself, so a window is stated rather than
// waited for.
type heldClock struct {
	at time.Time
}

func (h *heldClock) now() time.Time { return h.at }

func (h *heldClock) advance(by time.Duration) { h.at = h.at.Add(by) }

// testKeys is a keyer with a key, so these tests count against names a
// deployment would actually write rather than against unkeyed ones.
var testKeys = project.NewAttemptKeys(testSealingKey())

// testSealingKey is one fixed key, so the names these tests compute are stable
// across a run without being the zero key a deployment is warned about.
func testSealingKey() project.SealingKey {
	key, err := project.ParseSealingKey(base64.StdEncoding.EncodeToString(
		bytes.Repeat([]byte("throttle-key-32-bytes-exactly!!!"), 1)))
	if err != nil {
		panic("the fixed test sealing key is not one: " + err.Error())
	}

	return key
}

// The two keys every case below counts against.
var (
	oneNurse    = testKeys.Identity("clinic-a", "nurse@example.test")
	otherNurse  = testKeys.Identity("clinic-a", "other@example.test")
	oneHost     = testKeys.Address("10.0.0.1")
	anotherHost = testKeys.Address("10.0.0.2")
)

// stoppedLimiter builds a limiter over a real database with a clock the test
// moves. The counts live where they really live, so what these tests assert is
// what a deployment does.
func stoppedLimiter(t *testing.T) (*attemptLimiter, *heldClock, *sql.DB) {
	t.Helper()

	db := preparedDatabase(t)
	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}

	return newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, clock.now), clock, db
}

func permitted(t *testing.T, limiter *attemptLimiter, identity, address project.AttemptKey) bool {
	t.Helper()

	switch err := limiter.permits(context.Background(), identity, address); {
	case err == nil:
		return true
	case errors.Is(err, errTooManyAttempts):
		return false
	default:
		t.Fatalf("the limiter could not answer: %v", err)

		return false
	}
}

// TestAnIdentityIsThrottledBeforeItsPasswordIsProved. argon2id is expensive by
// design, so answering a guess is the work an attacker wants done: the limit is
// checked first, and a refused attempt costs a lookup.
func TestAnIdentityIsThrottledBeforeItsPasswordIsProved(t *testing.T) {
	limiter, _, _ := stoppedLimiter(t)

	for attempt := range attemptsPerIdentity {
		if !permitted(t, limiter, oneNurse, oneHost) {
			t.Fatalf("attempt %d was refused before the limit", attempt)
		}

		limiter.failed(context.Background(), oneNurse, oneHost)
	}

	if permitted(t, limiter, oneNurse, oneHost) {
		t.Error("a sixth attempt was permitted")
	}

	// Another identity from the same host is unaffected until the address limit.
	if !permitted(t, limiter, otherNurse, oneHost) {
		t.Error("one identity's failures locked out another")
	}
}

// TestTheWindowReopens, because a limit that never forgets is a permanent
// lockout an attacker can trigger on anyone's behalf.
func TestTheWindowReopens(t *testing.T) {
	limiter, clock, _ := stoppedLimiter(t)

	for range attemptsPerIdentity {
		limiter.failed(context.Background(), oneNurse, oneHost)
	}

	if permitted(t, limiter, oneNurse, oneHost) {
		t.Fatal("the limit did not apply")
	}

	clock.advance(attemptWindow + time.Second)

	if !permitted(t, limiter, oneNurse, oneHost) {
		t.Error("the window never reopened")
	}
}

// TestSuccessClearsWhatCameBefore, so someone who mistyped their password four
// times is not locked out by their own success.
func TestSuccessClearsWhatCameBefore(t *testing.T) {
	limiter, _, _ := stoppedLimiter(t)

	for range attemptsPerIdentity - 1 {
		limiter.failed(context.Background(), oneNurse, oneHost)
	}

	limiter.succeeded(context.Background(), oneNurse)

	for attempt := range attemptsPerIdentity {
		if !permitted(t, limiter, oneNurse, oneHost) {
			t.Fatalf("attempt %d was refused after a success cleared the count", attempt)
		}

		limiter.failed(context.Background(), oneNurse, oneHost)
	}
}

// TestOneHostIsLimitedAcrossIdentities. Spreading a guess across addresses from
// one host is the same attack wearing a hat.
func TestOneHostIsLimitedAcrossIdentities(t *testing.T) {
	limiter, _, _ := stoppedLimiter(t)

	for attempt := range attemptsPerAddress {
		identity := testKeys.Identity("clinic-a",
			string(rune('a'+attempt%26))+"@example.test")
		limiter.failed(context.Background(), identity, oneHost)
	}

	fresh := testKeys.Identity("clinic-a", "fresh@example.test")

	if permitted(t, limiter, fresh, oneHost) {
		t.Error("a host past its limit was permitted a fresh identity")
	}

	if !permitted(t, limiter, fresh, anotherHost) {
		t.Error("one host's failures locked out another")
	}
}

// TestTheLimitIsSharedAcrossProcesses. A deployment running three replicas
// would otherwise allow three times the guesses the limit states, and which
// replica answers is not something an attacker has to leave to chance.
func TestTheLimitIsSharedAcrossProcesses(t *testing.T) {
	db := preparedDatabase(t)
	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}

	// Two limiters, as two processes serving one install are.
	first := newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, clock.now)
	second := newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, clock.now)

	for range attemptsPerIdentity {
		first.failed(context.Background(), oneNurse, oneHost)
	}

	if permitted(t, second, oneNurse, oneHost) {
		t.Error("a second process did not see the first's refusals")
	}

	// And a success on one is seen by the other.
	second.succeeded(context.Background(), oneNurse)

	if !permitted(t, first, oneNurse, oneHost) {
		t.Error("a second process's success did not clear the first's count")
	}
}

// TestTheThrottleStoresNeitherAnAddressNorAnIdentity. A table of who tried to
// log in and failed is a list of this install's users and where they were, kept
// somewhere nobody thinks to look.
func TestTheThrottleStoresNeitherAnAddressNorAnIdentity(t *testing.T) {
	limiter, _, db := stoppedLimiter(t)

	limiter.failed(context.Background(), oneNurse, oneHost)

	rows, err := db.QueryContext(context.Background(), "SELECT key FROM login_attempts")
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	defer func() { _ = rows.Close() }()

	held := 0

	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan: %v", err)
		}

		held++

		for _, secret := range []string{"nurse", "example.test", "clinic-a", "10.0.0.1"} {
			if strings.Contains(key, secret) {
				t.Errorf("a stored key carries %q", secret)
			}
		}

		if len(key) != 64 {
			t.Errorf("a stored key is %d characters, want a digest", len(key))
		}
	}

	if held != 2 {
		t.Errorf("one failure recorded %d rows, want one per key", held)
	}
}

// TestSweepingDropsWhatAgedOut, so the table does not grow with every address
// anyone ever guessed at.
func TestSweepingDropsWhatAgedOut(t *testing.T) {
	limiter, clock, db := stoppedLimiter(t)

	limiter.failed(context.Background(), oneNurse, oneHost)

	clock.advance(attemptWindow + time.Second)

	// permits sweeps on the way past, so no goroutine outlives a request.
	permitted(t, limiter, oneNurse, oneHost)

	var remaining int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM login_attempts").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}

	if remaining != 0 {
		t.Errorf("%d attempts outlived the window", remaining)
	}
}

// refusingAttempts is a counter that cannot answer. Each operation fails on its
// own, because a fake that failed at the first one would leave every path after
// it unreached and untested.
type refusingAttempts struct {
	err                           error
	onFailures, onSweep, onRecord bool
	onClear                       bool
}

func (r refusingAttempts) Failures(context.Context, project.AttemptKey, time.Time) (int, error) {
	if r.onFailures {
		return 0, r.err
	}

	return 0, nil
}

func (r refusingAttempts) RecordFailure(context.Context, project.AttemptKey, time.Time) error {
	if r.onRecord {
		return r.err
	}

	return nil
}

func (r refusingAttempts) ClearFailures(context.Context, project.AttemptKey) error {
	if r.onClear {
		return r.err
	}

	return nil
}

func (r refusingAttempts) Sweep(context.Context, time.Time) error {
	if r.onSweep {
		return r.err
	}

	return nil
}

// TestAThrottleThatCannotBeConsultedRefuses. A limiter that silently stops
// limiting is the failure nobody notices until afterwards, so it is answered as
// a fault rather than as an unlimited one — whichever half of the check broke.
func TestAThrottleThatCannotBeConsultedRefuses(t *testing.T) {
	broken := errors.New("the counter is unavailable")

	for name, counter := range map[string]refusingAttempts{
		"the count cannot be read":        {err: broken, onFailures: true},
		"what aged out cannot be dropped": {err: broken, onSweep: true},
	} {
		limiter := newAttemptLimiter(counter, testKeys, nil)

		err := limiter.permits(context.Background(), oneNurse, oneHost)
		if !errors.Is(err, errThrottleUnavailable) {
			t.Errorf("%s: err = %v, want %v", name, err, errThrottleUnavailable)
		}

		if errors.Is(err, errTooManyAttempts) {
			t.Errorf("%s: a fault was reported as the caller having guessed too often", name)
		}
	}
}

// TestCapitalisationBuysNoFreshAttempts. An attacker who could reset the count
// by spelling the same mailbox differently would not be limited at all.
func TestCapitalisationBuysNoFreshAttempts(t *testing.T) {
	limiter, _, _ := stoppedLimiter(t)

	for range attemptsPerIdentity {
		limiter.failed(context.Background(), oneNurse, oneHost)
	}

	for _, spelling := range []string{
		"NURSE@EXAMPLE.TEST", "Nurse@Example.Test", "  nurse@example.test  ",
	} {
		same := testKeys.Identity("clinic-a", spelling)

		if permitted(t, limiter, same, anotherHost) {
			t.Errorf("%q was counted as a different identity", spelling)
		}
	}

	// A genuinely different mailbox is unaffected, so the folding above is what
	// matched rather than everything matching.
	if !permitted(t, limiter, otherNurse, anotherHost) {
		t.Error("folding the address matched a different mailbox")
	}
}

// TestTheProjectIsPartOfTheIdentity, so one Project's failures never lock a
// person out of another.
func TestTheProjectIsPartOfTheIdentity(t *testing.T) {
	limiter, _, _ := stoppedLimiter(t)

	for range attemptsPerIdentity {
		limiter.failed(context.Background(), oneNurse, oneHost)
	}

	elsewhere := testKeys.Identity("clinic-b", "nurse@example.test")

	if !permitted(t, limiter, elsewhere, anotherHost) {
		t.Error("one Project's failures locked the same address out of another")
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
