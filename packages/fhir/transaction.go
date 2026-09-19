package fhir

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// BundleTransaction and BundleBatch are the two ways a client submits several
// interactions in one request.
const (
	// BundleTransaction is all of them or none. An entry that fails takes every
	// other entry with it, which is what makes it worth using: a Patient and
	// their Observations either all exist or none do.
	BundleTransaction BundleType = "transaction"

	// BundleTransactionResponse is what one answers with.
	BundleTransactionResponse BundleType = "transaction-response"
)

var (
	// ErrNotABundle reports a body that is not a Bundle this server processes.
	ErrNotABundle = errors.New("fhir: that is not a Bundle this server processes")

	// ErrMalformedEntry reports an entry naming no request, or one this server
	// cannot act on.
	ErrMalformedEntry = errors.New("fhir: that Bundle entry names no interaction")

	// ErrDuplicateFullURL reports two entries claiming one identity. A reference
	// to it would name both, and nothing could say which was meant.
	ErrDuplicateFullURL = errors.New("fhir: two entries claim the same fullUrl")

	// ErrPreconditionNotSupported reports an entry stating a precondition this
	// server does not honour. Dropping it silently would answer a conditional
	// write with an unconditional one and say nothing about the difference.
	ErrPreconditionNotSupported = errors.New(
		"fhir: this server performs no conditional interaction")
)

// SubmittedBundle is a transaction or batch as it arrived.
type SubmittedBundle struct {
	ResourceType string           `json:"resourceType"`
	Type         BundleType       `json:"type"`
	Entry        []SubmittedEntry `json:"entry"`
	Extra        map[string]any   `json:"-"`
}

// SubmittedEntry is one interaction within it.
type SubmittedEntry struct {
	FullURL  string          `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
	Request  *EntryRequest   `json:"request"`
}

// ReadBundle reads a submitted Bundle, refusing one this server will not act on.
func ReadBundle(body []byte) (SubmittedBundle, error) {
	var held SubmittedBundle
	if err := json.Unmarshal(body, &held); err != nil {
		return SubmittedBundle{}, fmt.Errorf("%w: %w", ErrNotABundle, err)
	}

	if held.ResourceType != "Bundle" {
		return SubmittedBundle{}, fmt.Errorf("%w: it is a %s", ErrNotABundle, held.ResourceType)
	}

	if held.Type != BundleTransaction {
		return SubmittedBundle{}, fmt.Errorf(
			"%w: this server processes a %s, and that is a %s",
			ErrNotABundle, BundleTransaction, held.Type)
	}

	seen := map[string]bool{}

	for index, entry := range held.Entry {
		if entry.Request == nil || entry.Request.Method == "" || entry.Request.URL == "" {
			return SubmittedBundle{}, fmt.Errorf("%w: entry %d", ErrMalformedEntry, index)
		}

		if stated := entry.Request.Precondition(); stated != "" {
			return SubmittedBundle{}, fmt.Errorf(
				"%w: entry %d states %s", ErrPreconditionNotSupported, index, stated)
		}

		if entry.FullURL == "" {
			continue
		}

		if seen[entry.FullURL] {
			return SubmittedBundle{}, fmt.Errorf("%w: %s", ErrDuplicateFullURL, entry.FullURL)
		}

		seen[entry.FullURL] = true
	}

	return held, nil
}

// Ordered returns the entries in the order R4 says a transaction performs them:
// deletes, then creates, then updates, then reads.
//
// The order is what lets one transaction delete a resource and create another
// under the same identity, and what lets a read at the end see everything the
// same transaction wrote. Within a group the client's own order is kept, because
// nothing else would be predictable.
func (b SubmittedBundle) Ordered() []int {
	rank := map[HTTPVerb]int{VerbDelete: 0, VerbPost: 1, VerbPut: 2}

	order := make([]int, 0, len(b.Entry))
	for index := range b.Entry {
		order = append(order, index)
	}

	slices.SortStableFunc(order, func(x, y int) int {
		return placeOf(rank, b.Entry[x]) - placeOf(rank, b.Entry[y])
	})

	return order
}

// placeOf ranks one entry. Anything this server does not write — a read — runs
// last, so it sees what the transaction did.
func placeOf(rank map[HTTPVerb]int, entry SubmittedEntry) int {
	if held, known := rank[entry.Request.Method]; known {
		return held
	}

	return len(rank)
}

// placeholderPrefix is what R4 gives a resource a transaction is creating, so
// other entries can reference it before it has an identity.
const placeholderPrefix = "urn:uuid:"

// IsPlaceholder reports whether one identity is a transaction-local one.
func IsPlaceholder(held string) bool { return strings.HasPrefix(held, placeholderPrefix) }

// Resolved rewrites every transaction-local reference to the identity the server
// assigned.
//
// A transaction states a Patient and an Observation about them at once, and the
// Observation cannot name an id nobody has minted yet — so it names the
// placeholder, and this is what turns that into the reference a reader follows.
//
// A placeholder nothing in the Bundle claims is left alone. It may be a
// reference to something outside this transaction, and rewriting it to nothing
// would quietly break a link the client meant.
func Resolved(content json.RawMessage, assigned map[string]string) (json.RawMessage, bool, error) {
	if len(assigned) == 0 || len(content) == 0 {
		return content, false, nil
	}

	var held any
	if err := json.Unmarshal(content, &held); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrNotABundle, err)
	}

	rewritten := rewrite(held, assigned)

	if !rewritten {
		return content, false, nil
	}

	encoded, err := json.Marshal(held)
	if err != nil {
		return nil, false, fmt.Errorf("fhir: encode a resolved entry: %w", err)
	}

	return encoded, true, nil
}

// rewrite replaces placeholders wherever a reference carries one.
//
// It rewrites the "reference" member and nothing else. A placeholder elsewhere
// in a resource is a string that happens to look like one, and a rewriter that
// replaced every occurrence would edit content nobody asked it to.
func rewrite(held any, assigned map[string]string) bool {
	switch value := held.(type) {
	case map[string]any:
		changed := false

		if named, isString := value["reference"].(string); isString {
			if resolved, known := assigned[named]; known {
				value["reference"] = resolved
				changed = true
			}
		}

		for _, member := range value {
			changed = rewrite(member, assigned) || changed
		}

		return changed

	case []any:
		changed := false
		for _, member := range value {
			changed = rewrite(member, assigned) || changed
		}

		return changed
	}

	return false
}

// NewTransactionResponse collects what each entry answered, in the order the
// client submitted them.
//
// R4 requires the response to line up with the request entry for entry, whatever
// order the server performed them in — a client reads the third response as the
// answer to its third request.
func NewTransactionResponse(entries []BundleEntry) Bundle {
	return Bundle{
		ResourceType: "Bundle",
		Type:         BundleTransactionResponse,
		Entry:        slices.Clone(entries),
	}
}
