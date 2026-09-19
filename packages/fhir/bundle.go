package fhir

import (
	"encoding/json"
	"slices"
)

// BundleType names what a Bundle collects.
type BundleType string

// The Bundle types Ilavrita builds.
const (
	// BundleHistory collects the versions of one resource.
	BundleHistory BundleType = "history"

	// BundleSearchset collects the resources one search matched.
	BundleSearchset BundleType = "searchset"
)

// HTTPVerb is the interaction one Bundle entry records.
type HTTPVerb string

// The verbs a history entry can record.
const (
	VerbPost   HTTPVerb = "POST"
	VerbPut    HTTPVerb = "PUT"
	VerbDelete HTTPVerb = "DELETE"
)

// Bundle is a collection of entries.
//
// Total is a pointer because a searchset carries one only when it was computed:
// an absent total and a wrong one are very different promises, and a field that
// always serialised would make the wrong one on every page.
type Bundle struct {
	ResourceType string        `json:"resourceType"`
	Type         BundleType    `json:"type"`
	Total        *int          `json:"total,omitempty"`
	Link         []BundleLink  `json:"link,omitempty"`
	Entry        []BundleEntry `json:"entry,omitempty"`
}

// BundleLink points at this page or the one after it.
type BundleLink struct {
	Relation string `json:"relation"`
	URL      string `json:"url"`
}

// EntrySearch records why an entry is in a searchset. Every entry Ilavrita
// returns is a match: nothing is included alongside one, because _include is
// not implemented and an entry nobody asked for would be indistinguishable.
type EntrySearch struct {
	Mode string `json:"mode"`
}

// BundleEntry is one entry. Resource is absent exactly when a history entry
// records a deletion, which carries no body.
//
// Request, Response and Search are pointers because they belong to different
// Bundle types: a history entry records the interaction that produced it, and a
// search entry records why it matched. An entry carrying the other's fields as
// empty objects would be describing something nobody did.
type BundleEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource,omitempty"`
	Search   *EntrySearch    `json:"search,omitempty"`
	Request  *EntryRequest   `json:"request,omitempty"`
	Response *EntryResponse  `json:"response,omitempty"`
}

// EntryRequest is one entry's interaction: the one that produced a version, in
// a history Bundle, or the one a transaction entry asks for.
type EntryRequest struct {
	Method HTTPVerb `json:"method"`
	URL    string   `json:"url"`

	// The preconditions R4 lets a transaction entry state. This server honours
	// none of them, and reads them only so an entry stating one can be refused:
	// a client that asked for ifNoneExist and was quietly given a duplicate has
	// no way to find out it did not get what it asked for.
	//
	// A history Bundle never sets them, so omitempty leaves what it answers
	// with unchanged.
	IfMatch         string `json:"ifMatch,omitempty"`
	IfNoneExist     string `json:"ifNoneExist,omitempty"`
	IfNoneMatch     string `json:"ifNoneMatch,omitempty"`
	IfModifiedSince string `json:"ifModifiedSince,omitempty"`
}

// Precondition names the precondition an entry states, or the empty string if
// it states none.
func (r EntryRequest) Precondition() string {
	for _, stated := range []struct {
		name, value string
	}{
		{"ifMatch", r.IfMatch},
		{"ifNoneExist", r.IfNoneExist},
		{"ifNoneMatch", r.IfNoneMatch},
		{"ifModifiedSince", r.IfModifiedSince},
	} {
		if stated.value != "" {
			return stated.name
		}
	}

	return ""
}

// EntryResponse records what that interaction answered.
type EntryResponse struct {
	Status       string `json:"status"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
}

// HistoryConfig is what one page of a resource's history states about itself.
type HistoryConfig struct {
	Entries []BundleEntry

	// SelfURL is the request that produced this page, and NextURL the one that
	// produces the page after it. Next is empty when nothing follows: a link
	// offered when nothing does is a promise the next request breaks.
	SelfURL string
	NextURL string

	// Total is carried only when Counted says it was computed, which is only
	// when the whole history fits in one page. A history longer than that would
	// need a second query to count, and a total that was guessed is worse than
	// one that is absent: a client can handle a missing total and cannot handle
	// a wrong one.
	Total   int
	Counted bool
}

// NewHistoryBundle collects one page of a resource's versions. The entries keep
// the order they arrive in, which is newest first.
func NewHistoryBundle(config HistoryConfig) Bundle {
	bundle := Bundle{
		ResourceType: "Bundle",
		Type:         BundleHistory,
		Entry:        slices.Clone(config.Entries),
	}

	if config.Counted {
		total := config.Total
		bundle.Total = &total
	}

	if config.SelfURL != "" {
		bundle.Link = append(bundle.Link, BundleLink{Relation: "self", URL: config.SelfURL})
	}

	if config.NextURL != "" {
		bundle.Link = append(bundle.Link, BundleLink{Relation: "next", URL: config.NextURL})
	}

	return bundle
}

// SearchsetConfig is what one page of search results states about itself.
type SearchsetConfig struct {
	Entries []BundleEntry

	// SelfURL is the search that produced this page, and NextURL the one that
	// produces the page after it. Next is empty when nothing follows: a link
	// offered when nothing does is a promise the next request breaks.
	SelfURL string
	NextURL string

	// Total is carried only when Counted says it was computed.
	Total   int
	Counted bool
}

// NewSearchBundle collects one page of matches.
func NewSearchBundle(config SearchsetConfig) Bundle {
	bundle := Bundle{
		ResourceType: "Bundle",
		Type:         BundleSearchset,
		Entry:        slices.Clone(config.Entries),
	}

	if config.Counted {
		total := config.Total
		bundle.Total = &total
	}

	if config.SelfURL != "" {
		bundle.Link = append(bundle.Link, BundleLink{Relation: "self", URL: config.SelfURL})
	}

	if config.NextURL != "" {
		bundle.Link = append(bundle.Link, BundleLink{Relation: "next", URL: config.NextURL})
	}

	return bundle
}

// MatchEntry is one resource a search returned.
func MatchEntry(fullURL string, resource json.RawMessage) BundleEntry {
	return BundleEntry{
		FullURL:  fullURL,
		Resource: resource,
		Search:   &EntrySearch{Mode: "match"},
	}
}
