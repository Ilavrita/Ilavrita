package search

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// Entry is one indexed value, ready for whatever stores it. A resource with
// three categories yields three token entries for one parameter, and a search
// naming any of them finds it.
type Entry struct {
	Parameter string
	Kind      Kind

	// Code carries a token's code and a reference's "Type/id".
	Code string

	// System qualifies a token, empty when the element carried none.
	System string

	// Folded is a string value lowered for prefix matching, so a search does
	// not have to know how a name was capitalised.
	Folded string

	// Lower and Upper bound a date, in milliseconds, inclusive.
	Lower, Upper int64
}

// Extract reads every indexed value one resource carries.
//
// It walks the same dotted paths a filter does and it crosses arrays the same
// way, because FHIR models one element as repeating and the next as single and
// a parameter's author should not have to know which. A value it cannot read is
// left out rather than guessed at: an index entry nobody can justify would make
// a search return a resource that does not match it.
func Extract(resourceType storage.ResourceType, content []byte) ([]Entry, error) {
	if len(content) == 0 {
		return nil, nil
	}

	var resource any
	if err := json.Unmarshal(content, &resource); err != nil {
		return nil, fmt.Errorf("search: read a resource to index it: %w", err)
	}

	var entries []Entry

	for _, parameter := range Indexed(resourceType) {
		for _, held := range walk([]any{resource}, parameter.Path()) {
			entry, indexable := entryFor(parameter, held)
			if !indexable {
				continue
			}

			// A resource naming one value twice is indexed once: a second row
			// costs a predicate and answers nothing new.
			if !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
		}
	}

	return entries, nil
}

// walk follows a dotted path, crossing arrays wherever it meets them. An
// element that repeats and one that does not are read alike, which is what lets
// one path serve "category.coding" whether the resource wrote one or many.
func walk(held []any, path []string) []any {
	for _, segment := range path {
		var next []any

		for _, value := range flatten(held) {
			members, object := value.(map[string]any)
			if !object {
				continue
			}

			if member, present := members[segment]; present {
				next = append(next, member)
			}
		}

		held = next
	}

	return flatten(held)
}

// flatten opens every array one level, so an element FHIR repeats reads as the
// values in it rather than as the list holding them.
func flatten(held []any) []any {
	var opened []any

	for _, value := range held {
		if list, repeating := value.([]any); repeating {
			opened = append(opened, flatten(list)...)

			continue
		}

		opened = append(opened, value)
	}

	return opened
}

// entryFor turns one reached value into the entry its parameter indexes.
func entryFor(parameter Parameter, held any) (Entry, bool) {
	switch parameter.Kind() {
	case KindToken:
		return tokenEntry(parameter, held)
	case KindString:
		text, readable := literal(held)
		if !readable || text == "" {
			return Entry{}, false
		}

		return Entry{Parameter: parameter.Name(), Kind: KindString, Folded: strings.ToLower(text)}, true
	case KindReference:
		return referenceEntry(parameter, held)
	case KindDate:
		return dateEntry(parameter, held)
	default:
		return Entry{}, false
	}
}

// tokenEntry reads a code and the system beside it out of one element. Both are
// read from the same element, so a search can never pair one coding's system
// with another's code.
func tokenEntry(parameter Parameter, held any) (Entry, bool) {
	entry := Entry{Parameter: parameter.Name(), Kind: KindToken}

	if parameter.CodeMember() == "" {
		code, readable := literal(held)
		if !readable || code == "" {
			return Entry{}, false
		}

		entry.Code = code

		return entry, true
	}

	members, object := held.(map[string]any)
	if !object {
		return Entry{}, false
	}

	code, readable := literal(members[parameter.CodeMember()])
	if !readable || code == "" {
		return Entry{}, false
	}

	entry.Code = code

	if parameter.SystemMember() != "" {
		if system, present := literal(members[parameter.SystemMember()]); present {
			entry.System = system
		}
	}

	return entry, true
}

// referenceEntry indexes a relative "Type/id" link and nothing else. An
// absolute URL names a resource on another server and a contained reference
// names one inside this document; neither is a row a search here can return.
func referenceEntry(parameter Parameter, held any) (Entry, bool) {
	text, readable := literal(held)
	if !readable {
		return Entry{}, false
	}

	resourceType, id, split := strings.Cut(text, "/")
	if !split || resourceType == "" || id == "" || strings.ContainsAny(text, ":#") {
		return Entry{}, false
	}

	if strings.Contains(id, "/") {
		return Entry{}, false
	}

	return Entry{Parameter: parameter.Name(), Kind: KindReference, Code: text}, true
}

// dateEntry reads the two forms this build indexes: a whole day and one
// instant. A partial date is left out rather than widened to the year it names.
func dateEntry(parameter Parameter, held any) (Entry, bool) {
	text, readable := literal(held)
	if !readable {
		return Entry{}, false
	}

	if day, err := time.Parse(time.DateOnly, text); err == nil {
		start := day.UTC()

		return Entry{
			Parameter: parameter.Name(), Kind: KindDate,
			Lower: start.UnixMilli(),
			Upper: start.AddDate(0, 0, 1).Add(-time.Millisecond).UnixMilli(),
		}, true
	}

	moment, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return Entry{}, false
	}

	return Entry{
		Parameter: parameter.Name(), Kind: KindDate,
		Lower: moment.UTC().UnixMilli(), Upper: moment.UTC().UnixMilli(),
	}, true
}

// literal reads a JSON scalar as the text an index holds. An object or an array
// is not a value, and a boolean is indexed as FHIR writes it.
func literal(held any) (string, bool) {
	switch value := held.(type) {
	case string:
		return value, true
	case bool:
		if value {
			return "true", true
		}

		return "false", true
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", value), "0"), "."), true
	default:
		return "", false
	}
}
