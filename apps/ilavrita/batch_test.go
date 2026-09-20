package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
)

// batchOf writes one batch bundle.
func batchOf(entries ...string) string {
	return `{"resourceType":"Bundle","type":"batch","entry":[` +
		strings.Join(entries, ",") + `]}`
}

// batchResponse decodes what a batch answered.
func batchResponse(t *testing.T, routes http.Handler, body string) fhir.Bundle {
	t.Helper()

	answer := submitting(t, routes, body)
	assertStatus(t, answer, http.StatusOK)

	held := responseBundleOfType(t, answer, fhir.BundleBatchResponse)

	return held
}

// TestABatchKeepsTheEntriesThatWorked.
//
// This is the whole difference from a transaction, and the reason both exist. A
// client sending a day's unrelated writes wants the ones that worked to have
// worked; a transaction would throw them all away for one bad entry.
func TestABatchKeepsTheEntriesThatWorked(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	held := batchResponse(t, routes, batchOf(
		entryOf("", "POST", "Organization", valid("Organization", nil)),
		// A type this server does not serve: this entry cannot be performed.
		entryOf("", "POST", "Appointment", `{"resourceType":"Appointment"}`),
		entryOf("", "POST", "Organization", valid("Organization", nil)),
	))

	if len(held.Entry) != 3 {
		t.Fatalf("a batch of three answered %d entries", len(held.Entry))
	}

	for index, want := range []string{"201", "4", "201"} {
		entry := held.Entry[index]

		if entry.Response == nil {
			t.Errorf("entry %d described nothing", index)

			continue
		}

		if !strings.HasPrefix(entry.Response.Status, want) {
			t.Errorf("entry %d answered %q, want %s…", index, entry.Response.Status, want)
		}
	}

	// The failed entry says why, because a status alone names which entry went
	// wrong and not what about it.
	if held.Entry[1].Response.Outcome == nil {
		t.Error("the refused entry carries no outcome")
	}

	// And the two that worked are there, which a transaction would have undone.
	if found := searchedOrganizations(t, routes); found != 2 {
		t.Errorf("%d Organizations survived the batch, want 2", found)
	}
}

// TestAFailedBatchEntryLeavesNothingOfItsOwnBehind. Each entry runs inside a
// savepoint, so an entry that failed halfway undoes its own writes without
// touching the entries beside it.
func TestAFailedBatchEntryLeavesNothingOfItsOwnBehind(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	// An Organization that violates org-1, which is refused after the entry has
	// begun rather than before the bundle is read.
	held := batchResponse(t, routes, batchOf(
		entryOf("", "POST", "Organization", valid("Organization", nil)),
		entryOf("", "POST", "Organization", `{"resourceType":"Organization"}`),
	))

	if len(held.Entry) != 2 {
		t.Fatalf("a batch of two answered %d entries", len(held.Entry))
	}

	if !strings.HasPrefix(held.Entry[0].Response.Status, "201") {
		t.Errorf("the good entry answered %q", held.Entry[0].Response.Status)
	}

	if !strings.HasPrefix(held.Entry[1].Response.Status, "4") {
		t.Errorf("the bad entry answered %q", held.Entry[1].Response.Status)
	}

	if found := searchedOrganizations(t, routes); found != 1 {
		t.Errorf("%d Organizations exist, want the one that worked", found)
	}
}

// TestABatchIsDeclared, because a client reads the statement to know whether it
// may rely on one.
func TestABatchIsDeclared(t *testing.T) {
	routes := servingFHIR(t, everyAction)

	answer := call{method: http.MethodGet, path: fhir.BasePath + "/metadata"}.send(t, routes)
	assertStatus(t, answer, http.StatusOK)

	for _, code := range []string{"transaction", "batch"} {
		if !strings.Contains(answer.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("the statement does not declare %s", code)
		}
	}
}
