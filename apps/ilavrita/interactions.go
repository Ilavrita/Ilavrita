package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// The actions each interaction is authorized for. A write also asks for read:
// the store refuses a create the caller could not have read back, and update
// and delete must learn the current version before they may name it.
var (
	readActions    = []storage.Action{storage.ActionRead}
	historyActions = []storage.Action{storage.ActionHistory}
	writeActions   = []storage.Action{storage.ActionRead, storage.ActionWrite}
	deleteActions  = []storage.Action{storage.ActionRead, storage.ActionDelete}
)

// createResource mints a logical id and stores the submitted resource under it.
// A body carrying its own id is refused: on this route the server assigns them.
func createResource(request *core.RequestEvent) error {
	// R4 makes a create conditional with this header. Nothing here performs a
	// conditional interaction, and a header quietly dropped is a client that
	// asked for "only if this is not already here", was given an unconditional
	// create, and is told nothing about the duplicate it now holds.
	if request.Request.Header.Get(ifNoneExistField) != "" {
		return refuse(request, conditionalUnavailable)
	}

	if carriesRawPayload(request) {
		return createPayload(request)
	}

	held, err := begin(request, writeActions)
	if err != nil {
		return refuse(request, err)
	}

	fields, err := submittedResource(request, held.resourceType)
	if err != nil {
		return refuse(request, err)
	}

	if _, carried := fields[idField]; carried {
		return refuse(request, assignedID)
	}

	key, content, err := held.minted(fields)
	if err != nil {
		return refuse(request, err)
	}

	return createResourceAt(request, held, key, content)
}

// mintedKey assigns the logical id the route owns, for a body that is not
// encoded here.
func (g granted) mintedKey() (storage.ResourceKey, error) {
	id, err := mintLogicalID()
	if err != nil {
		return storage.ResourceKey{}, err
	}

	return storage.NewResourceKey(g.project, g.resourceType, id)
}

// minted assigns the logical id the route owns and encodes the body under it.
func (g granted) minted(fields map[string]json.RawMessage) (storage.ResourceKey, json.RawMessage, error) {
	id, err := mintLogicalID()
	if err != nil {
		return storage.ResourceKey{}, nil, err
	}

	key, err := storage.NewResourceKey(g.project, g.resourceType, id)
	if err != nil {
		return storage.ResourceKey{}, nil, err
	}

	content, err := storedResource(fields, id)
	if err != nil {
		return storage.ResourceKey{}, nil, err
	}

	return key, content, nil
}

// readResource returns the current version. An absent resource and one this
// Scope cannot see are the same answer, so an id cannot be probed for.
func readResource(request *core.RequestEvent) error {
	held, err := begin(request, readActions)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.addressed(request)
	if err != nil {
		return refuse(request, err)
	}

	record, err := held.resources.Read(request.Request.Context(), held.scope, key)
	if errors.Is(err, storage.ErrNotFound) {
		return readCanonical(request, held, key, err)
	}

	if err != nil {
		return refuse(request, err)
	}

	return respondPossiblePayload(request, held, record)
}

// readCanonical answers from the FHIR base definitions when the Project holds no
// resource of that id.
//
// The Project's own store is asked first, so a Project that wrote its own
// StructureDefinition for a type serves that one. What is behind this is the
// specification: the same bytes in every Project, belonging to none of them.
//
// It is answered only for a caller whose Grant narrows nothing. A canonical
// resource belongs to no Project, so there is no row for a compartment, a filter
// or a projection to be evaluated against, and a Grant carrying one cannot be
// said to admit it. Asking that is the whole point: the Project's store returns
// ErrNotFound both for a row that is not there and for one this caller may not
// see — an id must not be probeable — so deciding from that answer would be
// deciding from an inference that is wrong exactly when it matters.
//
// The original failure is carried, so a caller who is not admitted, and a
// deployment holding no definitions, both answer the 404 they always did.
func readCanonical(
	request *core.RequestEvent, held granted, key storage.ResourceKey, absent error,
) error {
	if serving == nil || serving.definitions == nil {
		return refuse(request, absent)
	}

	if !held.scope.Admits(held.project, storage.KindFHIR, key.Type, storage.ActionRead) {
		return refuse(request, absent)
	}

	record, found, err := serving.definitions.Read(request.Request.Context(), key.Type, key.ID)
	if err != nil {
		return refuse(request, err)
	}

	if !found {
		return refuse(request, absent)
	}

	record.Key = key

	return respondResource(request, http.StatusOK, record)
}

