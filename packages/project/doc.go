// Package tenancy defines the isolation boundary every stored record carries.
//
// A tenant identifier is derived server-side and attached to resources,
// versions, search indexes, files and audit events. It is never read from a
// client-supplied FHIR field, because cross-tenant exposure of health data is
// the most severe failure this system can have (FR-026, R-004).
package tenancy
