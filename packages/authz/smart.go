package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// A SMART scope narrows what a person already holds. It never widens it.
//
// An app acting for somebody must not reach anything that somebody cannot
// reach. Their standing already produces a Scope — an AccessPolicy compiled
// into Grants with compartments, filters and projections — and a SMART scope is
// an additional restriction the app asked for, on top of that.
//
// So Narrow intersects. It never unions and never replaces: `user/*.*` does not
// make an app omnipotent, it makes it exactly as capable as the person who
// approved it, which is the most it may ever be.
//
// Building Grants *from* the scopes instead would make a scope string a
// privilege, which is the mistake this package exists to make impossible.
// docs/design/smart-scope-spec.md is where the rest of the reasoning lives.

var (
	// ErrMalformedScope reports a scope string that is not one.
	ErrMalformedScope = errors.New("authz: that is not a SMART scope")

	// ErrUnsupportedScope reports a scope this build cannot honour exactly.
	//
	// It is an error rather than a narrowing, because SMART lets a server grant
	// fewer scopes than were asked for and a client is required to read what it
	// was granted. Refusing is visible; quietly granting something adjacent is
	// not.
	ErrUnsupportedScope = errors.New("authz: this server does not grant that scope")
)

// SmartContext is whose data a scope names.
type SmartContext string

// The contexts SMART defines.
const (
	// ContextPatient is one patient's compartment, named by the launch rather
	// than by the scope.
	ContextPatient SmartContext = "patient"

	// ContextUser is whatever the person themselves may reach.
	ContextUser SmartContext = "user"

	// ContextSystem is a backend service with no person behind it.
	ContextSystem SmartContext = "system"
)

// SmartScope is one parsed scope.
type SmartScope struct {
	Context SmartContext

	// Type is the resource type, empty when the scope named every type with a
	// star. It is left empty rather than expanded here, because what a star
	// covers is decided against what the person already holds.
	Type storage.ResourceType

	// Actions are what the scope permits, already resolved from v1 words or v2
	// letters into the actions this build authorizes.
	Actions []storage.Action
}

// Everything reports whether the scope named every type.
func (s SmartScope) Everything() bool { return s.Type == "" }

// v1Actions are what each v1 word covers.
//
// v1 read is the whole read side, so it carries history: there is no v1 way to
// ask for a history without it, and a client using v1 that could read a
// resource could always read how it got that way.
var v1Actions = map[string][]storage.Action{
	"read":  {storage.ActionRead, storage.ActionSearch, storage.ActionHistory},
	"write": {storage.ActionWrite, storage.ActionDelete},
	"*": {
		storage.ActionRead, storage.ActionSearch, storage.ActionHistory,
		storage.ActionWrite, storage.ActionDelete,
	},
}

// v2Actions are what each v2 letter covers.
//
// c and u are both a write here, because this build does not separate them: a
// create and an update are one authorization. ParseScope refuses a scope naming
// one without the other rather than granting both, which would be a widening.
var v2Actions = map[rune]storage.Action{
	'c': storage.ActionWrite,
	'r': storage.ActionRead,
	'u': storage.ActionWrite,
	'd': storage.ActionDelete,
	's': storage.ActionSearch,
}

// ParseScope reads one scope string.
func ParseScope(stated string) (SmartScope, error) {
	context, rest, found := strings.Cut(strings.TrimSpace(stated), "/")
	if !found {
		return SmartScope{}, fmt.Errorf("%w: %q", ErrMalformedScope, stated)
	}

	held := SmartScope{Context: SmartContext(context)}

	switch held.Context {
	case ContextPatient, ContextUser, ContextSystem:
	default:
		return SmartScope{}, fmt.Errorf("%w: %q names no context", ErrMalformedScope, stated)
	}

	// A v2 scope may carry a search restriction. This build reads the scope it
	// qualifies and refuses the qualification rather than dropping it: a scope
	// silently widened is one the app believes is narrower than it is.
	if before, _, restricted := strings.Cut(rest, "?"); restricted {
		return SmartScope{}, fmt.Errorf(
			"%w: %s restricts by a search, which this build does not apply",
			ErrUnsupportedScope, before)
	}

	resourceType, access, found := strings.Cut(rest, ".")
	if !found || resourceType == "" || access == "" {
		return SmartScope{}, fmt.Errorf("%w: %q", ErrMalformedScope, stated)
	}

	if resourceType != "*" {
		if !fhir.ServesResourceType(resourceType) {
			return SmartScope{}, fmt.Errorf("%w: %s is not a type this server serves",
				ErrUnsupportedScope, resourceType)
		}

		held.Type = storage.ResourceType(resourceType)
	}

	actions, err := parseAccess(access, stated)
	if err != nil {
		return SmartScope{}, err
	}

	held.Actions = actions

	return held, nil
}

