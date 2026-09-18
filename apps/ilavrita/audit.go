package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

var (
	// errInteractionRefused rolls back an interaction this server did not allow.
	// A handler may write and then refuse — a read-back the caller's own Scope
	// cannot satisfy is the case — so the answer decides the transaction rather
	// than the handler's return, which is nil once a refusal has been rendered.
	errInteractionRefused = errors.New("ilavrita: the interaction was not allowed")

	// errAuditUnavailable reports that what happened could not be recorded. The
	// interaction is undone and refused: an action this server cannot account
	// for afterwards is one it must not take.
	errAuditUnavailable = errors.New("ilavrita: this interaction cannot be recorded")
)

// auditedAs names the action each interaction is recorded under. An interaction
// missing from here is not audited, which is why a test checks this table
// against the served one rather than trusting either to be complete.
var auditedAs = map[fhir.Interaction]audit.Action{
	fhir.InteractionCreate:          audit.ActionWrite,
	fhir.InteractionRead:            audit.ActionRead,
	fhir.InteractionUpdate:          audit.ActionWrite,
	fhir.InteractionDelete:          audit.ActionDelete,
	fhir.InteractionInstanceHistory: audit.ActionHistory,
	fhir.InteractionVersionRead:     audit.ActionHistory,
	fhir.InteractionSearchType:      audit.ActionSearch,
}

// answered is what each status means in the audit's own vocabulary. It is keyed
// by the status the client was given, so the record can never disagree with
// what this server actually said.
var answered = map[int]struct {
	outcome audit.Outcome
	reason  audit.Reason
}{
	http.StatusOK:                   {audit.OutcomeAllowed, audit.ReasonNone},
	http.StatusCreated:              {audit.OutcomeAllowed, audit.ReasonNone},
	http.StatusNoContent:            {audit.OutcomeAllowed, audit.ReasonNone},
	http.StatusUnauthorized:         {audit.OutcomeRefused, audit.ReasonUnidentified},
	http.StatusForbidden:            {audit.OutcomeRefused, audit.ReasonNotAuthorized},
	http.StatusNotFound:             {audit.OutcomeRefused, audit.ReasonNotFound},
	http.StatusConflict:             {audit.OutcomeRefused, audit.ReasonAlreadyExists},
	http.StatusGone:                 {audit.OutcomeRefused, audit.ReasonDeleted},
	http.StatusPreconditionFailed:   {audit.OutcomeRefused, audit.ReasonVersionConflict},
	http.StatusTooManyRequests:      {audit.OutcomeRefused, audit.ReasonThrottled},
	http.StatusBadRequest:           {audit.OutcomeRefused, audit.ReasonMalformed},
	http.StatusMethodNotAllowed:     {audit.OutcomeRefused, audit.ReasonMalformed},
	http.StatusNotAcceptable:        {audit.OutcomeRefused, audit.ReasonMalformed},
	http.StatusUnsupportedMediaType: {audit.OutcomeRefused, audit.ReasonMalformed},
	http.StatusUnprocessableEntity:  {audit.OutcomeRefused, audit.ReasonMalformed},
}

// classify reads one answer. A status nobody classified is recorded as a fault
// rather than guessed at, for the reason translate treats an unrecognised error
// as an internal failure.
func classify(status int) (audit.Outcome, audit.Reason) {
	if known, found := answered[status]; found {
		return known.outcome, known.reason
	}

	return audit.OutcomeFailed, audit.ReasonUnavailable
}

// heldResponse buffers what a handler wrote, so nothing reaches the client
// until the record of it has committed. A client told its write succeeded, by a
// process that then failed to record that write, has been told something this
// server cannot stand behind.
type heldResponse struct {
	held   http.Header
	body   bytes.Buffer
	status int
	fixed  bool
}

func newHeldResponse() *heldResponse {
	return &heldResponse{held: http.Header{}, status: http.StatusOK}
}

func (h *heldResponse) Header() http.Header { return h.held }

// WriteHeader keeps the first status, as net/http does: a handler that writes a
// body after refusing must not overwrite the refusal.
func (h *heldResponse) WriteHeader(status int) {
	if h.fixed {
		return
	}

	h.status, h.fixed = status, true
}

func (h *heldResponse) Write(body []byte) (int, error) {
	h.fixed = true

	return h.body.Write(body)
}

// flushTo copies the held answer onto the writer the client is reading.
func (h *heldResponse) flushTo(response http.ResponseWriter) error {
	for name, values := range h.held {
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}

	response.WriteHeader(h.status)

	_, err := response.Write(h.body.Bytes())

	return err
}

// settledKey is where an interaction reports the resource it settled on, for
// the one case the URL does not name it: a create mints its own id, and an
// audit row that could not say which resource was created would answer nothing
// an incident asks.
type settledKey struct{}

// settled is the slot a write fills in.
type settled struct{ key storage.ResourceKey }

// settle records the resource an interaction acted on, if something is
// listening. Nothing is listening when the interaction is not audited.
func settle(ctx context.Context, key storage.ResourceKey) {
	if slot, listening := ctx.Value(settledKey{}).(*settled); listening {
		slot.key = key
	}
}

