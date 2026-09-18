package validate

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// idPattern is R4's own id datatype: letters, digits, hyphens and dots, up to
// 64 characters. Every logical id and every relative reference's id is one.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9\-.]{1,64}$`)

// datePattern covers R4's date, dateTime and instant in one expression, because
// each is a prefix of the next and an element declared as one accepts the
// shorter forms. Anything that is not a prefix of a full instant is refused.
var datePattern = regexp.MustCompile(
	`^([0-9]{4})(-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01])` +
		`(T([01][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\.[0-9]+)?` +
		`(Z|[+-]((0[0-9]|1[0-3]):[0-5][0-9]|14:00)))?)?)?$`)

// maximumDepth bounds how far into a body this walks. A resource nests, and a
// body nested ten thousand deep is a request that costs this server a stack
// rather than a resource anybody meant to store.
const maximumDepth = 64

// Resource checks one submitted resource.
//
// The type is what the route decided it is, and the content is what the client
// sent. A body that is not a JSON object at all is one error and nothing
// further: everything below reads members, and there are none.
func Resource(resourceType storage.ResourceType, content []byte) Report {
	var report Report

	var held map[string]any
	if err := json.Unmarshal(content, &held); err != nil {
		report.note(SeverityError, string(resourceType), "The body is not a JSON object.")

		return report
	}

	declared, _ := held["resourceType"].(string)
	if declared != string(resourceType) {
		report.note(SeverityError, string(resourceType)+".resourceType",
			"The body names "+strconv.Quote(declared)+" and the request names "+
				strconv.Quote(string(resourceType))+".")

		return report
	}

	report.checkIdentifier(held, string(resourceType))
	report.checkServerOwned(held, string(resourceType))
	report.walk(held, string(resourceType), 0)
	report.checkIndexedElements(resourceType, content)

	return report
}

// checkIdentifier holds the id datatype, which every logical id is.
func (r *Report) checkIdentifier(held map[string]any, at string) {
	raw, carried := held["id"]
	if !carried {
		return
	}

	id, isString := raw.(string)
	if !isString || !idPattern.MatchString(id) {
		r.note(SeverityError, at+".id",
			"An id is 1 to 64 characters of letters, digits, hyphens and dots.")
	}
}

// checkServerOwned warns about the members a client does not get to set.
//
// It is a warning rather than an error: the write path ignores them and stamps
// its own, so the resource is stored correctly either way. What the client
// needs to know is that what they sent was not kept.
func (r *Report) checkServerOwned(held map[string]any, at string) {
	meta, carried := held["meta"].(map[string]any)
	if !carried {
		return
	}

	for _, owned := range []string{"versionId", "lastUpdated"} {
		if _, stated := meta[owned]; stated {
			r.note(SeverityWarning, at+".meta."+owned,
				"This server sets "+owned+" itself; what was submitted is not kept.")
		}
	}
}

// walk checks the rules that hold for every element of every resource, whatever
// its definition says that element is.
//
// These are rules about how JSON represents FHIR rather than about any
// particular resource, which is exactly why they can be checked without an
// element model: no StructureDefinition permits a null, an empty string or an
// empty array anywhere.
func (r *Report) walk(held map[string]any, at string, depth int) {
	if depth > maximumDepth {
		r.note(SeverityError, at, "The body nests deeper than this server reads.")

		return
	}

	for name, value := range held {
		where := at + "." + name

		if name == "" {
			r.note(SeverityError, at, "An element has no name.")

			continue
		}

		r.checkValue(value, where, depth)
	}
}

// checkValue holds one element's value to the representation rules.
func (r *Report) checkValue(value any, where string, depth int) {
	switch held := value.(type) {
	case nil:
		// R4's JSON representation has no null. An element with no value is
		// absent; one written as null says something no StructureDefinition can
		// describe.
		r.note(SeverityError, where, "An element with no value is absent, never null.")
	case string:
		if strings.TrimSpace(held) == "" {
			r.note(SeverityError, where, "A string element has at least one non-whitespace character.")
		}
	case []any:
		if len(held) == 0 {
			r.note(SeverityError, where, "A repeating element with no values is absent, never empty.")
		}

		for index, member := range held {
			r.checkValue(member, where+"["+strconv.Itoa(index)+"]", depth+1)
		}
	case map[string]any:
		r.checkReference(held, where)
		r.walk(held, where, depth+1)
	}
}

// checkReference holds a relative reference to "Type/id".
//
// Only a relative one. An absolute URL names a resource on another server, a
// "urn:" names one inside a bundle and a "#" names a contained resource; none
// of those is this server's to judge, and refusing one would refuse a resource
// R4 permits.
func (r *Report) checkReference(held map[string]any, where string) {
	raw, carried := held["reference"]
	if !carried {
		return
	}

	reference, isString := raw.(string)
	if !isString {
		// Caught as a representation error by the walk below it.
		return
	}

	if strings.Contains(reference, ":") || strings.HasPrefix(reference, "#") {
		return
	}

	resourceType, id, split := strings.Cut(reference, "/")
	if !split || !fhir.IsResourceType(resourceType) || !idPattern.MatchString(id) {
		// A type R4 defines rather than one this build serves. A reference
		// names a resource that may live on another server running the whole of
		// R4, and refusing that would refuse a resource R4 permits.
		r.note(SeverityError, where+".reference",
			"A relative reference names an R4 resource type and an id, as Type/id.")
	}
}

// checkIndexedElements holds the syntax of the elements this build already
// asserts something about.
//
// The search registry is where this build says what an element is: a date
// parameter over "effectiveDateTime" is a claim that the element holds a date,
// made by the same table a query is compiled from. Checking it here means a
// resource is refused for a malformed date rather than stored and then quietly
// missing from every search for it.
func (r *Report) checkIndexedElements(resourceType storage.ResourceType, content []byte) {
	for _, parameter := range search.Supported(resourceType) {
		if parameter.Kind() != search.KindDate || !parameter.Projects() {
			continue
		}

		for _, raw := range search.ValuesAt(content, parameter.Path()) {
			held, isString := raw.(string)
			if !isString || datePattern.MatchString(held) {
				continue
			}

			r.note(SeverityError,
				string(resourceType)+"."+strings.Join(parameter.Path(), "."),
				"A date is YYYY, YYYY-MM, YYYY-MM-DD or a full instant.")
		}
	}
}
