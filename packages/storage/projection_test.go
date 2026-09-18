package storage_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func mustProjection(t *testing.T, elements ...string) storage.Projection {
	t.Helper()

	projection, err := storage.NewProjection(elements...)
	if err != nil {
		t.Fatalf("NewProjection(%v): %v", elements, err)
	}

	return projection
}

// TestAProjectionAlwaysReturnsWhatAddressesTheResource. A resource that cannot
// be addressed or version-checked is not usable, and withholding those buys
// nothing: the reader already holds the row.
func TestAProjectionAlwaysReturnsWhatAddressesTheResource(t *testing.T) {
	projection := mustProjection(t, "status")

	for _, required := range []string{"resourceType", "id", "meta"} {
		if !slices.Contains(projection.Elements(), required) {
			t.Errorf("a projection naming only status dropped %s", required)
		}
	}
}

// TestAnUnnamedElementIsAbsentRatherThanEmpty. A reader must not be able to tell
// a withheld value from one nobody recorded, which an empty member would say.
func TestAnUnnamedElementIsAbsentRatherThanEmpty(t *testing.T) {
	content := []byte(`{"resourceType":"Observation","id":"obs-1","status":"final",` +
		`"note":[{"text":"private"}],"valueQuantity":{"value":7}}`)

	narrowed, err := mustProjection(t, "status").Apply(content)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(narrowed, &members); err != nil {
		t.Fatalf("decode the narrowed resource: %v", err)
	}

	for _, withheld := range []string{"note", "valueQuantity"} {
		if _, present := members[withheld]; present {
			t.Errorf("%s survived a projection that does not name it", withheld)
		}
	}

	for _, kept := range []string{"resourceType", "id", "status"} {
		if _, present := members[kept]; !present {
			t.Errorf("%s was dropped by a projection that returns it", kept)
		}
	}
}

// TestAProjectionIsBuiltOnlyFromElementNames, because a name nothing matches
// would withhold everything, which is a restriction nobody stated.
func TestAProjectionIsBuiltOnlyFromElementNames(t *testing.T) {
	refused := map[string]struct {
		elements []string
		want     error
	}{
		"no element at all": {nil, storage.ErrEmptyProjection},
		"an empty name":     {[]string{""}, storage.ErrMalformedProjectionElement},
		"a dotted path":     {[]string{"code.coding"}, storage.ErrMalformedProjectionElement},
		"a wildcard":        {[]string{"*"}, storage.ErrMalformedProjectionElement},
		"a quote":           {[]string{"status'"}, storage.ErrMalformedProjectionElement},
		"one bad among good": {
			[]string{"status", "note[0]"}, storage.ErrMalformedProjectionElement,
		},
	}

	for name, refusal := range refused {
		if _, err := storage.NewProjection(refusal.elements...); !errors.Is(err, refusal.want) {
			t.Errorf("%s: err = %v, want %v", name, err, refusal.want)
		}
	}
}

// TestTheZeroProjectionReturnsNothingRatherThanEverything. A Grant carries
// *Projection so absent means unrestricted; a present one nobody built must be
// distinguishable from that.
func TestTheZeroProjectionReturnsNothingRatherThanEverything(t *testing.T) {
	var zero storage.Projection

	if !zero.IsZero() {
		t.Error("the zero Projection does not report itself as one")
	}

	if _, err := zero.Apply([]byte(`{"resourceType":"Observation"}`)); !errors.Is(err, storage.ErrEmptyProjection) {
		t.Errorf("the zero Projection applied without complaint: err = %v", err)
	}
}

// TestGrantsHeldTogetherReturnTheWidestOfThem. Grants are held together, so a
// member any one of them returns is one the reader may have.
func TestGrantsHeldTogetherReturnTheWidestOfThem(t *testing.T) {
	status := mustProjection(t, "status")
	value := mustProjection(t, "valueQuantity")

	combined := storage.WidestProjection([]*storage.Projection{&status, &value})
	if combined == nil {
		t.Fatal("two projected grants combined to no restriction at all")
	}

	for _, kept := range []string{"status", "valueQuantity", "id", "resourceType", "meta"} {
		if !slices.Contains(combined.Elements(), kept) {
			t.Errorf("the combination dropped %s", kept)
		}
	}

	if slices.Contains(combined.Elements(), "note") {
		t.Error("the combination returns a member neither grant names")
	}
}

// TestOneUnprojectedGrantReturnsEverything, because a Grant nobody narrowed
// reaches all of the resource and the others cannot take that back.
func TestOneUnprojectedGrantReturnsEverything(t *testing.T) {
	status := mustProjection(t, "status")

	if combined := storage.WidestProjection([]*storage.Projection{&status, nil}); combined != nil {
		t.Errorf("an unprojected grant was narrowed to %q", combined)
	}
}

// TestAProjectionCannotBeWidenedByItsHolder.
func TestAProjectionCannotBeWidenedByItsHolder(t *testing.T) {
	projection := mustProjection(t, "status")

	elements := projection.Elements()
	elements[0] = "note"

	if got := projection.Elements(); slices.Contains(got, "note") {
		t.Errorf("a caller widened the projection to %v", got)
	}
}
