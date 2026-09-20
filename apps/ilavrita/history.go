package main

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// requestedWindow reads which page of a history the caller asked for.
//
// It takes the same parameter names a search takes, because a client paging one
// collection and paging another should not have to learn two spellings. A count
// this server does not answer is refused rather than rounded down: a caller who
// asked for a thousand and silently got two hundred cannot tell the difference
// between a short page and the end of the history.
func requestedWindow(request *core.RequestEvent) (storage.VersionWindow, error) {
	asked := request.Request.URL.Query()

	since, err := requestedSince(asked)
	if err != nil {
		return storage.VersionWindow{}, err
	}

	cursor := storage.VersionID(asked.Get(search.CursorParameter))

	stated := asked.Get(search.CountParameter)
	if stated == "" {
		if cursor == "" {
			return storage.DefaultWindow().From(since), nil
		}

		held, err := storage.NewVersionWindow(storage.DefaultVersions, cursor)

		return held.From(since), err
	}

	count, err := strconv.Atoi(stated)
	if err != nil {
		return storage.VersionWindow{}, fmt.Errorf("%w: %q is not a count",
			storage.ErrMalformedWindow, stated)
	}

	held, err := storage.NewVersionWindow(count, cursor)

	return held.From(since), err
}

// historyParameters are the ones a history answers. Everything else is refused
// rather than ignored: a caller who narrowed a history and was handed the whole
// of it has no way to tell, and will read it as the answer to what they asked.
var historyParameters = []string{
	search.CountParameter, search.CursorParameter, sinceParameter,
}

// The history parameters R4 names, and what this build does with each.
const (
	// sinceParameter narrows to what changed after a moment.
	sinceParameter = "_since"

	// atParameter narrows to what a resource was at a moment, and listParameter
	// narrows to the members of a List. Neither is implemented, and both are
	// named here so they are refused by name rather than as unknown.
	atParameter   = "_at"
	listParameter = "_list"
)

// requestedSince reads _since, and refuses the narrowings this build does not
// perform.
func requestedSince(asked url.Values) (time.Time, error) {
	for _, named := range []string{atParameter, listParameter} {
		if asked.Has(named) {
			return time.Time{}, fmt.Errorf("%w: %s narrows a history in a way this "+
				"server does not", storage.ErrMalformedWindow, named)
		}
	}

	for named := range asked {
		if !slices.Contains(historyParameters, named) {
			return time.Time{}, fmt.Errorf("%w: %s is not a parameter a history answers",
				storage.ErrMalformedWindow, named)
		}
	}

	stated := asked.Get(sinceParameter)
	if stated == "" {
		return time.Time{}, nil
	}

	moment, err := time.Parse(time.RFC3339, stated)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s is an instant, and %q is not one",
			storage.ErrMalformedWindow, sinceParameter, stated)
	}

	return moment, nil
}

// historyURL rebuilds one history request as a URL.
//
// The cursor is replaced rather than appended, so following a next link
// repeatedly does not accumulate them.
func historyURL(
	base string, key storage.ResourceKey, asked url.Values, cursor storage.VersionID,
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

	href := base + "/" + string(key.Type) + "/" + string(key.ID) + "/_history"
	if encoded := carried.Encode(); encoded != "" {
		href += "?" + encoded
	}

	return href
}

// nextHistoryURL names the page after this one, and nothing when none follows.
func nextHistoryURL(
	base string, key storage.ResourceKey, asked url.Values, page storage.VersionPage,
) string {
	cursor := page.Cursor()
	if cursor == "" {
		return ""
	}

	return historyURL(base, key, asked, cursor)
}
