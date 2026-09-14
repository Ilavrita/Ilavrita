// Package project defines the isolation boundary every stored record carries.
//
// A Project is the primary tenant, security, configuration and FHIR data
// boundary. Ilavrita's hierarchy runs Instance -> Super Project -> Project ->
// ProjectMembership -> Profile -> Resource, and every normal resource belongs to
// exactly one Project (FR-045).
//
// The Project identifier is derived server-side and attached to resources,
// versions, search indexes, files, audit events and jobs. It is never read from
// a client-supplied FHIR field, because cross-Project exposure of health data is
// the most severe failure this system can have (FR-026, FR-046, R-004).
//
// Membership, administrative status and the Super Project arrive in Phase 2.
// They share this boundary rather than introducing a second one.
package project
