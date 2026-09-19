package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// searchActions is what a search authorizes under. It is its own action, not a
// read: a Scope may let a clinician read any chart they are handed the id of
// and search only their own patients, and compiling one into the other would
// answer the wider question.
var searchActions = []storage.Action{storage.ActionSearch}

// The most a form-encoded search body may carry. A body is parsed before
// anything is authorized, so it is bounded before it is read.
const maxSearchBody = 64 << 10

// errUnreadableSearchBody reports a POSTed search this server could not read.
var errUnreadableSearchBody = errors.New("ilavrita: the search body could not be read")

// searchResources answers a search stated in the query string.
func searchResources(request *core.RequestEvent) error {
	held, err := begin(request, searchActions)
	if err != nil {
		return refuse(request, err)
	}

	return answerGranted(request, held, request.Request.URL.Query())
}

// searchResourcesByPost answers the same search stated in a form-encoded body.
//
// It is the same interaction as the GET: R4 offers it so a query too long or
// too sensitive for a URL is not one a client has to shorten, and the two
// resolve identically because they share everything below this line (SRC-1).
func searchResourcesByPost(request *core.RequestEvent) error {
	held, err := beginReading(request, searchActions, searchBodyMediaTypes)
	if err != nil {
		return refuse(request, err)
	}

	asked, err := postedQuery(request)
	if err != nil {
		return refuse(request, err)
	}

	return answerGranted(request, held, asked)
}

// postedQuery reads the form-encoded body R4 names for this route. Parameters
// in the URL are read too, because a client may split them and dropping either
// half would answer a query nobody asked.
func postedQuery(request *core.RequestEvent) (url.Values, error) {
	body, err := io.ReadAll(io.LimitReader(request.Request.Body, maxSearchBody+1))
	if err != nil || len(body) > maxSearchBody {
		return nil, errUnreadableSearchBody
	}

	posted, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, errUnreadableSearchBody
	}

	for name, values := range request.Request.URL.Query() {
		posted[name] = append(posted[name], values...)
	}

	return posted, nil
}

// answerGranted is where both forms of the interaction meet. Everything below
// this line is shared, which is what makes the two resolve identically (SRC-1).
func answerGranted(request *core.RequestEvent, held granted, asked url.Values) error {
	base, err := baseURL(request)
	if err != nil {
		return refuse(request, err)
	}

	plan, err := search.Parse(held.custom, held.resourceType, asked)
	if err != nil {
		return refuse(request, err)
	}

	page, err := held.searches.Search(request.Request.Context(), held.scope, plan)
	if err != nil {
		return refuse(request, err)
	}

	bundle, err := searchBundle(base, held.resourceType, asked, page)
	if err != nil {
		return refuse(request, err)
	}

	return respondFHIR(request, http.StatusOK, bundle)
}

// searchBundle renders one page. Every entry is a match: nothing is included
// alongside one, because _include is not implemented and an entry nobody asked
// for would be indistinguishable from one they did.
func searchBundle(
	base string, resourceType storage.ResourceType, asked url.Values, page search.Page,
) (fhir.Bundle, error) {
	entries := make([]fhir.BundleEntry, 0, len(page.Records))

	for _, record := range page.Records {
		rendered, err := renderedResource(record)
		if err != nil {
			return fhir.Bundle{}, err
		}

		entries = append(entries, fhir.MatchEntry(resourceURL(base, record.Key), rendered))
	}

	return fhir.NewSearchBundle(fhir.SearchsetConfig{
		Entries: entries,
		SelfURL: searchURL(base, resourceType, asked, ""),
		NextURL: nextURL(base, resourceType, asked, page),
		Total:   page.Total,
		Counted: page.Counted,
	}), nil
}

// nextURL names the page after this one, and nothing when none follows.
func nextURL(
	base string, resourceType storage.ResourceType, asked url.Values, page search.Page,
) string {
	cursor := page.Cursor()
	if cursor == "" {
		return ""
	}

	return searchURL(base, resourceType, asked, cursor)
}

// searchURL rebuilds one search as a URL. The cursor is replaced rather than
// appended, so following a next link repeatedly does not accumulate them.
func searchURL(
	base string, resourceType storage.ResourceType, asked url.Values, cursor storage.LogicalID,
) string {
	carried := url.Values{}

	for name, values := range asked {
		if name == search.CursorParameter {
			continue
		}

		carried[name] = append([]string(nil), values...)
	}

	if cursor != "" {
		carried.Set(search.CursorParameter, string(cursor))
	}

	href := base + "/" + string(resourceType)
	if encoded := carried.Encode(); encoded != "" {
		href += "?" + encoded
	}

	return href
}