// respondPossiblePayload answers a read with whatever the caller asked for: a
// Binary's own bytes when the Accept names them, and the resource otherwise.
func respondPossiblePayload(
	request *core.RequestEvent, held granted, record storage.ResourceRecord,
) error {
	if record.Key.Type != binaryType {
		return respondResource(request, http.StatusOK, record)
	}

	served, err := servePayload(request, held, record)
	if err != nil {
		return refuse(request, err)
	}

	if served {
		return nil
	}

	embedded, err := embedPayload(request.Request.Context(), held, record)
	if err != nil {
		return refuse(request, err)
	}

	return respondResource(request, http.StatusOK, embedded)
}

// readResourceVersion returns one immutable version. It authorizes under
// history, never read: a read Grant may name the compartment the resource is in
// today, which an older version need not share.
func readResourceVersion(request *core.RequestEvent) error {
	held, err := begin(request, historyActions)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.addressed(request)
	if err != nil {
		return refuse(request, err)
	}

	version, err := addressedVersion(request)
	if err != nil {
		return refuse(request, err)
	}

	// The store matched the stored version id against the URL's own, so the
	// record carries that literal and the ETag echoes what was asked for.
	record, err := held.versions.ReadVersion(request.Request.Context(), held.scope, key, version)
	if err != nil {
		return refuse(request, err)
	}

	return respondPossiblePayload(request, held, record)
}

// updateResource replaces a resource, or brings the id the client named into
// existence. Which storage call ran is the whole difference between 200 and
// 201; nothing else decides it.
func updateResource(request *core.RequestEvent) error {
	held, err := begin(request, writeActions)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.addressed(request)
	if err != nil {
		return refuse(request, err)
	}

	content, err := replacementBody(request, held, key)
	if err != nil {
		return refuse(request, err)
	}

	claim, err := claimedPrecondition(request)
	if err != nil {
		return refuse(request, err)
	}

	current, err := held.resources.Read(request.Request.Context(), held.scope, key)

	switch {
	case err == nil:
		if claim.disagrees(current.Version) {
			return refuse(request, staleVersion)
		}

		return replaceResource(request, held, key, content, claim.expected())
	case errors.Is(err, storage.ErrDeleted), errors.Is(err, storage.ErrNotFound):
		// Any claim against a resource this Scope sees as deleted or absent can
		// never be corroborated, "*" included: there is no representation to
		// match, so RFC 9110 13.1.1 refuses the method rather than creating.
		if claim.stated() {
			return refuse(request, staleVersion)
		}

		return createResourceAt(request, held, key, content)
	default:
		return refuse(request, err)
	}
}

// replacementBody reads the submitted resource and refuses one that disagrees
// with the URL it was sent to, before any storage call.
func replacementBody(
	request *core.RequestEvent,
	held granted,
	key storage.ResourceKey,
) (json.RawMessage, error) {
	fields, err := submittedResource(request, held.resourceType)
	if err != nil {
		return nil, err
	}

	if err := checkSubmittedID(fields, key.ID); err != nil {
		return nil, err
	}

	return storedResource(fields, key.ID)
}

// replaceResource writes over a live resource. A caller that named a version
// gets it enforced by the same statement that writes; one that named none is
// answered last-write-wins rather than with a race it never opted into.
func replaceResource(
	request *core.RequestEvent,
	held granted,
	key storage.ResourceKey,
	content json.RawMessage,
	expect storage.VersionID,
) error {
	record, err := held.replace(request.Request.Context(), key, content, expect)
	if err != nil {
		return refuse(request, err)
	}

	return respondResource(request, http.StatusOK, record)
}

