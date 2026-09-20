package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// The headers a client reads with when it already has a copy.
const (
	ifNoneMatchField     = "If-None-Match"
	ifModifiedSinceField = "If-Modified-Since"
)

// unchangedSince answers a read whose caller already holds the current version.
//
// A client polling a resource it has read before is asking "has this changed",
// and answering the whole resource says yes whatever the truth is. 304 says no,
// carrying the validators it was asked about and no body.
//
// If-None-Match is decided first and alone when it is present, which is what
// RFC 9110 requires: a version id is exact, and a timestamp is a second
// question nobody asked once the first was answered.
func unchangedSince(request *core.RequestEvent, record storage.ResourceRecord) bool {
	if request.Request.Method != http.MethodGet && request.Request.Method != http.MethodHead {
		return false
	}

	if asked := request.Request.Header.Get(ifNoneMatchField); asked != "" {
		return matchesAnyTag(asked, weakETag(record.Version))
	}

	asked := request.Request.Header.Get(ifModifiedSinceField)
	if asked == "" {
		return false
	}

	since, err := http.ParseTime(asked)
	if err != nil {
		// A malformed date is a header this server cannot act on. RFC 9110 says
		// to ignore it, which here means answering the resource: the caller
		// gets more than they asked for rather than less.
		return false
	}

	// HTTP dates carry seconds, and a record's instant carries milliseconds, so
	// the comparison is made at the resolution the header has. Otherwise a
	// resource written at .500 looks newer than the second it was written in,
	// every time, and nothing is ever unchanged.
	return !record.LastUpdated.Truncate(time.Second).After(since)
}

// matchesAnyTag reports whether an If-None-Match header covers one entity tag.
//
// The comparison is weak, which is what R4's versioning means: two responses
// with the same version id are the same resource, whether or not the bytes were
// serialised identically.
func matchesAnyTag(header, tag string) bool {
	for _, held := range strings.Split(header, ",") {
		held = strings.TrimSpace(held)

		if held == "*" || weakly(held) == weakly(tag) {
			return true
		}
	}

	return false
}

// weakly strips the weakness marker, so W/"1" and "1" compare as the same tag.
func weakly(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), weakPrefix)
}

// respondUnchanged answers 304 with the validators and no body.
func respondUnchanged(request *core.RequestEvent, record storage.ResourceRecord) error {
	request.Response.Header().Set(etagField, weakETag(record.Version))
	request.Response.Header().Set(lastModifiedField, httpDate(record.LastUpdated))
	request.Response.WriteHeader(http.StatusNotModified)

	return nil
}

// preferField is how a client says what it wants back from a write.
const preferField = "Prefer"

// The returns R4 defines, and what each means here.
const (
	// returnMinimal is a client saying it does not want the resource back. It
	// already has what it sent, and the headers carry the version.
	returnMinimal = "minimal"

	// returnRepresentation is the whole resource as stored, which is what this
	// server answers when nothing was asked for.
	returnRepresentation = "representation"

	// returnOutcome is an OperationOutcome describing the write, which is what
	// a client wanting the warnings but not the resource asks for.
	returnOutcome = "OperationOutcome"
)

// preferredReturn reads what a client asked a write to answer with.
//
// A preference this server does not recognise is not an error: R4 says Prefer
// is a preference and a server may ignore one, so an unknown value falls back
// to what it would have answered anyway. That is the one place this build does
// not refuse what it cannot honour, because the header itself says so.
func preferredReturn(request *core.RequestEvent) string {
	for _, held := range strings.Split(request.Request.Header.Get(preferField), ",") {
		name, value, found := strings.Cut(strings.TrimSpace(held), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "return") {
			continue
		}

		switch value = strings.TrimSpace(value); {
		case strings.EqualFold(value, returnMinimal):
			return returnMinimal
		case strings.EqualFold(value, returnRepresentation):
			return returnRepresentation
		case strings.EqualFold(value, returnOutcome):
			return returnOutcome
		}
	}

	return returnRepresentation
}
