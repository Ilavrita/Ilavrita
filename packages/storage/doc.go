// Package storage is the persistence boundary the FHIR services depend on.
//
// Services talk to these interfaces and never to a database driver or to the
// PocketBase runtime. That indirection is what lets a PostgreSQL backend be
// added later without rewriting FHIR behaviour (FR-025, R-002).
package storage
