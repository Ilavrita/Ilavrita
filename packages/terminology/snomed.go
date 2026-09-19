package terminology

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// The RF2 columns this build reads, by name. A release file names them in its
// header row, and reading by name rather than position is what survives a
// release adding one.
const (
	rf2ID          = "id"
	rf2Active      = "active"
	rf2ConceptID   = "conceptId"
	rf2TypeID      = "typeId"
	rf2Term        = "term"
	rf2LanguageTag = "languageCode"
)

// fullySpecifiedName is the description type SNOMED gives each concept exactly
// one of, which is what makes it the display a directory shows. A synonym is one
// of many and picking between them is an editorial decision this build does not
// make.
const fullySpecifiedName = "900000000000003001"

// maximumLine bounds one RF2 row. A term is a sentence, not a document, and a
// file claiming otherwise is one this build refuses rather than reads into
// memory.
const maximumLine = 1 << 20

// SNOMEDRelease reads a SNOMED CT RF2 snapshot.
//
// It takes two files because RF2 keeps them apart: the concept file says which
// concepts exist and whether they are active, and the description file says what
// each one is called. Neither alone is a directory.
type SNOMEDRelease struct {
	concepts     io.Reader
	descriptions io.Reader
	version      string
}

var _ Release = (*SNOMEDRelease)(nil)

// NewSNOMEDRelease binds the two snapshot files of one release.
func NewSNOMEDRelease(concepts, descriptions io.Reader, version string) *SNOMEDRelease {
	return &SNOMEDRelease{concepts: concepts, descriptions: descriptions, version: version}
}

// System is SNOMED CT's canonical url.
func (r *SNOMEDRelease) System() string { return SNOMED }

// Version is the release the operator named — for SNOMED, the edition and the
// effective date, which together are what identifies one.
func (r *SNOMEDRelease) Version() string { return r.version }

// Read hands over every concept with the name its release gives it.
//
// The descriptions are read first and held, because a concept row carries no
// name and reading the two in step would mean seeking one file while streaming
// the other. Names are the smaller half: one fully specified name per concept,
// against every description in every language.
func (r *SNOMEDRelease) Read(receive func(Concept) error) error {
	names, err := r.readNames()
	if err != nil {
		return err
	}

	return eachRow(r.concepts, func(row map[string]string) error {
		code := row[rf2ID]
		if strings.TrimSpace(code) == "" {
			return nil
		}

		return receive(Concept{
			Code:    code,
			Display: names[code],
			Active:  row[rf2Active] == "1",
		})
	})
}

// readNames reads one fully specified name per concept.
func (r *SNOMEDRelease) readNames() (map[string]string, error) {
	names := map[string]string{}

	err := eachRow(r.descriptions, func(row map[string]string) error {
		if row[rf2Active] != "1" || row[rf2TypeID] != fullySpecifiedName {
			return nil
		}

		// One language, whichever the release carries first. A release holding
		// several is one where choosing between them is the deployment's call,
		// and this build does not make it for them.
		if held, taken := names[row[rf2ConceptID]]; taken && held != "" {
			return nil
		}

		names[row[rf2ConceptID]] = row[rf2Term]

		return nil
	})
	if err != nil {
		return nil, err
	}

	return names, nil
}

// eachRow reads a tab-delimited RF2 file, handing over each row by column name.
func eachRow(held io.Reader, receive func(map[string]string) error) error {
	scanner := bufio.NewScanner(held)
	scanner.Buffer(make([]byte, 0, 64<<10), maximumLine)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrUnreadableRelease, err)
		}

		return fmt.Errorf("%w: it holds no header", ErrUnreadableRelease)
	}

	header := strings.Split(strings.TrimRight(scanner.Text(), "\r"), "\t")

	for _, needed := range []string{rf2ID, rf2Active} {
		if !contains(header, needed) {
			return fmt.Errorf("%w: it names no %s column", ErrUnreadableRelease, needed)
		}
	}

	row := make(map[string]string, len(header))

	for scanner.Scan() {
		clear(row)

		for index, value := range strings.Split(strings.TrimRight(scanner.Text(), "\r"), "\t") {
			if index < len(header) {
				row[header[index]] = value
			}
		}

		if err := receive(row); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadableRelease, err)
	}

	return nil
}

func contains(held []string, want string) bool {
	for _, name := range held {
		if name == want {
			return true
		}
	}

	return false
}

// RF2ConceptColumns names the header a concept snapshot carries, so a test can
// build one in the shape a real release has.
func RF2ConceptColumns() []string {
	return []string{rf2ID, "effectiveTime", rf2Active, "moduleId", "definitionStatusId"}
}

// RF2DescriptionColumns names the header a description snapshot carries.
func RF2DescriptionColumns() []string {
	return []string{
		rf2ID, "effectiveTime", rf2Active, "moduleId", rf2ConceptID,
		rf2LanguageTag, rf2TypeID, rf2Term, "caseSignificanceId",
	}
}
