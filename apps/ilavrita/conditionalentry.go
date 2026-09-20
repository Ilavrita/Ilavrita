package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// A transaction entry may name what it means by a search rather than an id.
//
// Two ways, and they are different questions. `ifNoneExist` on a create says
// "unless one already matches", which is the same idempotence a conditional
// create gives a lone request. A reference written `Patient?identifier=…` says
// "the patient this identifies", which lets a Bundle from another system point
// at resources whose ids here it has never seen.
//
// Both resolve before anything is written, for the same reason every identity
// does: an entry that resolved as it went could name a resource a later entry
// was about to create, and the order entries are submitted in is not the order
// they run.

// errUnresolvedReference reports a conditional reference matching none.
var errUnresolvedReference = errors.New(
	"ilavrita: a conditional reference in this bundle matches no resource")

// conditionalReference reports whether a reference names a search rather than a
// resource, and returns the type and the query it states.
func conditionalReference(held string) (storage.ResourceType, url.Values, bool) {
	resourceType, query, found := strings.Cut(held, "?")
	if !found || query == "" || !fhir.ServesResourceType(resourceType) {
		return "", nil, false
	}

	asked, err := url.ParseQuery(query)
	if err != nil || len(asked) == 0 {
		return "", nil, false
	}

	return storage.ResourceType(resourceType), asked, true
}

// resolveConditionalReferences rewrites every conditional reference in one
// entry to the resource it identifies.
//
// A reference matching nothing fails the transaction rather than being left as
// written. The entry said "the patient this identifies" and there is none, so
// storing it would store a reference to a search — which no reader could follow
// and no later write would fix.
func resolveConditionalReferences(
	request *core.RequestEvent, content json.RawMessage,
) (json.RawMessage, error) {
	if len(content) == 0 {
		return content, nil
	}

	var held any
	if err := json.Unmarshal(content, &held); err != nil {
		return nil, unreadableBody
	}

	rewritten, err := rewriteConditional(request, held)
	if err != nil {
		return nil, err
	}

	if !rewritten {
		return content, nil
	}

	encoded, err := json.Marshal(held)
	if err != nil {
		return nil, fmt.Errorf("ilavrita: encode a resolved entry: %w", err)
	}

	return encoded, nil
}

// rewriteConditional walks one entry, replacing conditional references, and
// reports whether it changed anything.
func rewriteConditional(request *core.RequestEvent, held any) (bool, error) {
	switch value := held.(type) {
	case map[string]any:
		changed := false

		if named, is := value["reference"].(string); is {
			resolved, rewritten, err := resolvedReference(request, named)
			if err != nil {
				return false, err
			}

			if rewritten {
				value["reference"] = resolved
				changed = true
			}
		}

		for _, one := range value {
			deeper, err := rewriteConditional(request, one)
			if err != nil {
				return false, err
			}

			changed = changed || deeper
		}

		return changed, nil

	case []any:
		changed := false

		for _, one := range value {
			deeper, err := rewriteConditional(request, one)
			if err != nil {
				return false, err
			}

			changed = changed || deeper
		}

		return changed, nil

	default:
		return false, nil
	}
}

// resolvedReference turns one conditional reference into the resource it names.
func resolvedReference(request *core.RequestEvent, named string) (string, bool, error) {
	resourceType, asked, conditional := conditionalReference(named)
	if !conditional {
		return named, false, nil
	}

	matched, err := matchedInType(request, resourceType, asked)
	if err != nil {
		return "", false, err
	}

	if len(matched) == 0 {
		return "", false, fmt.Errorf("%w: %s", errUnresolvedReference, named)
	}

	return string(resourceType) + "/" + string(matched[0].Key.ID), true, nil
}

// settledEntry is what one entry will be stored under, and whether storing it
// is still something to do.
type settledEntry struct {
	key storage.ResourceKey

	// alreadyThere reports a conditional create whose condition matched. The
	// entry has an identity and nothing to write.
	alreadyThere bool
}

// matchedByEntryCondition runs a create entry's ifNoneExist, if it states one.
func matchedByEntryCondition(
	request *core.RequestEvent, resourceType storage.ResourceType, entry fhir.SubmittedEntry,
) (storage.LogicalID, bool, error) {
	if entry.Request == nil || entry.Request.IfNoneExist == "" {
		return "", false, nil
	}

	asked, err := url.ParseQuery(entry.Request.IfNoneExist)
	if err != nil || len(asked) == 0 {
		return "", false, errUnconditioned
	}

	matched, err := matchedInType(request, resourceType, asked)
	if err != nil {
		return "", false, err
	}

	if len(matched) == 0 {
		return "", false, nil
	}

	return matched[0].Key.ID, true, nil
}
