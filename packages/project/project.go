package project

import (
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// ID is storage.ProjectID by alias, not a second identifier type. Two project
// id types would need a conversion at the storage boundary, and a conversion is
// where the wrong project gets passed.
type ID = storage.ProjectID

// ValidateID rejects the empty id and the wildcard. There is no sentinel
// meaning "every project": the first code that reads one as a wildcard is the
// cross-project leak.
func ValidateID(id ID) error {
	if id == "" || id == "*" {
		return fmt.Errorf("%w: %q", ErrInvalidProjectID, id)
	}

	return nil
}

// OrgUnit labels where a Project sits in an operator's organisation. It holds a
// name and not an ID, so no code can walk it into another Project's data; reach
// between Projects comes only from a Link.
type OrgUnit string

// Kind separates the single Super Project from every ordinary Project. It is
// fixed when the Project is created and never changes.
type Kind string

// The kinds a Project can be.
const (
	KindStandard Kind = "standard"
	KindSuper    Kind = "super"
)

// Valid reports whether the kind is one this server recognises.
func (k Kind) Valid() bool {
	return k == KindStandard || k == KindSuper
}

// AllowsSuperAdmin reports whether a membership in a Project of this kind may
// hold super admin. Only the Super Project may.
func (k Kind) AllowsSuperAdmin() bool {
	return k == KindSuper
}

// State is a Project's lifecycle position. It is a precondition checked at
// request admission, before any query is built, never a row predicate that gets
// filtered out after fetching.
type State string

// The lifecycle states of a Project.
const (
	StateActive    State = "active"
	StateSuspended State = "suspended"
	StateArchived  State = "archived"
	StateDeleting  State = "deleting"
)

// Valid reports whether the state is one this server recognises.
func (s State) Valid() bool {
	switch s {
	case StateActive, StateSuspended, StateArchived, StateDeleting:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. Deleting is
// terminal: the purge worker has started and nothing reverses it.
func (s State) CanTransitionTo(next State) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case StateActive:
		return next == StateSuspended || next == StateArchived || next == StateDeleting
	case StateSuspended:
		return next == StateActive || next == StateArchived || next == StateDeleting
	case StateArchived:
		return next == StateActive || next == StateDeleting
	default:
		return false
	}
}

// TransitionTo returns the next state, or refuses the move. An unrecognised
// current state fails closed rather than being read as the nearest known one.
func (s State) TransitionTo(next State) (State, error) {
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}

	if !s.CanTransitionTo(next) {
		return "", fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s, next)
	}

	return next, nil
}

// Admits reports whether the lifecycle permits an action at all. Suspended and
// deleting admit nothing; archived keeps reads and refuses every write.
func (s State) Admits(action storage.Action) bool {
	switch s {
	case StateActive:
		return true
	case StateArchived:
		return action == storage.ActionRead || action == storage.ActionSearch || action == storage.ActionHistory
	default:
		return false
	}
}
