package main

import (
	"net/http"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

const (
	healthPath  = "/healthz"
	versionPath = "/version"
)

type healthReport struct {
	Status string `json:"status"`
}

type versionReport struct {
	Version     string `json:"version"`
	Revision    string `json:"revision"`
	FHIRVersion string `json:"fhirVersion"`
}

// Operational endpoints live outside the FHIR namespace so that monitoring a
// deployment never depends on the healthcare API surface (FR-033).
func registerOperationalRoutes(routes *router.Router[*core.RequestEvent]) {
	routes.GET(healthPath, reportHealth)
	routes.GET(versionPath, reportVersion)
}

func reportHealth(request *core.RequestEvent) error {
	return request.JSON(http.StatusOK, healthReport{Status: "ok"})
}

func reportVersion(request *core.RequestEvent) error {
	return request.JSON(http.StatusOK, versionReport{
		Version:     version,
		Revision:    revision,
		FHIRVersion: fhir.Release,
	})
}
