package main

import (
	"fmt"
	"net/url"
	"strconv"

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

	cursor := storage.VersionID(asked.Get(search.CursorParameter))

	stated := asked.Get(search.CountParameter)
	if stated == "" {
		if cursor == "" {
			return storage.DefaultWindow(), nil
		}

		return storage.NewVersionWindow(storage.DefaultVersions, cursor)
	}

	count, err := strconv.Atoi(stated)
	if err != nil {
		return storage.VersionWindow{}, fmt.Errorf("%w: %q is not a count",
			storage.ErrMalformedWindow, stated)
	}

	return storage.NewVersionWindow(count, cursor)
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
