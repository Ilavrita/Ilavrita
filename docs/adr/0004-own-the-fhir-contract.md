# 4. Own the FHIR contract behind storage interfaces

Date: 2026-09-14

## Status

Accepted

## Context

The fastest way to ship a FHIR server on PocketBase is to model resources as
PocketBase collections and let its REST API serve them.

That approach fails on both axes that matter. It exposes PocketBase semantics —
collection names, error shapes, filter syntax — as the healthcare API, which is
not FHIR. And it couples business logic to SQLite, so the eventual move to a
clustered PostgreSQL backend becomes a rewrite rather than an addition.

## Decision

Ilavrita owns the public FHIR contract. Services depend on interfaces in
`packages/storage`, never on a database driver or on the PocketBase runtime.
SQLite-specific SQL stays inside `packages/storage/pocketbase`, which is the only
package permitted to import PocketBase — enforced by a `depguard` rule rather
than by convention.

Search follows the same rule: queries are parsed into a backend-independent plan,
and only the backend compiler knows SQL.

FHIR failures return `OperationOutcome`. Runtime and database errors are
translated at the boundary.

## Consequences

More code than exposing PocketBase directly, and a persistence layer that must be
written rather than inherited.

In exchange, a second storage backend is an addition rather than a rewrite, the
published API does not change when storage does, and internal detail cannot leak
to clients through an error path.
