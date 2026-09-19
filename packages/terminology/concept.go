// Package terminology reads code systems a deployment supplies for itself.
//
// Nothing here is bundled. LOINC and SNOMED CT are licensed — SNOMED per
// country and per affiliate, LOINC on its own terms — so a release is something
// an install loads from its own copy, never something this project
// redistributes. What this package holds is the reading of those releases and
// nothing of their content.
package terminology

import (
	"errors"
	"fmt"
	"strings"
)

// The systems this build can read a release of, by the canonical url FHIR names
// them under.
const (
	LOINC  = "http://loinc.org"
	SNOMED = "http://snomed.info/sct"
)

var (
	// ErrUnreadableRelease reports a file that is not the release it was said to
	// be. It is refused rather than partly read: half a code system loaded is
	// one that answers "no such code" for everything it did not reach.
	ErrUnreadableRelease = errors.New("terminology: that is not a release this build can read")

	// ErrUnknownSystem reports a system this build has no reader for.
	ErrUnknownSystem = errors.New("terminology: no reader for that code system")
)

// Concept is one code and what it means.
//
// Display is the name a directory shows. Inactive concepts are read and kept:
// a code that was retired is still one a stored resource may carry, and a
// lookup that could not resolve it would leave that resource unreadable.
type Concept struct {
	Code    string
	Display string
	Active  bool
}

// Valid reports whether a concept names a code at all.
func (c Concept) Valid() bool { return strings.TrimSpace(c.Code) != "" }

// Release is one code system's published release, read as a stream.
//
// A LOINC table is tens of megabytes and a SNOMED release is hundreds, so
// nothing here holds one: concepts are handed over as they are read, and
// whatever stores them decides how much to keep in memory at once.
type Release interface {
	// System is the canonical url of the code system this release is of.
	System() string

	// Version is what the release calls itself, which is what an install records
	// so an operator can tell which one is loaded.
	Version() string

	// Read hands over every concept in the release, stopping at the first error
	// the receiver returns.
	Read(receive func(Concept) error) error
}

// Describe names a release for a log line or a job record.
func Describe(release Release) string {
	return fmt.Sprintf("%s %s", release.System(), release.Version())
}
