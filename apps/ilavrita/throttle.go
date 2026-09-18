package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// What a login may attempt before it is made to wait. The identity limit is the
// tighter one: guessing one password is the attack, and spreading it across
// addresses from one host is the same attack wearing a hat.
const (
	attemptsPerIdentity = 5
	attemptsPerAddress  = 20
	attemptWindow       = 15 * time.Minute
)

// errThrottleUnavailable reports that this server could not tell whether an
// attempt was within the limit. It refuses rather than guessing: a limiter that
// silently stops limiting is the failure nobody notices until afterwards.
var errThrottleUnavailable = errors.New("ilavrita: the login throttle cannot be consulted")

// attemptStore counts failed logins. It is an interface so the route can be
// tested against a counter whose clock a test moves, and so nothing here has to
// know where the counts live.
type attemptStore interface {
	Failures(ctx context.Context, key project.AttemptKey, since time.Time) (int, error)
	RecordFailure(ctx context.Context, key project.AttemptKey, at time.Time) error
	ClearFailures(ctx context.Context, key project.AttemptKey) error
	Sweep(ctx context.Context, before time.Time) error
}

// attemptLimiter refuses a login that has already been guessed at too often.
//
// The counts live in the database rather than in this process, because a
// deployment running three replicas would otherwise allow three times the
// guesses the limit states — and which replica answers is not something an
// attacker has to leave to chance.
//
// It holds no address and no credential: the keys it counts against are
// digests, so nothing here is worth stealing.
type attemptLimiter struct {
	attempts attemptStore
	now      func() time.Time
}

// newAttemptLimiter builds one. A nil clock falls back to the real one, so weak
// wiring cannot substitute a clock that never advances.
func newAttemptLimiter(attempts attemptStore, now func() time.Time) *attemptLimiter {
	if now == nil {
		now = time.Now
	}

	return &attemptLimiter{attempts: attempts, now: now}
}

// permits reports whether this attempt may proceed. It is asked before the
// password is proved, so a refused caller costs nothing but the lookup — which
// is the point: argon2id is expensive by design, and answering a guess is the
// work an attacker is trying to make this server do.
func (l *attemptLimiter) permits(ctx context.Context, identity, address project.AttemptKey) error {
	// A backend wired without one counts nothing, so it limits nothing. That is
	// a wiring mistake rather than a decision, and refusing every login would be
	// a worse answer than an unlimited one.
	if l == nil || l.attempts == nil {
		return nil
	}

	at := l.now()

	if err := l.attempts.Sweep(ctx, at.Add(-attemptWindow)); err != nil {
		return fmt.Errorf("%w: %w", errThrottleUnavailable, err)
	}

	for _, held := range []struct {
		key     project.AttemptKey
		allowed int
	}{{identity, attemptsPerIdentity}, {address, attemptsPerAddress}} {
		failures, err := l.attempts.Failures(ctx, held.key, at.Add(-attemptWindow))
		if err != nil {
			return fmt.Errorf("%w: %w", errThrottleUnavailable, err)
		}

		if failures >= held.allowed {
			return errTooManyAttempts
		}
	}

	return nil
}

// failed records one refused attempt against both keys.
func (l *attemptLimiter) failed(ctx context.Context, identity, address project.AttemptKey) {
	if l == nil || l.attempts == nil {
		return
	}

	at := l.now()

	for _, key := range []project.AttemptKey{identity, address} {
		if err := l.attempts.RecordFailure(ctx, key, at); err != nil {
			// The attempt was already refused and there is nothing left to undo.
			// A count this server could not write down is reported rather than
			// turned into a different answer for the caller.
			report(err)
		}
	}
}

// succeeded clears the identity's failures, so someone who mistyped a password
// four times is not locked out by their own success.
func (l *attemptLimiter) succeeded(ctx context.Context, identity project.AttemptKey) {
	if l == nil || l.attempts == nil {
		return
	}

	if err := l.attempts.ClearFailures(ctx, identity); err != nil {
		report(err)
	}
}
