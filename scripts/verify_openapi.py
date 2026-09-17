"""Compare the OpenAPI description against a running Ilavrita server.

Every documented operation — every method on every path, not only GET — is
requested, and the status it answers with must be one the description declares;
that response's content type and top-level fields are then checked against it. A
route the server no longer serves the way the description claims is a failure,
not a warning: a specification that has drifted is worse than none, because
callers trust it and are wrong.

An operation may declare several responses — a read answers 200, 401 or 404
depending on who is asking — so only the one the server actually produced here
can be compared. An undeclared status is itself a failure, which is what keeps
this from becoming a check that passes on anything.

What this does not reach, stated so nobody reads a pass as more than it is: the
server under test is started with no ILAVRITA_DEV_PRINCIPAL, so nothing
identifies a caller and every FHIR interaction stops at 401. Only the refusal
half of the contract is compared here. The success responses (200, 201, 204) and
the headers beside them are asserted by the Go conformance suite in
apps/ilavrita, which wires an authorization decision that actually resolves —
against the behaviour, not against this document.

Invoked by scripts/verify-openapi.sh, which bundles the description and starts
the server first.
"""

import json
import os
import sys
import urllib.error
import urllib.request

BASE_URL = f"http://127.0.0.1:{os.environ.get('ILAVRITA_VERIFY_PORT', '8111')}"

# What a method that carries one needs as a body. The type matches the URL, so a
# request reaches authorization rather than stopping at the body check.
SAMPLE_BODIES = {
    "post": b'{"resourceType":"Organization"}',
    "put": b'{"resourceType":"Organization","id":"example"}',
}

# Templated paths need a concrete value, so one sample stands in for the family.
# The type is one the CapabilityStatement declares, because an undeclared one is
# not an endpoint and would exercise a different route's answer.
SAMPLE_PATHS = {
    "/fhir/R4/{resourceType}/{id}/{operation}": "/fhir/R4/Organization/example/$everything",
    "/fhir/R4/{resourceType}": "/fhir/R4/Organization",
    "/fhir/R4/{resourceType}/_history": "/fhir/R4/Organization/_history",
    "/fhir/R4/{resourceType}/{id}": "/fhir/R4/Organization/example",
    "/fhir/R4/{resourceType}/{id}/_history": "/fhir/R4/Organization/example/_history",
    "/fhir/R4/{resourceType}/{id}/_history/{vid}": "/fhir/R4/Organization/example/_history/1",
}


def request(method, path):
    body = SAMPLE_BODIES.get(method)
    headers = {"Content-Type": "application/fhir+json"} if body else {}
    sent = urllib.request.Request(
        BASE_URL + path, data=body, headers=headers, method=method.upper()
    )

    try:
        response = urllib.request.urlopen(sent, timeout=10)
        return response.status, response.headers.get("Content-Type", ""), read_body(response)
    except urllib.error.HTTPError as failure:
        return failure.code, failure.headers.get("Content-Type", ""), read_body(failure)


def read_body(response):
    """Return the decoded body, or an empty mapping for a response with none."""
    raw = response.read()
    return json.loads(raw) if raw else {}


def documented_response(response_spec):
    """Return the media type and top-level fields a response should carry.

    A response with no content at all, such as a 204, describes no body.
    """
    content = response_spec.get("content")
    if not content:
        return "", set()

    media_type, body = next(iter(content.items()))
    return media_type, set(body["schema"].get("properties", {}))


def check(method, path, operation):
    request_path = SAMPLE_PATHS.get(path, path)
    status, content_type, body = request(method, request_path)

    label = f"{method.upper()} {request_path}"

    declared = operation["responses"].get(str(status))
    if declared is None:
        described = sorted(operation["responses"])
        return label, [f"answered {status}, which the description does not declare; it declares {described}"]

    problems = []
    media_type, fields = documented_response(declared)

    if media_type and media_type not in content_type:
        problems.append(f"content type {content_type!r} is not {media_type}")

    missing = fields - set(body)
    if missing:
        problems.append(f"the {status} response is missing {sorted(missing)}")

    # An undocumented response field is drift in the other direction: the
    # description stopped describing what the server actually returns.
    undocumented = set(body) - fields
    if fields and undocumented:
        problems.append(f"the {status} response has undocumented fields {sorted(undocumented)}")

    return label, problems


def main():
    bundled = sys.argv[1] if len(sys.argv) > 1 else "api/openapi.bundled.json"
    with open(bundled, encoding="utf-8") as handle:
        spec = json.load(handle)

    drifted = 0
    for path, operations in spec["paths"].items():
        for method, operation in operations.items():
            if method == "parameters":
                continue

            label, problems = check(method, path, operation)
            if problems:
                drifted += 1
                print(f"FAIL  {label}")
                for problem in problems:
                    print(f"      {problem}")
            else:
                print(f"ok    {label}")

    if drifted:
        print(f"\n{drifted} operation(s) drifted from the OpenAPI description", file=sys.stderr)
        return 1

    print("\nThe OpenAPI description matches the running server")
    return 0


if __name__ == "__main__":
    sys.exit(main())
