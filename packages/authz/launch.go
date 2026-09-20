package authz

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ErrMissingLaunch reports a request that did not say whether an app is asking.
//
// It is refused rather than read as "no app", because those two are the same
// value and opposite answers: a session an app holds must be narrowed to what
// that app was granted, and one nobody's app holds must not be narrowed at all.
// A caller that never considered the question is a wiring mistake, and the
// mistake widens, so it is an error the way a nil resolver is.
var ErrMissingLaunch = errors.New("authz: request needs to state whether an app is asking")

// Launch is what one request's session was launched with, already parsed: the
// scopes the app was granted, and the patient it was granted them for.
//
// Its zero value states nothing, and BuildScope refuses it. The two ways to hold
// one are NoLaunch, for a request no app is behind, and ParseLaunch, for a
// session's own stored context — so every call site had to decide which, rather
// than reaching the permissive answer by leaving a field alone.
type Launch struct {
	// stated separates "no app is asking" from "nobody said". Both look like an
	// empty scope list, and only one of them may skip the narrowing.
	stated bool

	// app is whether a SMART app holds this session at all.
	app bool

	patient storage.LogicalID
	granted []SmartScope
}

// NoLaunch is a request no app is behind: an ordinary login, or a background
// task with no session at all. Nothing narrows it, because nobody asked for a
// subset of what the principal already holds.
func NoLaunch() Launch {
	return Launch{stated: true}
}

// ParseLaunch reads a session's stored launch context.
//
// A context granting nothing is an ordinary login and parses to NoLaunch. One
// granting something parses every scope, and a scope this build cannot honour
// exactly fails the whole launch rather than being dropped: a scope silently
// discarded here would widen what survives, because what is discarded is a
// restriction.
func ParseLaunch(held project.LaunchContext) (Launch, error) {
	if held.IsZero() {
		return NoLaunch(), nil
	}

	stated := strings.Fields(held.Scopes())
	granted := make([]SmartScope, 0, len(stated))

	for _, one := range stated {
		// Carried in the approval, absent from the narrowing: these name no
		// resource type, so there is nothing here for them to restrict.
		if NarrowsNothing(one) {
			continue
		}

		scope, err := ParseScope(one)
		if err != nil {
			return Launch{}, fmt.Errorf("authz: read granted scopes: %w", err)
		}

		granted = append(granted, scope)
	}

	return Launch{
		stated:  true,
		app:     true,
		patient: storage.LogicalID(held.Patient()),
		granted: granted,
	}, nil
}

// App reports whether a SMART app holds the session this launch came from.
func (l Launch) App() bool {
	return l.app
}

// Patient returns the patient the session was launched for, empty when it was
// not launched in a patient context.
func (l Launch) Patient() storage.LogicalID {
	return l.patient
}

// Narrow returns what the app holds: everything the principal holds, narrowed to
// what the app was granted and never anything more.
//
// The three answers are the three states. A launch nobody stated — the zero
// value, and what ParseLaunch hands back beside an error — narrows to nothing,
// because returning everything would make ignoring that error the widest
// possible mistake. A launch stated to be nobody's app is returned untouched,
// there being no restriction to apply. An app's is intersected.
func (l Launch) Narrow(held storage.Scope) storage.Scope {
	switch {
	case !l.stated:
		return storage.Scope{}
	case !l.app:
		return held
	default:
		return Narrow(held, l.granted, l.patient)
	}
}
