package audit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrMissingProject reports an event outside every Project. Nothing here is
	// recorded about the install as a whole, so a row without a tenant is one no
	// query would be scoped to find.
	ErrMissingProject = errors.New("audit: an event names the Project it happened in")

	// ErrMissingEventID reports an event with no identifier of its own.
	ErrMissingEventID = errors.New("audit: an event needs an identifier")

	// ErrMissingInstant reports an event with no instant. When it happened is
	// half of what an incident asks.
	ErrMissingInstant = errors.New("audit: an event records when it happened")

	// ErrMissingPrincipal reports an event naming nobody. Who acted is the other
	// half, and anonymous is not a value: a request nothing identified is
	// recorded against the unidentified principal, which is a named kind.
	ErrMissingPrincipal = errors.New("audit: an event names who acted")

	// ErrUnknownAction reports an action outside the enum storage declares.
	ErrUnknownAction = errors.New("audit: unknown action")

	// ErrUnknownOutcome reports an outcome outside the enum.
	ErrUnknownOutcome = errors.New("audit: unknown outcome")

	// ErrUnknownReason reports a reason outside the closed vocabulary. It is
	// closed so nothing a caller supplied can reach the record.
	ErrUnknownReason = errors.New("audit: unknown reason")
)

// eventPrefix namespaces an audit identifier, as the principal registries
// namespace theirs.
const eventPrefix = "aud_"

// eventIDBytes is how much randomness an identifier carries.
const eventIDBytes = 16

// Action is what was attempted. It is the audit's own vocabulary rather than
// the authorization one, because this record covers more than what a Grant can
// authorize: proving a credential is not an action any policy permits, and it
// is the first thing an incident asks about.
type Action string

// What an event records an attempt at.
const (
	ActionRead   Action = "read"
	ActionWrite  Action = "write"
	ActionDelete Action = "delete"
	ActionSearch Action = "search"

	// ActionHistory covers reading one past version and listing them alike.
	ActionHistory Action = "history"

	// ActionAuthenticate covers proving a credential. No Grant authorizes it —
	// it is what a Grant is later built from.
	ActionAuthenticate Action = "authenticate"
)

// Recorded maps an authorization action onto the audit's own vocabulary. An
// action outside the enum records nothing rather than a nearby value.
func Recorded(action storage.Action) (Action, bool) {
	switch action {
	case storage.ActionRead:
		return ActionRead, true
	case storage.ActionWrite:
		return ActionWrite, true
	case storage.ActionDelete:
		return ActionDelete, true
	case storage.ActionSearch:
		return ActionSearch, true
	case storage.ActionHistory:
		return ActionHistory, true
	default:
		return "", false
	}
}

// Outcome is what became of the attempt.
type Outcome string

// What an attempt came to.
const (
	// OutcomeAllowed records an interaction that happened.
	OutcomeAllowed Outcome = "allowed"

	// OutcomeRefused records one authorization declined. A refusal is recorded
	// as loudly as a success, because a log holding only what succeeded cannot
	// answer the question an incident asks (AUD-3).
	OutcomeRefused Outcome = "refused"

	// OutcomeFailed records one that broke. It is distinct from a refusal: an
	// incident needs to tell a server fault from a decision.
	OutcomeFailed Outcome = "failed"
)

// Reason is the short, closed vocabulary an event's detail is drawn from.
//
// It is closed rather than free text so nothing a caller supplied can reach the
// record. An audit row that quoted a request would hold the very content AUD-2
// keeps out of it, and a row that quoted a login would hold the password.
type Reason string

// Why an attempt came to what it did.
const (
	// ReasonNone is the absence of a reason, which is what a success carries.
	ReasonNone Reason = ""

	// ReasonNotAuthorized records a Scope that authorized nothing.
	ReasonNotAuthorized Reason = "not-authorized"

	// ReasonNotFound records a resource that is absent or outside the caller's
	// reach, which this server answers alike.
	ReasonNotFound Reason = "not-found"

	// ReasonDeleted records a resource whose current version is a tombstone.
	ReasonDeleted Reason = "deleted"

	// ReasonVersionConflict records a write whose expectation did not hold.
	ReasonVersionConflict Reason = "version-conflict"

	// ReasonAlreadyExists records a create over an id already taken.
	ReasonAlreadyExists Reason = "already-exists"

	// ReasonMalformed records a request this server could not read.
	ReasonMalformed Reason = "malformed"

	// ReasonUnidentified records a request nothing identified.
	ReasonUnidentified Reason = "unidentified"

	// ReasonThrottled records an attempt refused before it was proved.
	ReasonThrottled Reason = "throttled"

	// ReasonUnavailable records a fault in this server.
	ReasonUnavailable Reason = "unavailable"
)

