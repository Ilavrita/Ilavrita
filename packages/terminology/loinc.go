package terminology

import (
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The columns a LOINC table release carries that a directory needs. They are
// found by name rather than by position, because the release adds columns
// between versions and a reader counting from the left would silently read the
// wrong one.
const (
	loincCode    = "LOINC_NUM"
	loincDisplay = "LONG_COMMON_NAME"
	loincShort   = "SHORTNAME"
	loincStatus  = "STATUS"
)

// loincActive is the one status LOINC calls current. Everything else —
// DEPRECATED, DISCOURAGED, TRIAL — is read and kept as inactive, because a code
// a resource already carries has to stay resolvable.
const loincActive = "ACTIVE"

// LOINCRelease reads a LOINC table release from its CSV.
type LOINCRelease struct {
	held    io.Reader
	version string
}

var _ Release = (*LOINCRelease)(nil)

// NewLOINCRelease binds a reader to one release. The version is what the
// operator says it is: the CSV does not carry one, and an install that could not
// say which release it holds is one nobody can reason about.
func NewLOINCRelease(held io.Reader, version string) *LOINCRelease {
	return &LOINCRelease{held: held, version: version}
}

// System is LOINC's canonical url.
func (r *LOINCRelease) System() string { return LOINC }

// Version is the release the operator named.
func (r *LOINCRelease) Version() string { return r.version }

// Read hands over every concept in the table.
func (r *LOINCRelease) Read(receive func(Concept) error) error {
	reader := csv.NewReader(r.held)

	// The release has columns this build does not read, and a row that is short
	// or long is the release changing shape rather than this row being wrong.
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = true

	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadableRelease, err)
	}

	at := map[string]int{}
	for index, name := range header {
		at[strings.TrimSpace(strings.Trim(name, `"`))] = index
	}

	for _, needed := range []string{loincCode, loincStatus} {
		if _, found := at[needed]; !found {
			return fmt.Errorf("%w: it names no %s column", ErrUnreadableRelease, needed)
		}
	}

	for {
		row, err := reader.Read()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnreadableRelease, err)
		}

		concept := loincConcept(row, at)
		if !concept.Valid() {
			continue
		}

		if err := receive(concept); err != nil {
			return err
		}
	}
}

// loincConcept reads one row.
func loincConcept(row []string, at map[string]int) Concept {
	held := func(name string) string {
		index, found := at[name]
		if !found || index >= len(row) {
			return ""
		}

		return strings.TrimSpace(row[index])
	}

	display := held(loincDisplay)
	if display == "" {
		// A release that carries no long name still names the code somehow, and
		// a directory entry with no display is one nobody can read.
		display = held(loincShort)
	}

	return Concept{
		Code:    held(loincCode),
		Display: display,
		Active:  strings.EqualFold(held(loincStatus), loincActive),
	}
}

// LOINCColumns lists what this reader looks for, so a test can build a release
// in the shape a real one has.
func LOINCColumns() []string {
	return slices.Clone([]string{loincCode, loincDisplay, loincShort, loincStatus})
}
