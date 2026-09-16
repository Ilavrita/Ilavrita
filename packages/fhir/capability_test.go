package fhir

import (
	"testing"
	"time"
)

func statement() CapabilityStatement {
	return NewCapabilityStatement("0.0.1", time.Unix(1_700_000_000, 0), "https://example.org/fhir/R4")
}

// R4 requires status, date, kind, fhirVersion and at least one format.
func TestRequiredElementsArePresent(t *testing.T) {
	got := statement()

	for name, value := range map[string]string{
		"status":      got.Status,
		"date":        got.Date,
		"kind":        got.Kind,
		"fhirVersion": got.FHIRVersion,
	} {
		if value == "" {
			t.Errorf("%s is empty; R4 requires it", name)
		}
	}

	if len(got.Format) == 0 {
		t.Error("format is empty; R4 requires at least one")
	}
}

func TestDateIsAnRFC3339Instant(t *testing.T) {
	if _, err := time.Parse(time.RFC3339, statement().Date); err != nil {
		t.Errorf("date %q is not a valid dateTime: %v", statement().Date, err)
	}
}

// R4 invariant cpb-2: if kind is "instance", implementation must be present.
func TestAnInstanceStatementCarriesItsImplementation(t *testing.T) {
	got := statement()

	if got.Kind == "instance" && got.Implementation.Description == "" {
		t.Error("kind is instance but implementation is absent, violating cpb-2")
	}
}

// R4 invariant cpb-1: at least one of rest, messaging or document.
func TestStatementDeclaresARestEndpoint(t *testing.T) {
	if len(statement().Rest) == 0 {
		t.Error("no rest element, violating cpb-1")
	}
}

func TestNoResourceIsAdvertisedUntilOneIsImplemented(t *testing.T) {
	for _, rest := range statement().Rest {
		if len(rest.Resource) != 0 {
			t.Errorf("advertised %d resource(s) while every FHIR route answers 501", len(rest.Resource))
		}
	}
}

func TestTheStatementIsMarkedExperimental(t *testing.T) {
	if !statement().Experimental {
		t.Error("a server implementing no interaction must not present itself as production")
	}
}
