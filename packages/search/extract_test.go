package search_test

import (
	"slices"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/search"
)

func extracted(t *testing.T, resourceType, body string) []search.Entry {
	t.Helper()

	entries, err := search.Extract(nil, storageType(resourceType), []byte(body))
	if err != nil {
		t.Fatalf("extract %s: %v", resourceType, err)
	}

	return entries
}

// TestATokenIsIndexedWithTheSystemBesideIt. Reading the two halves from one
// element is what stops a search pairing one coding's system with another's
// code, which would return a resource that does not match the query.
func TestATokenIsIndexedWithTheSystemBesideIt(t *testing.T) {
	entries := extracted(t, "Observation", `{"resourceType":"Observation","category":[{"coding":[
		{"system":"http://a","code":"vital-signs"},{"system":"http://b","code":"laboratory"}]}]}`)

	want := []search.Entry{
		{Parameter: "category", Kind: search.KindToken, Code: "vital-signs", System: "http://a"},
		{Parameter: "category", Kind: search.KindToken, Code: "laboratory", System: "http://b"},
	}

	for _, entry := range want {
		if !slices.Contains(entries, entry) {
			t.Errorf("%v is not indexed; got %v", entry, entries)
		}
	}

	// The crossed pairing is the one a naive extractor produces.
	crossed := search.Entry{
		Parameter: "category", Kind: search.KindToken, Code: "vital-signs", System: "http://b",
	}
	if slices.Contains(entries, crossed) {
		t.Errorf("a code was indexed under another coding's system: %v", entries)
	}
}

// TestAPlainCodeIsItsOwnToken, because a status carries no system to pair.
func TestAPlainCodeIsItsOwnToken(t *testing.T) {
	entries := extracted(t, "Observation", `{"resourceType":"Observation","status":"final"}`)

	want := search.Entry{Parameter: "status", Kind: search.KindToken, Code: "final"}
	if !slices.Contains(entries, want) {
		t.Errorf("a plain status is not indexed: %v", entries)
	}
}

// TestAnElementIsReadThroughWhateverShapeFHIRChose, which is what lets one
// parameter serve a resource that repeated an element and one that did not.
func TestAnElementIsReadThroughWhateverShapeFHIRChose(t *testing.T) {
	shapes := map[string]string{
		"arrays all the way":  `{"resourceType":"Observation","category":[{"coding":[{"code":"vital-signs"}]}]}`,
		"objects all the way": `{"resourceType":"Observation","category":{"coding":{"code":"vital-signs"}}}`,
		"an array of codings": `{"resourceType":"Observation","category":{"coding":[{"code":"vital-signs"}]}}`,
	}

	want := search.Entry{Parameter: "category", Kind: search.KindToken, Code: "vital-signs"}

	for name, body := range shapes {
		if entries := extracted(t, "Observation", body); !slices.Contains(entries, want) {
			t.Errorf("%s: not indexed; got %v", name, entries)
		}
	}
}

// TestAReferenceIsIndexedOnlyWhenItNamesARowHere. An absolute URL names a
// resource on another server and a contained reference one inside this
// document; a search here can return neither.
func TestAReferenceIsIndexedOnlyWhenItNamesARowHere(t *testing.T) {
	indexed := extracted(t, "Observation",
		`{"resourceType":"Observation","subject":{"reference":"Patient/pat-1"}}`)

	want := search.Entry{Parameter: "subject", Kind: search.KindReference, Code: "Patient/pat-1"}
	if !slices.Contains(indexed, want) {
		t.Errorf("a relative reference is not indexed: %v", indexed)
	}

	unindexable := map[string]string{
		"an absolute url":    `{"resourceType":"Observation","subject":{"reference":"http://x/Patient/p"}}`,
		"a contained target": `{"resourceType":"Observation","subject":{"reference":"#inline"}}`,
		"a versioned target": `{"resourceType":"Observation","subject":{"reference":"Patient/p/_history/2"}}`,
		"no type at all":     `{"resourceType":"Observation","subject":{"reference":"pat-1"}}`,
		"a fragment on a relative reference": `{"resourceType":"Observation",` +
			`"subject":{"reference":"Patient/pat-1#part"}}`,
		"a scheme with no host": `{"resourceType":"Observation","subject":{"reference":"urn:uuid/abc"}}`,
	}

	for name, body := range unindexable {
		for _, entry := range extracted(t, "Observation", body) {
			if entry.Kind == search.KindReference {
				t.Errorf("%s was indexed as %v", name, entry)
			}
		}
	}
}

