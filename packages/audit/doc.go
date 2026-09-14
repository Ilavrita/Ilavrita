// Package audit records security-relevant events.
//
// Audit evidence is written by the server and is independent of the FHIR
// AuditEvent resource, so a client can never edit away the record of its own
// actions (FR-030).
package audit
