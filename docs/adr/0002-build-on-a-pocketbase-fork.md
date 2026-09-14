# 2. Build on a PocketBase fork

Date: 2026-09-14

## Status

Accepted

## Context

Ilavrita needs a Go HTTP server, SQLite integration, migrations, hooks, file
handling, scheduling and administrative primitives before it can begin being a
FHIR server. Writing those is months of work that produces no healthcare value.

PocketBase provides all of them, is MIT licensed, and runs as a single binary
with no external database — which matches the deployment model Ilavrita targets
for clinics, pilots and edge devices.

Depending on upstream directly would mean living with upstream's release cadence
and having nowhere to carry changes we need.

## Decision

Build on a fork of PocketBase, maintained at
[`Ilavrita/pocketbase`](https://github.com/Ilavrita/pocketbase).

PocketBase is the runtime foundation only. It is not the public API, and it is
not an architectural ceiling. Its MIT copyright and permission notice are
preserved.

## Consequences

We inherit a working runtime and the responsibility of keeping the fork current
with upstream security fixes.

Because PocketBase is visible in the process, the boundary has to be enforced
rather than assumed: no PocketBase collection, admin route or error shape may
appear through `/fhir/R4`. See record 4.
