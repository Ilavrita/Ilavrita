package main

import (
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

const (
	metadataPath     = "/metadata"
	everythingElse   = "/{path...}"
	contentTypeField = "Content-Type"
)

// The FHIR surface is owned by Ilavrita. PocketBase collections, admin routes
// and error shapes must never appear beneath this base path (FR-007, FR-029).
func registerFHIRRoutes(routes *router.Router[*core.RequestEvent]) {
	base := routes.Group(fhir.BasePath)
	base.GET(metadataPath, describeCapabilities)
	base.Any(everythingElse, rejectUnimplemented)
}

// describeCapabilities publishes what this build actually supports.
func describeCapabilities(request *core.RequestEvent) error {
	return respondFHIR(request, http.StatusOK, fhir.NewCapabilityStatement(version))
}

// rejectUnimplemented answers every FHIR route with no behaviour yet, as an
// OperationOutcome so clients only ever parse FHIR-shaped failures.
func rejectUnimplemented(request *core.RequestEvent) error {
	outcome := fhir.NewOperationOutcome(
		fhir.SeverityError,
		fhir.CodeNotSupported,
		"This interaction is not implemented. See the CapabilityStatement at "+fhir.BasePath+metadataPath+".",
	)

	return respondFHIR(request, http.StatusNotImplemented, outcome)
}

func respondFHIR(request *core.RequestEvent, status int, payload any) error {
	request.Response.Header().Set(contentTypeField, fhir.ContentType)

	return request.JSON(status, payload)
}
