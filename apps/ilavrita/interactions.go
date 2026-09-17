package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/storage"
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
	if err != nil {
		return refuse(request, err)
	}

	return respondResource(request, http.StatusOK, record)
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

	return respondResource(request, http.StatusOK, record)
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
	record, err := held.written(request.Request.Context(), key, func(ctx context.Context) error {
		return held.resources.Update(ctx, held.scope, storage.ResourceRecord{Key: key, Content: content}, expect)
	})
	if err != nil {
		return refuse(request, err)
	}

	return respondResource(request, http.StatusOK, record)
}

// createResourceAt brings a logical id into existence, from nothing or over a
// tombstone. It serves both the minted id and the one a client named.
func createResourceAt(
	request *core.RequestEvent,
	held granted,
	key storage.ResourceKey,
	content json.RawMessage,
) error {
	record, err := held.written(request.Request.Context(), key, func(ctx context.Context) error {
		return held.resources.Create(ctx, held.scope, storage.ResourceRecord{Key: key, Content: content})
	})
	if err != nil {
		return refuse(request, err)
	}

	return respondCreated(request, record)
}

// written performs one write and reads the row back inside a single
// transaction. A read-back this Scope cannot satisfy rolls the write away, so a
// client is never told a committed row does not exist.
func (g granted) written(
	ctx context.Context,
	key storage.ResourceKey,
	write func(ctx context.Context) error,
) (storage.ResourceRecord, error) {
	var written storage.ResourceRecord

	err := g.transactions.WithinTransaction(ctx, func(ctx context.Context) error {
		if err := write(ctx); err != nil {
			return err
		}

		record, err := g.resources.Read(ctx, g.scope, key)
		written = record

		return err
	})

	return written, err
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

	current, err := held.resources.Read(request.Request.Context(), held.scope, key)
	if err != nil {
		if claim.stated() {
			return refuse(request, staleVersion)
		}

		return refuse(request, err)
	}

	if claim.disagrees(current.Version) {
		return refuse(request, staleVersion)
	}

	if err := held.resources.Delete(request.Request.Context(), held.scope, key, claim.expected()); err != nil {
		return refuse(request, err)
	}

	return request.NoContent(http.StatusNoContent)
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

	records, err := held.versions.ListVersions(request.Request.Context(), held.scope, key)
	if err != nil {
		return refuse(request, err)
	}

	base, err := baseURL(request)
	if err != nil {
		return refuse(request, err)
	}

	bundle, err := historyBundle(base, records)
	if err != nil {
		return refuse(request, err)
	}

	return respondBody(request, http.StatusOK, bundle)
}
