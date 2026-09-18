package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/validate"
	"github.com/pocketbase/pocketbase/core"
)

// Where a client asks what this server would make of a resource without storing
// it, and the name the CapabilityStatement declares it under.
const (
	validateOperationName = "validate"
	validatePath          = "/{resourceType}/$" + validateOperationName
)

// errInvalidResource reports a body this server will not store. It carries the
// report, so the refusal can say which elements rather than that something was
// wrong.
type errInvalidResource struct{ report validate.Report }

func (e errInvalidResource) Error() string { return "ilavrita: " + e.report.Error() }

// checkSubmission refuses a resource this server will not store.
//
// It runs on both a create and an update, on the content as the client sent it:
// a rule checked only on the way in is one PUT away from being no rule at all,
// and a Binary's payload is legitimately part of the body at this point.
func checkSubmission(key storage.ResourceKey, content json.RawMessage) error {
	report := validate.Resource(key.Type, content)
	if report.OK() {
		return nil
	}

	return errInvalidResource{report: report}
}

// validateResource answers what this server would make of a resource.
//
// R4 answers `200` whichever way it went: the operation was performed, and what
// it found is the OperationOutcome. A client reads the issues rather than the
// status, which is what lets one response carry a warning alongside a pass.
//
// It is authorized as a write of that type. Nothing is stored, but validating
// is what a client does before writing, and an endpoint that did this work for
// anybody who asked would be one that does work for anybody who asks.
func validateResource(request *core.RequestEvent) error {
	held, err := begin(request, writeActions)
	if err != nil {
		return refuse(request, err)
	}

	fields, err := submittedResource(request, held.resourceType)
	if err != nil {
		return refuse(request, err)
	}

	content, err := encode(fields)
	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusOK,
		validate.Resource(held.resourceType, content).Outcome())
}

// invalidResource renders a refused submission as the outcome that says which
// elements were wrong.
func invalidResource(err error) (fhir.OperationOutcome, bool) {
	var invalid errInvalidResource
	if !errors.As(err, &invalid) {
		return fhir.OperationOutcome{}, false
	}

	return invalid.report.Outcome(), true
}
