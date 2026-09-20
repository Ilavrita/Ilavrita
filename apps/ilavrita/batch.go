package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/pocketbase/pocketbase/core"
)

// A batch is several interactions that stand or fall on their own.
//
// It is the other half of the pair a transaction belongs to, and the difference
// is the whole point of having both. A transaction is for things that are only
// true together — a Patient and their Observations. A batch is for a day's
// unrelated writes, where a client wants the ones that worked to have worked.
//
// Each entry runs inside a savepoint. The request already holds a transaction,
// so rolling the whole of it back for one bad entry would make this a
// transaction, and rolling nothing back would leave half an entry behind.

// performBatchEntries runs every entry, keeping what each one answered.
//
// Nothing here returns an error for an entry that failed: a batch answers for
// all of them, and which failed is the answer. What does fail the request is
// this server being unable to record what happened, because then the bundle it
// would return is not what the store holds.
func performBatchEntries(
	request *core.RequestEvent, submitted fhir.SubmittedBundle,
) ([]byte, error) {
	base, err := baseURL(request)
	if err != nil {
		return nil, err
	}

	assigned, identities, err := settleIdentities(request, submitted)
	if err != nil {
		return nil, err
	}

	// The entries write their own audit records and must not settle the one the
	// decorator writes for the batch, for the same reason a transaction's do
	// not: a row naming whichever entry ran last would say the batch was about
	// that resource.
	asked := request.Request
	defer func() { request.Request = asked }()

	request.Request = asked.WithContext(
		context.WithValue(asked.Context(), settledKey{}, &settled{}))

	answers := make([]fhir.BundleEntry, len(submitted.Entry))

	for _, index := range submitted.Ordered() {
		answers[index] = batchedEntry(request, submitted.Entry[index], base, assigned, identities[index])
	}

	encoded, err := json.Marshal(fhir.NewBatchResponse(answers))
	if err != nil {
		return nil, fmt.Errorf("ilavrita: encode a batch response: %w", err)
	}

	return encoded, nil
}

// batchedEntry runs one entry and describes what it did, whether or not that
// was what the client hoped.
func batchedEntry(
	request *core.RequestEvent,
	entry fhir.SubmittedEntry,
	base string,
	assigned map[string]string,
	settled settledEntry,
) fhir.BundleEntry {
	if settled.err != nil {
		return refusedEntry(settled.err)
	}

	var answer fhir.BundleEntry

	err := withinEntry(request, func() error {
		performed, err := performEntry(request, entry, base, assigned, settled)
		if err != nil {
			return err
		}

		answer = performed

		return nil
	})
	if err == nil {
		return answer
	}

	// The entry is described by the refusal it earned, rendered the same way a
	// lone request would have been: a client reading one entry's failure should
	// not have to learn a second vocabulary for it.
	return refusedEntry(err)
}

// refusedEntry describes one entry by the refusal it earned, rendered the way a
// lone request would have been: a client reading one entry's failure should not
// have to learn a second vocabulary for it.
func refusedEntry(err error) fhir.BundleEntry {
	refused := translate(err)
	outcome := fhir.NewOperationOutcome(fhir.SeverityError, refused.code, refused.detail)

	return fhir.BundleEntry{
		Response: &fhir.EntryResponse{
			Status:  statusLine(refused.status),
			Outcome: &outcome,
		},
	}
}

// withinEntry runs one entry so its failure undoes only what it wrote.
func withinEntry(request *core.RequestEvent, work func() error) error {
	if serving == nil || serving.resources == nil {
		return work()
	}

	return serving.resources.WithinSavepoint(request.Request.Context(),
		func(ctx context.Context) error {
			held := request.Request
			request.Request = held.WithContext(ctx)

			defer func() { request.Request = held }()

			return work()
		})
}