// UnidentifiedPrincipal is who a request nothing identified is recorded
// against. A failed login and an unauthenticated read both happened, and an
// event naming nobody would be one no query could find (AUD-3).
var UnidentifiedPrincipal = project.PrincipalRef{Kind: "unidentified", ID: "unidentified"}

var (
	knownOutcomes = []Outcome{OutcomeAllowed, OutcomeRefused, OutcomeFailed}

	knownReasons = []Reason{
		ReasonNone, ReasonNotAuthorized, ReasonNotFound, ReasonDeleted,
		ReasonVersionConflict, ReasonAlreadyExists, ReasonMalformed,
		ReasonUnidentified, ReasonThrottled, ReasonUnavailable,
	}

	knownActions = []Action{
		ActionRead, ActionWrite, ActionDelete,
		ActionSearch, ActionHistory, ActionAuthenticate,
	}
)

// EventConfig is what one record is built from. There is deliberately no field
// for a credential, a token, a password hash or a resource body: an audit row
// that carried any of them would be a second copy of the thing it exists to
// watch over (AUD-2).
type EventConfig struct {
	Project    project.ID
	ID         storage.LogicalID
	At         time.Time
	Principal  project.PrincipalRef
	Membership project.MembershipID
	Action     Action
	Resource   project.ProfileRef
	Outcome    Outcome
	Reason     Reason
}

// Event is one security-relevant thing that happened. Its fields are unexported
// and set once: a record anything could edit afterwards is not a record.
type Event struct {
	auditProject project.ID
	id           storage.LogicalID
	at           time.Time
	principal    project.PrincipalRef
	membership   project.MembershipID
	action       Action
	resource     project.ProfileRef
	outcome      Outcome
	reason       Reason
}

// NewEvent builds one record. Everything an incident asks of a row is required,
// so a row that cannot answer is refused when it is built rather than found
// empty when it is read.
func NewEvent(cfg EventConfig) (Event, error) {
	if err := project.ValidateID(cfg.Project); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrMissingProject, err)
	}

	if cfg.ID == "" {
		return Event{}, ErrMissingEventID
	}

	if cfg.At.IsZero() {
		return Event{}, ErrMissingInstant
	}

	if cfg.Principal.Kind == "" || cfg.Principal.ID == "" {
		return Event{}, ErrMissingPrincipal
	}

	if !slices.Contains(knownActions, cfg.Action) {
		return Event{}, fmt.Errorf("%w: %q", ErrUnknownAction, string(cfg.Action))
	}

	if !slices.Contains(knownOutcomes, cfg.Outcome) {
		return Event{}, fmt.Errorf("%w: %q", ErrUnknownOutcome, string(cfg.Outcome))
	}

	if !slices.Contains(knownReasons, cfg.Reason) {
		return Event{}, fmt.Errorf("%w: %q", ErrUnknownReason, string(cfg.Reason))
	}

	return Event{
		auditProject: cfg.Project, id: cfg.ID, at: cfg.At.UTC(),
		principal: cfg.Principal, membership: cfg.Membership,
		action: cfg.Action, resource: cfg.Resource,
		outcome: cfg.Outcome, reason: cfg.Reason,
	}, nil
}

// MintEventID draws an identifier in the audit namespace.
func MintEventID(random io.Reader) (storage.LogicalID, error) {
	raw := make([]byte, eventIDBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("audit: cannot mint an event identifier: %w", err)
	}

	return storage.LogicalID(eventPrefix + base64.RawURLEncoding.EncodeToString(raw)), nil
}

// Project returns the Project the event happened in.
func (e Event) Project() project.ID { return e.auditProject }

// ID returns the event's own identifier.
func (e Event) ID() storage.LogicalID { return e.id }

// At returns when it happened, in UTC.
func (e Event) At() time.Time { return e.at }

// Principal returns who acted.
func (e Event) Principal() project.PrincipalRef { return e.principal }

// Membership returns the standing they acted under, empty when none resolved.
func (e Event) Membership() project.MembershipID { return e.membership }

// Action returns what they attempted.
func (e Event) Action() Action { return e.action }

// Resource returns which resource it was about, empty on an event about none.
func (e Event) Resource() project.ProfileRef { return e.resource }

// Outcome returns what became of the attempt.
func (e Event) Outcome() Outcome { return e.outcome }

// Reason returns why, drawn from the closed vocabulary.
func (e Event) Reason() Reason { return e.reason }

// Recorder writes events. It is an interface so the domain states what must be
// recorded without knowing what records it, and so a route can be tested
// against a recorder that only remembers.
type Recorder interface {
	// Record writes one event. It joins whatever transaction the context
	// carries, which is what makes an event atomic with the thing it describes
	// (AUD-1).
	Record(ctx context.Context, event Event) error
}