// replace states a new version of one resource, and is the whole of what an
// update is apart from answering. It is separate from the route for the same
// reason create is: a Bundle performs the same act.
func (g granted) replace(
	ctx context.Context, key storage.ResourceKey, content json.RawMessage, expect storage.VersionID,
) (storage.ResourceRecord, error) {
	// Asked before the body is, because no body could be right. A caller who can
	// only ever see part of a resource cannot state the whole of one, and
	// telling them an element is missing would send them to invent the content
	// that was withheld from them and be refused again for the real reason.
	if g.scope.Withholds(key.Project, storage.KindFHIR, key.Type, storage.ActionRead) {
		return storage.ResourceRecord{}, sqlite.ErrPartialView
	}

	compartments, err := fhir.Compartments(string(key.Type), key.ID, content)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSubmission(g.custom, key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSubscription(g.custom, key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSearchParameter(key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	row, carried, err := splitPayload(key, content)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	return g.written(ctx, key, func(ctx context.Context) error {
		return g.resources.Update(ctx, g.scope, storage.ResourceRecord{
			Key: key, Content: row, Compartments: compartments,
		}, expect)
	}, g.afterWrite(key, carried))
}

// createResourceAt brings a logical id into existence, from nothing or over a
// tombstone. It serves both the minted id and the one a client named.
func createResourceAt(
	request *core.RequestEvent,
	held granted,
	key storage.ResourceKey,
	content json.RawMessage,
) error {
	record, err := held.create(request.Request.Context(), key, content)
	if err != nil {
		return refuse(request, err)
	}

	return respondCreated(request, record)
}

// create brings one resource into existence, and is the whole of what a create
// is apart from answering.
//
// It is separate from the route so a Bundle can perform the same act without an
// HTTP response to write into. One implementation: a second one written beside
// it would eventually differ about what a create checks, and the difference
// would be a resource a transaction stored that a POST would have refused.
func (g granted) create(
	ctx context.Context, key storage.ResourceKey, content json.RawMessage,
) (storage.ResourceRecord, error) {
	compartments, err := fhir.Compartments(string(key.Type), key.ID, content)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSubmission(g.custom, key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSubscription(g.custom, key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := checkSearchParameter(key, content); err != nil {
		return storage.ResourceRecord{}, err
	}

	row, carried, err := splitPayload(key, content)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	return g.written(ctx, key, func(ctx context.Context) error {
		return g.resources.Create(ctx, g.scope, storage.ResourceRecord{
			Key: key, Content: row, Compartments: compartments,
		})
	}, g.afterWrite(key, carried))
}

// written performs one write and reads the row back inside a single
// transaction. A read-back this Scope cannot satisfy rolls the write away, so a
// client is never told a committed row does not exist.
func (g granted) written(
	ctx context.Context,
	key storage.ResourceKey,
	write func(ctx context.Context) error,
	after func(ctx context.Context, record storage.ResourceRecord) error,
) (storage.ResourceRecord, error) {
	var written storage.ResourceRecord

	// A boundary outside this one keeps its own slot; this is for the write
	// that is its own boundary, which is what an unaudited interaction is.
	slot, listening := ctx.Value(placedKey{}).(*placedPayloads)
	if !listening {
		slot = &placedPayloads{}
		ctx = context.WithValue(ctx, placedKey{}, slot)
	}

	err := g.transactions.WithinTransaction(ctx, func(ctx context.Context) error {
		// The audit is written by the decorator around this interaction, and a
		// create mints an id no URL carries. This is the one thing it cannot
		// read off the request.
		settle(ctx, key)

		if err := write(ctx); err != nil {
			return err
		}

		record, err := g.resources.Read(ctx, g.scope, key)
		if err != nil {
			return err
		}

		written = record

		// A Binary's bytes are written here: the version the row settled on is
		// what names them, and it is not known until the row exists. Inside the
		// transaction, so bytes that cannot be written take the row with them
		// rather than leaving a resource describing a document nobody has.
		if after == nil {
			return nil
		}

		return after(ctx, record)
	})
	if err != nil {
		discardPlaced(ctx, g.payloads, slot)

		return storage.ResourceRecord{}, err
	}

	return written, nil
}

// deleteResource makes the resource a tombstone. Reads of the id afterwards
// report it as deleted, which is the read rule rather than a second one.
func deleteResource(request *core.RequestEvent) error {
	held, err := begin(request, deleteActions)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.addressed(request)
	if err != nil {
		return refuse(request, err)
	}

	claim, err := claimedPrecondition(request)
	if err != nil {
		return refuse(request, err)
	}

	if err := held.remove(request.Request.Context(), key, claim); err != nil {
		return refuse(request, err)
	}

	return request.NoContent(http.StatusNoContent)
}

// remove makes one resource a tombstone, and is the whole of what a delete is
// apart from answering. It is separate from the route for the same reason create
// and replace are: a Bundle performs the same act.
func (g granted) remove(ctx context.Context, key storage.ResourceKey, claim precondition) error {
	current, err := g.resources.Read(ctx, g.scope, key)
	if err != nil {
		// A precondition stated against a resource this caller cannot read is
		// a stale one: answering "no such resource" would say whether it exists.
		if claim.stated() {
			return staleVersion
		}

		return err
	}

	if claim.disagrees(current.Version) {
		return staleVersion
	}

	return g.resources.Delete(ctx, g.scope, key, claim.expected())
}

// listResourceHistory returns every version this Scope can see, newest first.
// A resource whose history ends in a deletion still has a history, so this
// route never reports one as gone.
func listResourceHistory(request *core.RequestEvent) error {
	held, err := begin(request, historyActions)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.addressed(request)
	if err != nil {
		return refuse(request, err)
	}

	window, err := requestedWindow(request)
	if err != nil {
		return refuse(request, err)
	}

	page, err := held.versions.ListVersions(request.Request.Context(), held.scope, key, window)
	if err != nil {
		return refuse(request, err)
	}

	base, err := baseURL(request)
	if err != nil {
		return refuse(request, err)
	}

	asked := request.Request.URL.Query()

	bundle, err := historyBundle(base, page,
		historyURL(base, key, asked, window.Before), nextHistoryURL(base, key, asked, page))
	if err != nil {
		return refuse(request, err)
	}

	return respondBody(request, http.StatusOK, bundle)
}
