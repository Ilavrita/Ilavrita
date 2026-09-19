package terminology_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/terminology"
)

// collected reads a whole release into a map, which is what a small test can
// assert on.
func collected(t *testing.T, release terminology.Release) map[string]terminology.Concept {
	t.Helper()

	held := map[string]terminology.Concept{}

	if err := release.Read(func(concept terminology.Concept) error {
		held[concept.Code] = concept

		return nil
	}); err != nil {
		t.Fatalf("read the release: %v", err)
	}

	return held
}

// aLOINCTable builds a release in the shape a real one has: named columns, in
// an order this reader must not depend on.
const aLOINCTable = `"STATUS","LOINC_NUM","SHORTNAME","CLASS","LONG_COMMON_NAME"
"ACTIVE","99991-1","Gluc","CHEM","Glucose in Serum or Plasma"
"DEPRECATED","99992-2","Old","CHEM","A measurement nobody makes any more"
"ACTIVE","99993-3","","CHEM",""
"ACTIVE","","Nameless","CHEM","A row naming no code"
`

// TestALOINCReleaseIsReadByColumnName. A release adds columns between versions,
// so a reader counting from the left eventually reads the wrong one — quietly,
// and against every code in the file.
func TestALOINCReleaseIsReadByColumnName(t *testing.T) {
	held := collected(t, terminology.NewLOINCRelease(strings.NewReader(aLOINCTable), "2.77"))

	if len(held) != 3 {
		t.Fatalf("read %d concepts, want the three that name a code: %v", len(held), held)
	}

	if got := held["99991-1"]; got.Display != "Glucose in Serum or Plasma" || !got.Active {
		t.Errorf("an active code reads %+v", got)
	}

	// Retired codes are kept. A code a stored resource already carries has to
	// stay resolvable, or that resource becomes unreadable.
	if got := held["99992-2"]; got.Active || got.Display == "" {
		t.Errorf("a deprecated code reads %+v", got)
	}

	// A row with no long name falls back to the short one rather than landing in
	// the directory with nothing to show.
	if got := held["99993-3"]; got.Display != "" {
		t.Errorf("a row with neither name reads %+v", got)
	}
}

// TestALOINCReleaseWithoutItsColumnsIsRefused, rather than read as empty: a
// system loaded as nothing answers "no such code" for every code in it.
func TestALOINCReleaseWithoutItsColumnsIsRefused(t *testing.T) {
	for described, table := range map[string]string{
		"no code column":   "\"STATUS\",\"LONG_COMMON_NAME\"\n\"ACTIVE\",\"Something\"\n",
		"no status column": "\"LOINC_NUM\",\"LONG_COMMON_NAME\"\n\"1-1\",\"Something\"\n",
		"no header at all": "",
	} {
		release := terminology.NewLOINCRelease(strings.NewReader(table), "2.77")

		err := release.Read(func(terminology.Concept) error { return nil })
		if !errors.Is(err, terminology.ErrUnreadableRelease) {
			t.Errorf("%s answered %v", described, err)
		}
	}
}

// The two files an RF2 snapshot keeps apart. Neither alone is a directory: the
// concept file says what exists, the description file says what it is called.
const (
	rf2Concepts = "id\teffectiveTime\tactive\tmoduleId\tdefinitionStatusId\n" +
		"73211009\t20020131\t1\t900000000000207008\t900000000000074008\n" +
		"11111111\t20020131\t0\t900000000000207008\t900000000000074008\n"

	rf2Descriptions = "id\teffectiveTime\tactive\tmoduleId\tconceptId\tlanguageCode\ttypeId\tterm\tcaseSignificanceId\n" +
		// A synonym, which is one of many and is not the display.
		"1\t20020131\t1\t900000000000207008\t73211009\ten\t900000000000013009\tDiabetes\t900000000000448009\n" +
		// The fully specified name, which is the one a concept has exactly one of.
		"2\t20020131\t1\t900000000000207008\t73211009\ten\t900000000000003001\tDiabetes mellitus (disorder)\t900000000000448009\n" +
		// Inactive, so it is not the name of anything.
		"3\t20020131\t0\t900000000000207008\t11111111\ten\t900000000000003001\tWithdrawn (disorder)\t900000000000448009\n"
)

// TestASNOMEDReleaseTakesItsNameFromTheFullySpecifiedName. A concept has exactly
// one, and many synonyms; choosing between synonyms is an editorial decision
// this build does not make.
func TestASNOMEDReleaseTakesItsNameFromTheFullySpecifiedName(t *testing.T) {
	held := collected(t, terminology.NewSNOMEDRelease(
		strings.NewReader(rf2Concepts), strings.NewReader(rf2Descriptions), "INT 20260101"))

	if len(held) != 2 {
		t.Fatalf("read %d concepts: %v", len(held), held)
	}

	got := held["73211009"]
	if got.Display != "Diabetes mellitus (disorder)" {
		t.Errorf("it is displayed as %q, which is not its fully specified name", got.Display)
	}

	if !got.Active {
		t.Error("an active concept reads as inactive")
	}

	// A retired concept is still in the directory, and its inactive description
	// is not its name.
	retired := held["11111111"]
	if retired.Active {
		t.Error("a retired concept reads as active")
	}

	if retired.Display != "" {
		t.Errorf("a retired concept took an inactive description as its name: %q", retired.Display)
	}
}

// TestAnRF2FileWithoutItsColumnsIsRefused.
func TestAnRF2FileWithoutItsColumnsIsRefused(t *testing.T) {
	for described, held := range map[string][2]string{
		"concepts naming no id":       {"effectiveTime\tactive\n2020\t1\n", rf2Descriptions},
		"concepts naming no active":   {"id\teffectiveTime\n1\t2020\n", rf2Descriptions},
		"descriptions with no header": {rf2Concepts, ""},
	} {
		release := terminology.NewSNOMEDRelease(
			strings.NewReader(held[0]), strings.NewReader(held[1]), "INT")

		err := release.Read(func(terminology.Concept) error { return nil })
		if !errors.Is(err, terminology.ErrUnreadableRelease) {
			t.Errorf("%s answered %v", described, err)
		}
	}
}

// TestAReadStopsWhereItsReceiverDoes, so whatever stores the concepts decides
// how far a release is read rather than being handed all of one.
func TestAReadStopsWhereItsReceiverDoes(t *testing.T) {
	stop := errors.New("that is enough")
	read := 0

	err := terminology.NewLOINCRelease(strings.NewReader(aLOINCTable), "2.77").
		Read(func(terminology.Concept) error {
			read++

			return stop
		})

	if !errors.Is(err, stop) {
		t.Errorf("the read answered %v", err)
	}

	if read != 1 {
		t.Errorf("it read %d concepts after being told to stop", read)
	}
}
