package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/conformance"
	"github.com/Ilavrita/Ilavrita/packages/fhirpath"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/validate"
)

// A resource may declare the profiles it claims to conform to, in meta.profile.
//
// The claim is the point. R4 says a resource declaring a profile SHALL conform
// to it, so a server that stored one violating the profile it names would be
// storing a record that says something about itself which is not true — and
// every reader downstream trusts that line.
//
// What this build does about it has two halves, and the second matters as much
// as the first. A profile it holds is applied. A profile it does not hold is
// said so, out loud, against the resource: a claim nobody checked must not come
// back looking checked.

// declaredProfiles reads the profiles a resource claims.
func declaredProfiles(content []byte) []string {
	var held struct {
		Meta struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
	}

	if err := json.Unmarshal(content, &held); err != nil {
		return nil
	}

	return held.Meta.Profile
}

// checkDeclaredProfiles holds a resource to the profiles it names.
//
// A profile this install does not hold is reported and not applied. It is a
// warning rather than a refusal because a client naming a profile from
// somewhere else has done nothing wrong — but it is reported, because the
// alternative is a resource that passes validation while nobody looked at the
// one thing it said about itself.
func checkDeclaredProfiles(
	ctx context.Context, resourceType storage.ResourceType, content []byte,
) (validate.Report, error) {
	var report validate.Report

	claimed := declaredProfiles(content)
	if len(claimed) == 0 || serving == nil || serving.definitions == nil {
		return report, nil
	}

	var resource any
	if err := json.Unmarshal(content, &resource); err != nil {
		return report, nil
	}

	for _, url := range claimed {
		record, held, err := serving.definitions.ReadByURL(ctx, url)
		if err != nil {
			return report, err
		}

		if !held {
			report.Note(validate.SeverityWarning, string(resourceType)+".meta.profile",
				"This server does not hold "+url+", so it did not check this resource "+
					"against it.")

			continue
		}

		applyProfile(&report, record.Content, url, resourceType, resource)
	}

	return report, nil
}

// applyProfile evaluates one profile's own invariants against a resource.
//
// Only its invariants. A profile also narrows cardinality, types and bindings,
// and slices repeating elements; none of that is applied here, which is why
// docs/known-limitations.md says a profile is partly checked rather than
// checked. Saying it applied the whole of a profile would be the same mistake
// as saying nothing.
func applyProfile(
	report *validate.Report, definition []byte, url string,
	resourceType storage.ResourceType, resource any,
) {
	constraints, against, err := conformance.ConstraintsIn(definition)
	if err != nil {
		report.Note(validate.SeverityWarning, string(resourceType)+".meta.profile",
			"This server holds "+url+" and could not read it, so this resource was "+
				"not checked against it.")

		return
	}

	if against != "" && against != string(resourceType) {
		report.Note(validate.SeverityError, string(resourceType)+".meta.profile",
			url+" constrains "+against+", and this is a "+string(resourceType)+".")

		return
	}

	for _, one := range constraints {
		if !one.Applies() {
			continue
		}

		parsed, err := fhirpath.Parse(one.Expression)
		if err != nil {
			report.Note(validate.SeverityWarning, string(resourceType)+".meta.profile",
				fmt.Sprintf("%s states %s, which this server cannot evaluate.", url, one.Key))

			continue
		}

		satisfied, err := fhirpath.Holds(parsed, fhirpath.Context{This: resource, Resource: resource})
		if err != nil || satisfied {
			continue
		}

		severity := validate.SeverityWarning
		if one.Required() {
			severity = validate.SeverityError
		}

		report.Note(severity, string(resourceType), one.Key+": "+one.Human)
	}
}

// checkProfiles refuses a resource that does not conform to a profile it
// declared, and lets one pass whose profile this server does not hold.
func checkProfiles(ctx context.Context, resourceType storage.ResourceType, content []byte) error {
	report, err := checkDeclaredProfiles(ctx, resourceType, content)
	if err != nil {
		return err
	}

	if report.OK() {
		return nil
	}

	return errInvalidResource{report: report}
}