// parseAccess resolves the access part of a scope into actions.
func parseAccess(access, stated string) ([]storage.Action, error) {
	if held, known := v1Actions[access]; known {
		return slices.Clone(held), nil
	}

	var (
		held             []storage.Action
		creates, updates bool
	)

	for _, letter := range access {
		action, known := v2Actions[letter]
		if !known {
			return nil, fmt.Errorf("%w: %q", ErrMalformedScope, stated)
		}

		switch letter {
		case 'c':
			creates = true
		case 'u':
			updates = true
		}

		if !slices.Contains(held, action) {
			held = append(held, action)
		}
	}

	// Creating and updating are one authorization here. An app granted only one
	// of them would hold the other, so a scope naming one alone is refused
	// rather than answered with more than it asked for.
	if creates != updates {
		return nil, fmt.Errorf(
			"%w: %s separates creating from updating, and this build does not",
			ErrUnsupportedScope, stated)
	}

	if len(held) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrMalformedScope, stated)
	}

	return held, nil
}

// patientCompartment is the type a patient-context scope confines to.
const patientCompartment = storage.ResourceType("Patient")

// Narrow returns what an app holds: everything the person holds, narrowed to
// what the app was granted and never anything more.
//
// launch is the patient the session was launched for, empty when it was not
// launched in a patient context.
//
// A Grant survives only when some granted scope covers its type and its action.
// Nothing is constructed from the scopes themselves: every Grant that comes out
// of here came in, unchanged or confined further, which is what makes a scope
// string a restriction rather than a privilege.
func Narrow(held storage.Scope, granted []SmartScope, launch storage.LogicalID) storage.Scope {
	if len(granted) == 0 {
		// An app that was granted nothing holds nothing, rather than holding
		// whatever the person does.
		return storage.Scope{}
	}

	var kept []storage.Grant

	for _, grant := range held.Grants() {
		if narrowed, survives := narrowGrant(grant, granted, launch); survives {
			kept = append(kept, narrowed)
		}
	}

	return storage.NewScope(kept...)
}

// narrowGrant answers what one Grant becomes, and whether it survives at all.
func narrowGrant(
	grant storage.Grant, granted []SmartScope, launch storage.LogicalID,
) (storage.Grant, bool) {
	var asIs, confined bool

	for _, scope := range granted {
		if !covers(scope, grant) {
			continue
		}

		switch scope.Context {
		case ContextUser:
			// The person's own reach, narrowed to this type and action and
			// otherwise exactly as their policy left it.
			asIs = true

		case ContextPatient:
			// A scope naming a patient when there is not one describes no set
			// of resources. Not everything, not the person's own — nothing.
			if launch == "" {
				continue
			}

			switch {
			case grant.Compartment == nil:
				confined = true
			case *grant.Compartment == compartmentFor(launch):
				asIs = true
			default:
				// The person is confined to one patient and the app asked for
				// another. Neither is the answer: the app may not have the one
				// it asked for, and must not be handed the one it did not.
				continue
			}

		case ContextSystem:
			// A backend service narrows against its own standing, exactly as a
			// person's app narrows against theirs. The difference is whose
			// standing went in: a client-credentials token's principal is the
			// registration itself, so the Grants here are the client's own.
			//
			// That is why a system scope must never reach a session acting for
			// a person. Nothing in this function could tell the difference —
			// the Grants look the same — so the refusal belongs where the
			// scopes are approved, and the authorization endpoint makes it.
			asIs = true
		}
	}

	switch {
	case asIs:
		return grant, true
	case confined:
		bounded := compartmentFor(launch)
		grant.Compartment = &bounded

		return grant, true
	default:
		return storage.Grant{}, false
	}
}

// covers reports whether one scope names a Grant's type and action.
func covers(scope SmartScope, grant storage.Grant) bool {
	// Platform resources are not FHIR, and no SMART scope names one.
	if grant.Kind != storage.KindFHIR {
		return false
	}

	if !scope.Everything() && scope.Type != grant.Type {
		return false
	}

	return slices.Contains(scope.Actions, grant.Action)
}

// compartmentFor is the compartment a patient-context scope confines to.
func compartmentFor(patient storage.LogicalID) storage.Compartment {
	return storage.Compartment{Type: patientCompartment, ID: patient}
}
