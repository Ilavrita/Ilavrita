package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// A conditional interaction names its target by a search rather than an id.
//
// It is how a client stays idempotent without holding the ids this server
// minted: "create this unless one already matches", "update the one that
// matches". Without it a pipeline that retried a create would make a second
// record of the same thing, and nothing downstream could tell which was meant.
//
// All three resolve the same way, and the way matters: the search runs under
// the caller's own Scope. A condition that matched a resource they cannot read
// would let them act on it — and learn it exists — through a door the read
// route does not open.

var (
	// errAmbiguousCondition reports a condition matching more than one
	// resource. R4 answers 412 for this: the client asked about a resource and
	// there are several, so nothing can be done that is what they meant.
	errAmbiguousCondition = errors.New(
		"ilavrita: that condition matches more than one resource")

	// errUnconditioned reports a conditional route reached with no condition at
	// all, which would otherwise act on every resource of a type.
	errUnconditioned = errors.New(
		"ilavrita: a conditional interaction states a search that narrows it")
)

// matchedByCondition runs a condition and returns what it matched.
//
// It reads one more than it needs. Knowing whether a condition is ambiguous
// takes two matches, and counting the rest of a type that may be large answers
// a question nobody asked.
func matchedByCondition(
	request *core.RequestEvent, asked url.Values,
) ([]storage.ResourceRecord, error) {
	if len(asked) == 0 {
		return nil, errUnconditioned
	}

	// Searching is its own action. A caller who may write a type and not search
	// it cannot name one by a condition, because naming it is reading it.
	held, err := permit(request, storage.ResourceType(
		request.Request.PathValue(resourceTypeParameter)), searchActions)
	if err != nil {
		return nil, err
	}

	plan, err := search.Parse(held.custom, held.resourceType, asked)
	if err != nil {
		return nil, err
	}

	page, err := held.searches.Search(request.Request.Context(), held.scope, plan.Narrowed(2))
	if err != nil {
		return nil, err
	}

	if len(page.Records) > 1 {
		return nil, errAmbiguousCondition
	}

	return page.Records, nil
}

// conditionOf reads the search a conditional interaction states.
//
// A create states it in a header, because its own URL names the type and
// nothing else. An update and a delete state it in the query of the type they
// address.
func conditionOf(request *core.RequestEvent, header bool) (url.Values, error) {
	if !header {
		return request.Request.URL.Query(), nil
	}

	stated := request.Request.Header.Get(ifNoneExistField)
	if stated == "" {
		return nil, nil
	}

	asked, err := url.ParseQuery(stated)
	if err != nil {
		return nil, unreadableBody
	}

	return asked, nil
}

// createConditionally performs a create unless the condition already matches.
//
// A match is answered with what is already there and nothing is written, which
// is the whole point: the client asked for the resource to exist, and it does.
func createConditionally(
	request *core.RequestEvent, held granted, asked url.Values,
) (handled bool, err error) {
	matched, err := matchedByCondition(request, asked)
	if err != nil {
		return true, err
	}

	if len(matched) == 0 {
		return false, nil
	}

	// 200 rather than 201: nothing was created. The Location names what is
	// there, so a client that cannot tell the difference from the status can
	// read it from the header it would have read anyway.
	base, err := baseURL(request)
	if err != nil {
		return true, err
	}

	record := matched[0]

	request.Response.Header().Set(locationField,
		versionedLocation(base, record.Key, record.Version))

	return true, respondResource(request, http.StatusOK, record)
}

// updateConditionally replaces the one resource a condition matches.
//
// Nothing matching is a create, which is what R4 says and what this server's
// unconditional update does as well: a client stating the whole resource and
// where it belongs has said everything a create needs.
func updateConditionally(request *core.RequestEvent) error {
	held, err := begin(request, writeActions)
	if err != nil {
		return refuse(request, err)
	}

	asked, err := conditionOf(request, false)
	if err != nil {
		return refuse(request, err)
	}

	matched, err := matchedByCondition(request, asked)
	if err != nil {
		return refuse(request, err)
	}

	fields, err := submittedResource(request, held.resourceType)
	if err != nil {
		return refuse(request, err)
	}

	if len(matched) == 0 {
		return createMatchingNothing(request, held, fields)
	}

	key := matched[0].Key

	// The body may not name a different resource than the one the condition
	// found. Preferring either would store something nobody asked for.
	if err := checkSubmittedID(fields, key.ID); err != nil {
		return refuse(request, err)
	}

	content, err := storedResource(fields, key.ID)
	if err != nil {
		return refuse(request, err)
	}

	record, err := held.replace(request.Request.Context(), key, content, "")
	if err != nil {
		return refuse(request, err)
	}

	return respondResource(request, http.StatusOK, record)
}

// createMatchingNothing is the create a conditional update falls back to.
func createMatchingNothing(
	request *core.RequestEvent, held granted, fields map[string]json.RawMessage,
) error {
	key, content, err := held.minted(fields)
	if err != nil {
		return refuse(request, err)
	}

	return createResourceAt(request, held, key, content)
}

// deleteConditionally removes the one resource a condition matches.
//
// Nothing matching is answered as done. R4 allows either that or 404, and done
// is what the client asked for: they wanted no resource matching, and there is
// none. A retry after a successful delete is the ordinary case.
func deleteConditionally(request *core.RequestEvent) error {
	held, err := begin(request, deleteActions)
	if err != nil {
		return refuse(request, err)
	}

	asked, err := conditionOf(request, false)
	if err != nil {
		return refuse(request, err)
	}

	matched, err := matchedByCondition(request, asked)
	if err != nil {
		return refuse(request, err)
	}

	if len(matched) == 0 {
		request.Response.WriteHeader(http.StatusNoContent)

		return nil
	}

	if err := held.remove(request.Request.Context(), matched[0].Key, precondition{}); err != nil {
		return refuse(request, err)
	}

	request.Response.WriteHeader(http.StatusNoContent)

	return nil
}
