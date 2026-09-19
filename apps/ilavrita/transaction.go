package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/audit"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// maximumEntries bounds one transaction.
//
// Every entry is a write inside one commit boundary, and this server holds one
// pooled connection per process — so a transaction of ten thousand entries is
// every other request in that process waiting for it. A bound refused is a
// client that splits its work; a bound absent is an install one request can
// stall.
const maximumEntries = 200

var (
	// errTooManyEntries reports a transaction larger than this server performs
	// in one commit.
	errTooManyEntries = errors.New(
		"ilavrita: that transaction holds more entries than this server performs at once")

	// errEntryNotSupported reports an entry naming an interaction a transaction
	// does not perform here.
	errEntryNotSupported = errors.New(
		"ilavrita: a transaction entry creates, updates or deletes one resource")
)

// performTransaction answers a Bundle of type transaction.
//
// Every entry happens or none does. What makes that true is the commit boundary
// the audit decorator already opens around this route: each entry's write, its
// audit record and the notifications it owes join that one transaction, and an
// entry that fails returns an error which rolls the whole of it away.
func performTransaction(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	if err := negotiate(request, bodyMediaTypes); err != nil {
		return refuse(request, err)
	}

	if _, err := baseURL(request); err != nil {
		return refuse(request, err)
	}

	body, err := io.ReadAll(http.MaxBytesReader(
		request.Response, request.Request.Body, maximumBodyBytes))
	if err != nil {
		return refuse(request, readFailure(err))
	}

	submitted, err := fhir.ReadBundle(body)
	if err != nil {
		return refuse(request, err)
	}

	if len(submitted.Entry) > maximumEntries {
		return refuse(request, errTooManyEntries)
	}

	answers, err := performEntries(request, submitted)
	if err != nil {
		return refuse(request, err)
	}

	return respondBody(request, http.StatusOK, answers)
}

// performEntries runs every entry in the order R4 performs them, collecting the
// answers in the order the client submitted them.
func performEntries(request *core.RequestEvent, submitted fhir.SubmittedBundle) ([]byte, error) {
	base, err := baseURL(request)
	if err != nil {
		return nil, err
	}

	// Every identity is settled before anything is performed.
	//
	// R4 performs deletes, then creates, then updates — so an entry created
	// early can reference one updated late, and resolving as it went would leave
	// that reference pointing at a placeholder nobody had claimed yet. Minting
	// the ids first makes the order a matter of when rows are written rather
	// than of what they may say about each other.
	assigned, identities, err := settleIdentities(request, submitted)
	if err != nil {
		return nil, err
	}

	// The entries write their own records and must not settle the one the
	// decorator writes for the transaction. That slot exists because a create
	// mints an id no URL carries — one interaction, one resource — and a
	// transaction is about neither: a row naming whichever entry happened to run
	// last would say the transaction was about that resource.
	asked := request.Request
	defer func() { request.Request = asked }()

	request.Request = asked.WithContext(
		context.WithValue(asked.Context(), settledKey{}, &settled{}))

	answers := make([]fhir.BundleEntry, len(submitted.Entry))

	for _, index := range submitted.Ordered() {
		answer, err := performEntry(request, submitted.Entry[index], base, assigned, identities[index])
		if err != nil {
			// One entry failing ends the transaction. The error carries which
			// entry it was, because a client reading "not found" against a
			// Bundle of forty has nowhere to look.
			return nil, fmt.Errorf("entry %d: %w", index, err)
		}

		answers[index] = answer
	}

	encoded, err := json.Marshal(fhir.NewTransactionResponse(answers))
	if err != nil {
		return nil, fmt.Errorf("ilavrita: encode a transaction response: %w", err)
	}

	return encoded, nil
}

// settleIdentities works out what every entry will be stored under, before any
// of them is.
//
// A create's identity is minted here; an update's and a delete's are stated by
// the entry's own url. The map it returns is from each entry's placeholder to
// the reference a reader follows, which is what every entry's content is then
// resolved against.
func settleIdentities(
	request *core.RequestEvent, submitted fhir.SubmittedBundle,
) (map[string]string, []storage.ResourceKey, error) {
	assigned := map[string]string{}
	identities := make([]storage.ResourceKey, len(submitted.Entry))

	for index, entry := range submitted.Entry {
		resourceType, id, err := addressedBy(entry.Request.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("entry %d: %w", index, err)
		}

		if entry.Request.Method == fhir.VerbPost {
			if id, err = mintLogicalID(); err != nil {
				return nil, nil, err
			}
		}

		if id == "" {
			return nil, nil, fmt.Errorf("entry %d: %w", index, errEntryNotSupported)
		}

		key, err := storage.NewResourceKey(
			storage.ProjectID(projectOf(request)), resourceType, id)
		if err != nil {
			return nil, nil, fmt.Errorf("entry %d: %w", index, err)
		}

		identities[index] = key

		if entry.FullURL != "" {
			assigned[entry.FullURL] = string(resourceType) + "/" + string(id)
		}
	}

	return assigned, identities, nil
}

// projectOf is the Project every entry is stored in, which is the caller's own.
func projectOf(request *core.RequestEvent) string {
	session, found, err := serving.session(request)
	if err != nil || !found {
		return ""
	}

	return string(session.Project())
}

// performEntry runs one entry and describes what it did.
func performEntry(
	request *core.RequestEvent,
	entry fhir.SubmittedEntry,
	base string,
	assigned map[string]string,
	key storage.ResourceKey,
) (fhir.BundleEntry, error) {
	// Every entry is decided on its own. A Bundle is not a way to perform an
	// interaction the caller could not have performed one at a time.
	held, err := permittedFor(request, key.Type, entry.Request.Method)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	content, _, err := fhir.Resolved(entry.Resource, assigned)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	ctx := request.Request.Context()

	switch entry.Request.Method {
	case fhir.VerbPost:
		return createdEntry(ctx, held, key, content, base)
	case fhir.VerbPut:
		return replacedEntry(ctx, held, key, content, base)
	case fhir.VerbDelete:
		return deletedEntry(ctx, held, key)
	default:
		return fhir.BundleEntry{}, errEntryNotSupported
	}
}