// identify answers who is asking, for the record rather than for a decision. A
// request nothing identified still happened, so it is recorded against the
// unidentified principal instead of not at all (AUD-3).
//
// It answers from the session the decorator already resolved, which is also
// what the handler beneath it reads: the token is looked up once per request
// and the record, the decision and the standing all speak of the same session.
func identify(held resolvedSession) (caller, project.MembershipID) {
	if held.err != nil || !held.found {
		return caller{project: project.SystemScope, principal: audit.UnidentifiedPrincipal}, ""
	}

	return caller{
		project: held.session.Project(), principal: held.session.Principal(),
	}, held.session.Membership()
}

// record writes one event through whatever transaction the context carries.
func (b *backend) record(ctx context.Context, event audit.EventConfig) error {
	if b.audits == nil {
		return nil
	}

	id, err := audit.MintEventID(rand.Reader)
	if err != nil {
		return err
	}

	event.ID = id
	event.At = time.Now().UTC()

	built, err := audit.NewEvent(event)
	if err != nil {
		return err
	}

	return b.audits.Record(ctx, built)
}

// audited wraps one interaction so that what happened is recorded exactly once.
//
// It is the only place a FHIR interaction is audited. A handler recording its
// own outcome would be a rule every new interaction has to remember, and the
// one that forgot would be the one an incident asks about.
//
// The record is written inside the transaction the interaction ran in, so a
// write cannot commit without it (AUD-1), and the answer is held back until
// that transaction commits. An interaction this server did not allow rolls that
// transaction back and is recorded on its own afterwards, because a refusal
// that vanished with the rollback would leave only successes in the log (AUD-3).
func audited(
	interaction fhir.Interaction, handler func(*core.RequestEvent) error,
) func(*core.RequestEvent) error {
	action, audits := auditedAs[interaction]

	return func(request *core.RequestEvent) error {
		if !audits || serving == nil || serving.audits == nil {
			return handler(request)
		}

		return serving.recordInteraction(request, action, handler)
	}
}

func (b *backend) recordInteraction(
	request *core.RequestEvent,
	action audit.Action,
	handler func(*core.RequestEvent) error,
) error {
	held := newHeldResponse()
	answer, asked := request.Response, request.Request

	request.Response = held

	defer func() { request.Response, request.Request = answer, asked }()

	presented := b.lookUpSession(request)
	who, membership := identify(presented)
	slot := &settled{}

	// The session is carried down so the handler reads the one already looked
	// up: a second lookup could answer differently from the one this record
	// names, and the two would disagree about who acted.
	asked = asked.WithContext(context.WithValue(asked.Context(), sessionKey{}, presented))

	commit := b.resources.WithinTransaction(asked.Context(), func(ctx context.Context) error {
		request.Request = asked.WithContext(context.WithValue(ctx, settledKey{}, slot))

		if err := handler(request); err != nil {
			return err
		}

		outcome, reason := classify(held.status)
		if outcome != audit.OutcomeAllowed {
			return errInteractionRefused
		}

		return b.record(ctx, b.eventFor(request, who, membership, slot, action, outcome, reason))
	})

	switch {
	case commit == nil:
		return held.flushTo(answer)
	case errors.Is(commit, errInteractionRefused):
		outcome, reason := classify(held.status)

		// Nothing committed, so this record stands alone. A refusal this server
		// could not write down is reported rather than turned into a different
		// answer: the caller was already told no, and saying something else now
		// would hide the refusal as well as losing it.
		if err := b.record(asked.Context(),
			b.eventFor(request, who, membership, slot, action, outcome, reason)); err != nil {
			report(err)
		}

		return held.flushTo(answer)
	default:
		report(commit)

		// The held answer is discarded along with the interaction it described,
		// so the refusal is written to the client's own writer rather than into
		// a buffer nothing will flush.
		request.Response = answer

		return refuse(request, errAuditUnavailable)
	}
}

// eventFor describes what happened. The resource is the one the interaction
// settled on when it named one, and the URL's own otherwise, so a create is
// recorded against the id it minted rather than against nothing.
func (b *backend) eventFor(
	request *core.RequestEvent,
	who caller,
	membership project.MembershipID,
	slot *settled,
	action audit.Action,
	outcome audit.Outcome,
	reason audit.Reason,
) audit.EventConfig {
	resource := project.ProfileRef{
		Type: storage.ResourceType(request.Request.PathValue(resourceTypeParameter)),
		ID:   storage.LogicalID(request.Request.PathValue(idParameter)),
	}

	if slot.key.Type != "" && slot.key.ID != "" {
		resource = project.ProfileRef{Type: slot.key.Type, ID: slot.key.ID}
	}

	// A search names a type and no id: what it acted on is the type, and a row
	// that dropped it would say less than what happened.
	if resource.Type == "" {
		resource = project.ProfileRef{}
	}

	return audit.EventConfig{
		Project: who.project, Principal: who.principal, Membership: membership,
		Action: action, Resource: resource, Outcome: outcome, Reason: reason,
	}
}
