package storage

import "slices"

// Action is the operation a Scope authorizes. A Scope authorizes one Action; a
// different Action requires a new authorization decision.
type Action string

// Actions a Scope can authorize.
const (
	ActionRead    Action = "read"
	ActionWrite   Action = "write"
	ActionDelete  Action = "delete"
	ActionSearch  Action = "search"
	ActionHistory Action = "history"
)

// Kind separates FHIR resources from platform resources. They live in separate
// tables so a forgotten predicate cannot cross between them.
type Kind string

// Kinds of resource, stored in separate tables.
const (
	KindFHIR     Kind = "fhir"
	KindPlatform Kind = "platform"
)

// GrantSource records why a Grant exists so an audit can tell a member's own
// Project from one reached through a link.
type GrantSource string

// Reasons a Grant exists.
const (
	SourceMembership GrantSource = "membership"
	SourceLink       GrantSource = "link"
	SourceSuperAdmin GrantSource = "super-admin"
)

// Compartment restricts a Grant to resources attached to one subject, such as a
// patient who may read only their own chart.
type Compartment struct {
	Type ResourceType
	ID   LogicalID
}

// Grant authorizes exactly one Action on one resource type in one Project.
// Carrying the type and action prevents a Scope issued for one from being reused
// for another, which is how an _include silently widens a read.
//
// Compartment, Filter and Projection each narrow it and none can widen it:
// absent means unrestricted in that dimension, so a Grant nobody narrowed
// reaches every resource of its type and returns all of it. A backend that
// cannot apply one of them must refuse the Grant, because ignoring a
// restriction is the same as widening it.
//
// The three narrow different things. Compartment and Filter decide which
// resources the Grant reaches; Projection decides how much of one it returns.
type Grant struct {
	Project     ProjectID
	Kind        Kind
	Type        ResourceType
	Action      Action
	Source      GrantSource
	Compartment *Compartment
	Filter      *Filter
	Projection  *Projection
}

// Scope is an authorization decision: every Grant a principal holds for one
// request. The zero value grants nothing, and storage can never widen it.
type Scope struct {
	grants []Grant
}

// NewScope builds a Scope. Only the authorization layer should call it; storage
// consumes Scopes and never constructs one.
func NewScope(grants ...Grant) Scope {
	return Scope{grants: slices.Clone(grants)}
}

// Grants returns a copy, so a caller holding a Scope cannot append to it.
func (s Scope) Grants() []Grant {
	return slices.Clone(s.grants)
}

// IsEmpty reports whether the Scope authorizes nothing.
func (s Scope) IsEmpty() bool {
	return len(s.grants) == 0
}

// Projects lists the Projects this Scope can reach, for building the query
// predicate. An empty result must compile to a query that matches no rows.
func (s Scope) Projects() []ProjectID {
	seen := make([]ProjectID, 0, len(s.grants))
	for _, grant := range s.grants {
		if !slices.Contains(seen, grant.Project) {
			seen = append(seen, grant.Project)
		}
	}

	return seen
}

// Allows reports whether the Scope authorizes this exact operation.
func (s Scope) Allows(project ProjectID, kind Kind, resourceType ResourceType, action Action) bool {
	return slices.ContainsFunc(s.grants, func(g Grant) bool {
		return g.Project == project && g.Kind == kind && g.Type == resourceType && g.Action == action
	})
}

// Admits reports whether the Scope authorizes this operation over every resource
// of the type, with nothing narrowed.
//
// It is a different question from Allows, and a much narrower one. Allows
// answers whether a decision was authorized at all; this answers whether the
// authorization is unconditional — no compartment to be in, no filter to pass,
// no elements withheld — so a caller holding it may be handed any resource of
// that type without anything further being checked.
//
// It exists for the one case where storage has no row to decide against: a
// resource that belongs to no Project, which no Grant's narrowing can be
// evaluated over. Answering such a read from an unrestricted Grant is a
// decision; answering it because a query found nothing is an inference, and a
// wrong one whenever "found nothing" also means "may not see it".
func (s Scope) Admits(project ProjectID, kind Kind, resourceType ResourceType, action Action) bool {
	return slices.ContainsFunc(s.grants, func(g Grant) bool {
		return g.Project == project && g.Kind == kind &&
			g.Type == resourceType && g.Action == action &&
			g.Compartment == nil && g.Filter == nil && g.Projection == nil
	})
}

// Withholds reports whether every Grant this Scope holds for one operation
// narrows what comes back.
//
// It is what says a caller can only ever see part of a resource of this type,
// and therefore cannot state the whole of one: a replace from such a caller
// would delete whatever was withheld from them, silently and on their behalf.
//
// A Scope holding no Grant for the operation withholds nothing, because there is
// nothing for it to withhold. Whether the caller may perform it at all is a
// different question, and Allows is the one that answers it.
func (s Scope) Withholds(project ProjectID, kind Kind, resourceType ResourceType, action Action) bool {
	held := make([]*Projection, 0, len(s.grants))

	for _, grant := range s.grants {
		if grant.Project == project && grant.Kind == kind &&
			grant.Type == resourceType && grant.Action == action {
			held = append(held, grant.Projection)
		}
	}

	return len(held) > 0 && WidestProjection(held) != nil
}

// Narrow returns the Grants matching one kind and action. It can only remove
// Grants, never add them, which is what keeps a Scope from widening downstream.
func (s Scope) Narrow(kind Kind, action Action) Scope {
	kept := make([]Grant, 0, len(s.grants))
	for _, grant := range s.grants {
		if grant.Kind == kind && grant.Action == action {
			kept = append(kept, grant)
		}
	}

	return Scope{grants: kept}
}