// createdEntry performs one create at the identity the transaction settled on.
func createdEntry(
	ctx context.Context, held granted, key storage.ResourceKey, content json.RawMessage, base string,
) (fhir.BundleEntry, error) {
	stated, err := statedUnder(content, key.ID)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	record, err := held.create(ctx, key, stated)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	if err := recordEntry(ctx, held, record.Key, audit.ActionWrite); err != nil {
		return fhir.BundleEntry{}, err
	}

	return answeredEntry(base, record, http.StatusCreated), nil
}

// replacedEntry performs one update at the identity the entry named.
//
// A PUT at an identity nothing holds creates it, which is what every other
// update route here does and what the CapabilityStatement declares with
// updateCreate. A transaction stating a Patient and an Observation about them
// is the ordinary case, and the Patient usually does not exist yet.
func replacedEntry(
	ctx context.Context, held granted, key storage.ResourceKey, content json.RawMessage, base string,
) (fhir.BundleEntry, error) {
	stated, err := statedUnder(content, key.ID)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	var (
		record storage.ResourceRecord
		status = http.StatusOK
	)

	switch _, err := held.resources.Read(ctx, held.scope, key); {
	case err == nil:
		record, err = held.replace(ctx, key, stated, "")
		if err != nil {
			return fhir.BundleEntry{}, err
		}

	case errors.Is(err, storage.ErrDeleted), errors.Is(err, storage.ErrNotFound):
		record, err = held.create(ctx, key, stated)
		if err != nil {
			return fhir.BundleEntry{}, err
		}

		status = http.StatusCreated

	default:
		return fhir.BundleEntry{}, err
	}

	if err := recordEntry(ctx, held, record.Key, audit.ActionWrite); err != nil {
		return fhir.BundleEntry{}, err
	}

	return answeredEntry(base, record, status), nil
}

// deletedEntry performs one delete.
func deletedEntry(
	ctx context.Context, held granted, key storage.ResourceKey,
) (fhir.BundleEntry, error) {
	// A transaction entry states no precondition: R4 puts one in the entry's
	// own request, and this build does not read it yet.
	if err := held.remove(ctx, key, precondition{}); err != nil {
		return fhir.BundleEntry{}, err
	}

	if err := recordEntry(ctx, held, key, audit.ActionDelete); err != nil {
		return fhir.BundleEntry{}, err
	}

	return fhir.BundleEntry{
		Response: &fhir.EntryResponse{Status: entryStatus(fhir.VerbDelete)},
	}, nil
}

// answeredEntry describes what one write produced.
func answeredEntry(base string, record storage.ResourceRecord, status int) fhir.BundleEntry {
	return fhir.BundleEntry{
		FullURL: resourceURL(base, record.Key),
		Response: &fhir.EntryResponse{
			Status:       statusLine(status),
			ETag:         weakETag(record.Version),
			LastModified: instantOf(record.LastUpdated),
		},
	}
}

// statusLine writes a status the way a Bundle entry states one.
func statusLine(status int) string {
	return fmt.Sprintf("%d %s", status, http.StatusText(status))
}

// addressedBy reads which resource one entry's request url names.
func addressedBy(held string) (storage.ResourceType, storage.LogicalID, error) {
	trimmed := strings.Trim(strings.TrimSpace(held), "/")
	if trimmed == "" {
		return "", "", errEntryNotSupported
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) > 2 {
		return "", "", errEntryNotSupported
	}

	resourceType := storage.ResourceType(parts[0])
	if !fhir.ServesResourceType(string(resourceType)) {
		return "", "", unknownResourceType
	}

	if len(parts) == 1 {
		return resourceType, "", nil
	}

	return resourceType, storage.LogicalID(parts[1]), nil
}

// permittedFor decides one entry against the caller's own standing, for the
// actions the method it names needs.
func permittedFor(
	request *core.RequestEvent, resourceType storage.ResourceType, method fhir.HTTPVerb,
) (granted, error) {
	switch method {
	case fhir.VerbPost, fhir.VerbPut:
		return permit(request, resourceType, writeActions)
	case fhir.VerbDelete:
		return permit(request, resourceType, deleteActions)
	default:
		return granted{}, errEntryNotSupported
	}
}

// statedUnder writes the identity a resource is stored under into its own body,
// which is what every other write path does before storing one.
func statedUnder(content json.RawMessage, id storage.LogicalID) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &fields); err != nil {
		return nil, unreadableBody
	}

	return storedResource(fields, id)
}

// recordEntry writes one entry's audit event, inside the transaction that
// performed it.
//
// A Bundle is several interactions, so it is several records: one row saying "a
// transaction happened" would answer nothing an incident asks. The decorator
// around this route writes that one as well, which is the act; these are its
// parts.
func recordEntry(
	ctx context.Context, held granted, key storage.ResourceKey, action audit.Action,
) error {
	if serving == nil || serving.audits == nil {
		return nil
	}

	return serving.record(ctx, audit.EventConfig{
		Project:    key.Project,
		Principal:  held.standing.Principal,
		Membership: held.standing.Membership,
		Action:     action,
		Resource:   project.ProfileRef{Type: key.Type, ID: key.ID},
		Outcome:    audit.OutcomeAllowed,
		Reason:     audit.ReasonNone,
	})
}