// TestAStringIsFoldedSoASearchNeedNotGuessTheCapitalisation.
func TestAStringIsFoldedSoASearchNeedNotGuessTheCapitalisation(t *testing.T) {
	entries := extracted(t, "Patient",
		`{"resourceType":"Patient","name":[{"family":"MacDonald","given":["Ada","Jane"]}]}`)

	for _, want := range []search.Entry{
		{Parameter: "family", Kind: search.KindString, Folded: "macdonald"},
		{Parameter: "given", Kind: search.KindString, Folded: "ada"},
		{Parameter: "given", Kind: search.KindString, Folded: "jane"},
	} {
		if !slices.Contains(entries, want) {
			t.Errorf("%v is not indexed; got %v", want, entries)
		}
	}
}

// TestADateIsIndexedAsTheSpanItNames, so a whole day matches an instant inside
// it and an instant matches only itself.
func TestADateIsIndexedAsTheSpanItNames(t *testing.T) {
	day := extracted(t, "Patient", `{"resourceType":"Patient","birthDate":"1980-07-04"}`)

	var span search.Entry

	for _, entry := range day {
		if entry.Parameter == "birthdate" {
			span = entry
		}
	}

	if span.Upper <= span.Lower {
		t.Fatalf("a whole day was indexed as %v, want a span", span)
	}

	if span.Upper-span.Lower != 24*60*60*1000-1 {
		t.Errorf("a whole day spans %d ms", span.Upper-span.Lower)
	}
}

// TestAValueThisBuildCannotReadIsLeftOut rather than guessed at. An index entry
// nobody can justify makes a search return a resource that does not match it.
func TestAValueThisBuildCannotReadIsLeftOut(t *testing.T) {
	unreadable := map[string]string{
		"a partial date":                      `{"resourceType":"Patient","birthDate":"1980-07"}`,
		"a date that is a year":               `{"resourceType":"Patient","birthDate":"1980"}`,
		"an object where a code was expected": `{"resourceType":"Observation","status":{"code":"final"}}`,
		"an empty code":                       `{"resourceType":"Observation","status":""}`,
		"an absent element":                   `{"resourceType":"Observation"}`,
	}

	for name, body := range unreadable {
		resourceType := "Observation"
		if name == "a partial date" || name == "a date that is a year" {
			resourceType = "Patient"
		}

		for _, entry := range extracted(t, resourceType, body) {
			t.Errorf("%s was indexed as %v", name, entry)
		}
	}
}

// TestARepeatedValueIsIndexedOnce, so a resource naming one code twice costs
// one row rather than two that say the same thing.
func TestARepeatedValueIsIndexedOnce(t *testing.T) {
	entries := extracted(t, "Observation", `{"resourceType":"Observation","category":[
		{"coding":[{"code":"vital-signs"}]},{"coding":[{"code":"vital-signs"}]}]}`)

	held := 0

	for _, entry := range entries {
		if entry.Parameter == "category" {
			held++
		}
	}

	if held != 1 {
		t.Errorf("one repeated code was indexed %d times", held)
	}
}

// TestMalformedContentIsAnErrorRatherThanNoEntries. A body this cannot read
// must not be treated as one carrying nothing, because nothing is what a
// search would then find it under.
func TestMalformedContentIsAnErrorRatherThanNoEntries(t *testing.T) {
	if _, err := search.Extract(nil, "Observation", []byte("not json")); err == nil {
		t.Error("unreadable content extracted no entries instead of failing")
	}
}
