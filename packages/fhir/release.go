package fhir

// Release is the FHIR specification version Ilavrita targets (PRD: R4 is the
// v0.1 compatibility baseline).
const Release = "4.0.1"

// ContentType is the only representation Ilavrita serves. XML is out of scope
// for v0.1 and must not be advertised until it is implemented.
const ContentType = "application/fhir+json"

// BasePath roots every FHIR route. Versioning the base path keeps a future FHIR
// release from disturbing existing clients.
const BasePath = "/fhir/R4"
