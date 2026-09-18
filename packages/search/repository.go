package search

import (
	"context"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// Page is what one search returns.
//
// Total is present only when it was computed, which is what Counted says: an
// absent total and a wrong one are very different promises, and a Bundle that
// always carried a number would be making the wrong one (SRC-2).
type Page struct {
	Records []storage.ResourceRecord

	// More reports whether a further page exists, which is the only thing a
	// next link may be built from. A link offered when nothing follows it is a
	// promise the next request breaks.
	More bool

	Total   int
	Counted bool
}

// Cursor returns where the next page resumes from, empty when none follows.
func (p Page) Cursor() storage.LogicalID {
	if !p.More || len(p.Records) == 0 {
		return ""
	}

	return p.Records[len(p.Records)-1].Key.ID
}

// Repository executes a plan. It takes the Scope every other read takes, so a
// search is bounded by the same authorization decision a by-key read is and
// adds parameters to it rather than replacing it (SRC-3).
type Repository interface {
	Search(ctx context.Context, scope storage.Scope, query Query) (Page, error)
}
