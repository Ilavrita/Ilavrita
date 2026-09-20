package storage

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// How much of a resource's history one page carries.
//
// A resource that has been written to for years has a history no client asked to
// receive in one response, and this server holds one pooled connection per
// process — so an unbounded read is not only a large answer, it is every other
// request in that process waiting behind it.
const (
	DefaultVersions = 20
	MaxVersions     = 200
)

// ErrMalformedWindow reports a page nobody could serve: a count outside what
// this server answers, or a cursor that is not a version.
var ErrMalformedWindow = errors.New("storage: that is not a page of history this server answers")

// VersionWindow is which page of a resource's history to read.
//
// Before is where the page resumes, exclusive, and empty on the newest page.
// History is ordered newest first, so resuming means reading below it.
type VersionWindow struct {
	Count  int
	Before VersionID

	// Since narrows a history to what changed after a moment. It is the _since
	// parameter R4 names, and the zero time is a caller who did not ask.
	Since time.Time
}

// From returns the window narrowed to what changed after a moment.
func (w VersionWindow) From(since time.Time) VersionWindow {
	w.Since = since

	return w
}

// NewVersionWindow reads a window a caller asked for, and refuses one this
// server does not answer rather than quietly serving a different page.
//
// A count of zero is a caller asking for nothing, which is refused. Asking for
// no particular count is a different request and is spelled by leaving it out —
// which is what DefaultWindow answers.
func NewVersionWindow(count int, before VersionID) (VersionWindow, error) {
	if count < 1 || count > MaxVersions {
		return VersionWindow{}, fmt.Errorf("%w: a count is between 1 and %d",
			ErrMalformedWindow, MaxVersions)
	}

	if before != "" {
		if _, err := before.Sequence(); err != nil {
			return VersionWindow{}, err
		}
	}

	return VersionWindow{Count: count, Before: before}, nil
}

// DefaultWindow is the newest page of a history, for a request that named no
// count. It is what bounds an install whose clients never page at all.
func DefaultWindow() VersionWindow {
	return VersionWindow{Count: DefaultVersions}
}

// Sequence reads the ordering a version id carries. Every version this server
// mints is the decimal of its own sequence, which is what lets a client resume
// from one it was given.
func (v VersionID) Sequence() (int64, error) {
	held, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil || held < 1 {
		return 0, fmt.Errorf("%w: %q is not a version", ErrMalformedWindow, string(v))
	}

	return held, nil
}

// VersionPage is what one window answered.
type VersionPage struct {
	Records []ResourceRecord

	// More reports whether a further page exists, which is the only thing a next
	// link may be built from. A link offered when nothing follows it is a promise
	// the next request breaks.
	More bool

	// First reports whether this is the newest page, which is what makes a total
	// knowable: a page that is both first and last holds the whole history, and
	// any other page holds part of one nobody counted.
	First bool
}

// Cursor returns where the next page resumes from, empty when none follows.
func (p VersionPage) Cursor() VersionID {
	if !p.More || len(p.Records) == 0 {
		return ""
	}

	return p.Records[len(p.Records)-1].Version
}

// Total returns how many versions there are, and whether that is known.
//
// It is known only for a history that fits in one page. Counting the rest would
// be a second query over the same rows, and a total that was guessed is worse
// than one that is absent: a client can handle a missing total, and cannot
// handle a wrong one.
func (p VersionPage) Total() (int, bool) {
	if !p.First || p.More {
		return 0, false
	}

	return len(p.Records), true
}
