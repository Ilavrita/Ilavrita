"""Compare the OpenAPI description against a running Ilavrita server.

Every documented route is requested, and its status, content type and top-level
response fields are checked against the description. A route the server no longer
serves the way the description claims is a failure, not a warning: a
specification that has drifted is worse than none, because callers trust it and
are wrong.

Invoked by scripts/verify-openapi.sh, which bundles the description and starts
the server first.
"""

import json
import os
import sys
import urllib.error
import urllib.request

BASE_URL = f"http://127.0.0.1:{os.environ.get('ILAVRITA_VERIFY_PORT', '8111')}"

# Templated paths need a concrete value, so one sample stands in for the family.
SAMPLE_PATHS = {"/fhir/R4/{path}": "/fhir/R4/Patient/example"}


def request(path):
    try:
        response = urllib.request.urlopen(BASE_URL + path, timeout=10)
        return response.status, response.headers.get("Content-Type", ""), json.loads(response.read())
    except urllib.error.HTTPError as failure:
        return failure.code, failure.headers.get("Content-Type", ""), json.loads(failure.read())


def documented_response(response_spec):
    """Return the media type and top-level fields a response should carry."""
    media_type, body = next(iter(response_spec["content"].items()))
    return media_type, set(body["schema"].get("properties", {}))


def check(path, operation):
    request_path = SAMPLE_PATHS.get(path, path)
    problems = []

    for status_text, response_spec in operation["responses"].items():
        expected_status = int(status_text)
        media_type, fields = documented_response(response_spec)
        status, content_type, body = request(request_path)

        if status != expected_status:
            problems.append(f"status {status}, description says {expected_status}")
        if media_type not in content_type:
            problems.append(f"content type {content_type!r} is not {media_type}")

        missing = fields - set(body)
        if missing:
            problems.append(f"response is missing {sorted(missing)}")

    return request_path, problems


def main():
    bundled = sys.argv[1] if len(sys.argv) > 1 else "api/openapi.bundled.json"
    with open(bundled, encoding="utf-8") as handle:
        spec = json.load(handle)

    drifted = 0
    for path, operations in spec["paths"].items():
        request_path, problems = check(path, operations["get"])
        if problems:
            drifted += 1
            print(f"FAIL  {request_path}")
            for problem in problems:
                print(f"      {problem}")
        else:
            print(f"ok    {request_path}")

    if drifted:
        print(f"\n{drifted} route(s) drifted from the OpenAPI description", file=sys.stderr)
        return 1

    print("\nThe OpenAPI description matches the running server")
    return 0


if __name__ == "__main__":
    sys.exit(main())
