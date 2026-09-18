package main

import (
	"sync"
	"time"
)

// What a login may attempt before it is made to wait. The identity limit is the
// tighter one: guessing one password is the attack, and spreading it across
// addresses from one host is the same attack wearing a hat.
const (
	attemptsPerIdentity = 5
	attemptsPerAddress  = 20
	attemptWindow       = 15 * time.Minute
)

// sweepAfter is how long an idle bucket is kept before it is dropped. The map
// would otherwise grow with every address anyone ever guessed at.
const sweepAfter = time.Hour

// attemptLimiter counts failed logins per identity and per client address. It
// holds no credential and no outcome, only that something failed and when, so
// nothing here is worth stealing.
//
// It lives in this process. A second instance counts its own attempts, which is
// a limit worth stating rather than a guarantee worth assuming.
type attemptLimiter struct {
	mutex   sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

// bucket is one key's recent failures.
type bucket struct {
	failures []time.Time
	touched  time.Time
}

// newAttemptLimiter builds one. A nil clock falls back to the real one, so weak
// wiring cannot substitute a clock that never advances.
func newAttemptLimiter(now func() time.Time) *attemptLimiter {
	if now == nil {
		now = time.Now
	}

	return &attemptLimiter{buckets: map[string]*bucket{}, now: now}
}

// permits reports whether this attempt may proceed. It is asked before the
// password is proved, so a refused caller costs nothing but the lookup — which
// is the point: argon2id is expensive by design, and answering a guess is the
// work an attacker is trying to make this server do.
func (l *attemptLimiter) permits(identity, address string) bool {
	// A backend wired without one counts nothing, so it limits nothing. That is a
	// wiring mistake rather than a decision, and crashing would be a worse answer
	// than an unlimited one.
	if l == nil {
		return true
	}

	l.mutex.Lock()
	defer l.mutex.Unlock()

	at := l.now()
	l.sweep(at)

	return l.under(identity, attemptsPerIdentity, at) && l.under(address, attemptsPerAddress, at)
}

// failed records one refused attempt against both keys.
func (l *attemptLimiter) failed(identity, address string) {
	if l == nil {
		return
	}

	l.mutex.Lock()
	defer l.mutex.Unlock()

	at := l.now()

	for _, key := range []string{identity, address} {
		held, kept := l.buckets[key]
		if !kept {
			held = &bucket{}
			l.buckets[key] = held
		}

		held.failures = append(held.failures, at)
		held.touched = at
	}
}

// succeeded clears the identity's failures, so someone who mistyped a password
// four times is not locked out by their own success.
func (l *attemptLimiter) succeeded(identity string) {
	if l == nil {
		return
	}

	l.mutex.Lock()
	defer l.mutex.Unlock()

	delete(l.buckets, identity)
}

// under reports whether this key has room left in the window.
func (l *attemptLimiter) under(key string, allowed int, at time.Time) bool {
	held, kept := l.buckets[key]
	if !kept {
		return true
	}

	return len(within(held.failures, at.Add(-attemptWindow))) < allowed
}

// sweep drops buckets nothing has touched, and expires failures that aged out of
// the window. Both run on the attempt path, so no goroutine outlives a request.
func (l *attemptLimiter) sweep(at time.Time) {
	floor := at.Add(-attemptWindow)

	for key, held := range l.buckets {
		held.failures = within(held.failures, floor)

		if len(held.failures) == 0 && at.Sub(held.touched) > sweepAfter {
			delete(l.buckets, key)
		}
	}
}

// within returns the failures at or after the floor, oldest first.
func within(failures []time.Time, floor time.Time) []time.Time {
	kept := failures[:0]

	for _, failure := range failures {
		if failure.After(floor) {
			kept = append(kept, failure)
		}
	}

	return kept
}
