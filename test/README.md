# Tests

Go unit tests live beside the code they cover. This directory holds the suites
that exercise a running server.

| Directory | Purpose |
| --- | --- |
| [`conformance`](conformance) | Asserts that advertised FHIR behaviour matches the specification |
| [`fixtures`](fixtures) | Shared FHIR resources used by tests |

Both are empty. They fill up as interactions ship, because a capability is not
finished until a test proves it.

Run the Go suite with `make test`.
