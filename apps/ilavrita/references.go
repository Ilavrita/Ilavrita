package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/validate"
	"github.com/pocketbase/pocketbase/core"
)

// maximumCheckedReferences bounds how many a single resource is followed for.
//
// Each one is a read, and one pooled connection per process means a resource
// naming five hundred would be every other request waiting on a diagnostic.
const maximumCheckedReferences = 50

// Reference integrity is answered rather than enforced.
//
// R4 permits a reference to name a resource this server does not hold: the
// target may live somewhere else, or may not exist yet. This build depends on
// that itself — a confined grant names the compartment before the Patient
// exists, which is how one is provisioned at all.
//
// So a dangling reference is not a refusal. It is something $validate reports,
// because $validate is where a client asks what this server makes of a resource
// and a reference nobody can follow is a record that reads as complete and is
// not.

// checkReferencesResolve reports the relative references that name nothing the
// caller can read.
//
// Resolved under the caller's own Scope, which matters: answering from the
// store itself would tell a caller whether a resource they may not see exists,
// and an id must not be probeable. Under their Scope, "nothing there" and "not
// yours" are one answer — the same one a read gives them.
func checkReferencesResolve(
	request *core.RequestEvent, content []byte,
) validate.Report {
	var report validate.Report

	ctx := request.Request.Context()

	for _, named := range referencesIn(content) {
		resourceType, id, ok := strings.Cut(named, "/")
		if !ok || !fhir.ServesResourceType(resourceType) {
			continue
		}

		// Reading whether a Patient exists needs read on Patient, so the
		// reference is followed under a grant for the type it names. A caller
		// without one is told nothing rather than "it is not there": the second
		// is an answer about a resource they may not ask about.
		held, err := permit(request, storage.ResourceType(resourceType), readActions)
		if err != nil {
			continue
		}

		target, err := storage.NewResourceKey(held.project,
			storage.ResourceType(resourceType), storage.LogicalID(id))
		if err != nil {
			continue
		}

		switch _, err := held.resources.Read(ctx, held.scope, target); {
		case err == nil:
		case errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrDeleted):
			report.Note(validate.SeverityWarning, string(target.Type),
				"This resource names "+named+", which is not something this Project "+
					"holds that you can read.")
		default:
			// Anything else is this server failing rather than the reference
			// being wrong, and saying the reference is wrong would be a lie.
			return report
		}
	}

	return report
}

// referencesIn collects the distinct relative references a resource states.
//
// Only relative ones. An absolute url names a resource on another server, a
// "urn:" names one inside a bundle and a "#" names a contained resource — none
// of those is this server's to resolve, and ref-1 already holds the last.
func referencesIn(content []byte) []string {
	var held any
	if err := json.Unmarshal(content, &held); err != nil {
		return nil
	}

	var found []string

	walkReferences(held, &found)

	return found
}

// walkReferences descends into whatever the resource is made of.
func walkReferences(held any, found *[]string) {
	if len(*found) >= maximumCheckedReferences {
		return
	}

	switch value := held.(type) {
	case map[string]any:
		if named, is := value["reference"].(string); is {
			if relative(named) && !slices.Contains(*found, named) {
				*found = append(*found, named)
			}
		}

		for _, one := range value {
			walkReferences(one, found)
		}

	case []any:
		for _, one := range value {
			walkReferences(one, found)
		}
	}
}

// relative reports a reference this server could resolve for itself.
func relative(named string) bool {
	return named != "" &&
		!strings.HasPrefix(named, "#") &&
		!strings.Contains(named, "://") &&
		!strings.HasPrefix(named, "urn:")
}
