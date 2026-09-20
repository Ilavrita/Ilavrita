package project

import (
	"fmt"
	"strings"
)

// maxGrantedScopes bounds what one session may carry. SMART puts the granted
// scopes in the token response and this build stores them verbatim, so the
// ceiling is the row's rather than a protocol's: a scope string longer than this
// is a caller filling the table, not an app describing itself.
const maxGrantedScopes = 4096

// LaunchContext is what an app's session was launched with: the scopes the app
// was granted, and the patient it was granted them for.
//
// The strings are opaque here. What a scope means is authz's to say, and this
// package deliberately does not know: a session stores what the token response
// reported, so what the app was told it holds and what the server narrows by are
// the same text rather than two renderings of it.
//
// A LaunchContext granting nothing is refused, because a session that is an
// app's and says nothing about what the app was granted would be
// indistinguishable from a session that is nobody's app at all — and that one is
// narrowed by nothing.
type LaunchContext struct {
	patient string
	scopes  string
}

// NewLaunchContext reads what an app was granted. It is the one way an outside
// value becomes a LaunchContext, so a stored context always granted something.
//
// patient is empty when the session was not launched in a patient context, which
// is an ordinary state: a patient-context scope with no patient authorizes
// nothing, so absence narrows rather than widens.
func NewLaunchContext(patient, scopes string) (LaunchContext, error) {
	scopes = strings.Join(strings.Fields(scopes), " ")
	if scopes == "" {
		return LaunchContext{}, fmt.Errorf(
			"%w: an app's session states which scopes the app was granted", ErrInvalidLaunch)
	}

	if len(scopes) > maxGrantedScopes {
		return LaunchContext{}, fmt.Errorf(
			"%w: %d characters of granted scopes exceeds the %d a session carries",
			ErrInvalidLaunch, len(scopes), maxGrantedScopes)
	}

	patient = strings.TrimSpace(patient)
	if strings.ContainsAny(patient, " \t\r\n") {
		return LaunchContext{}, fmt.Errorf("%w: %q is not one logical id", ErrInvalidLaunch, patient)
	}

	return LaunchContext{patient: patient, scopes: scopes}, nil
}

// Patient returns the patient the session was launched for, empty when it was
// not launched in a patient context.
func (l LaunchContext) Patient() string {
	return l.patient
}

// Scopes returns the granted scopes as the token response reported them.
func (l LaunchContext) Scopes() string {
	return l.scopes
}

// IsZero reports whether this session belongs to no app. A session with no
// launch context is an ordinary login: there are no granted scopes to narrow by,
// because nobody asked for a subset of what the person already holds.
func (l LaunchContext) IsZero() bool {
	return l.scopes == ""
}

// String renders the context, scopes included: a scope is a public claim about
// what an app may do, and the token response already told the app its own.
func (l LaunchContext) String() string {
	if l.IsZero() {
		return "no launch context"
	}

	if l.patient == "" {
		return "granted " + l.scopes
	}

	return "granted " + l.scopes + " for Patient/" + l.patient
}
