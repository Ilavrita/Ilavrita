## What this changes

<!-- What the change does, and why it is needed. The why is the interesting part. -->

## Related issue

<!-- Closes #123, or "none" for a small fix. -->

## How it was verified

<!-- Tests you added, commands you ran, requests you made. "CI is green" on its own is not verification. -->

## Checklist

- [ ] Commits are single-line Conventional Commits and signed off (`git commit -s`)
- [ ] New behaviour has tests
- [ ] `make lint` and `make test` pass
- [ ] Documentation updated, if behaviour changed
- [ ] No patient data in code, tests, fixtures or logs

## Architectural boundaries

Tick what applies, or state why it does not.

- [ ] No PocketBase concept is visible through `/fhir/R4`
- [ ] Only `packages/storage/pocketbase` imports the PocketBase runtime
- [ ] No SQL outside the storage backend
- [ ] The CapabilityStatement advertises only interactions that are implemented and tested
- [ ] No resource body is written to logs

## Breaking change

<!-- Describe the break and the migration path, or "none". -->
