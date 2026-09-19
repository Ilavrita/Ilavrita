"""Judge what Ilavrita puts on the wire with an implementation that is not ours.

The Go suite in apps/ilavrita asserts behaviour against its own reading of R4 —
the same reading that wrote the server. It cannot catch the two of them being
wrong together, and that is exactly what happened twice: a lastModified written
as an HTTP-date where R4 declares an instant, and an element written as {} where
R4 has no empty object. Both passed every test here and both are invalid FHIR.

So this runs the HL7 validator over the bytes the server actually produced. A
finding it reports is either a defect or one of the accepted ones below, each of
which names why it is accepted. Anything else fails the run.

Invoked by scripts/conformance.sh, which starts the validator and captures the
shapes first.
"""

import json
import os
import sys
import urllib.error
import urllib.request

VALIDATOR = os.environ.get("ILAVRITA_VALIDATOR_URL", "http://localhost:4567")

# Findings that are not defects. Each is matched on a substring of the issue's
# own text, and each says why it stands — an entry without a reason is a
# suppression, and a suppression nobody can argue with is how a conformance gate
# stops meaning anything.
ACCEPTED = [
    (
        "not found in the value set 'MimeType'",
        "The validator cannot expand urn:ietf:bcp:13; application/fhir+json is "
        "R4's own declared format value.",
    ),
    (
        "The System URI could not be determined for the code 'application/fhir+json'",
        "Same BCP-13 limitation, reported a second way.",
    ),
    (
        "Constraint failed: org-1",
        "This build checks no FHIRPath invariant, which docs/known-limitations.md "
        "states. The fixture is left invalid on purpose so the gap stays visible.",
    ),
]


def validate(path, profile):
    """Return the issues the validator reports for one captured shape."""
    with open(path, "rb") as held:
        body = held.read()

    sent = urllib.request.Request(
        f"{VALIDATOR}/validate?profile=http://hl7.org/fhir/StructureDefinition/{profile}",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )

    try:
        with urllib.request.urlopen(sent, timeout=120) as answer:
            return json.loads(answer.read()).get("issue", [])
    except urllib.error.URLError as failure:
        sys.exit(f"the validator at {VALIDATOR} could not be reached: {failure}")


def accepted_for(text):
    """Return why a finding is accepted, or None if it is a defect."""
    for fragment, reason in ACCEPTED:
        if fragment in text:
            return reason

    return None


def describe(issue):
    """Return the element a finding is about and what it says."""
    where = (issue.get("expression") or issue.get("location") or ["?"])[0]

    return where, issue.get("details", {}).get("text", "")


def main(directory):
    # The profile each shape is judged against is the second-to-last part of its
    # own name, so capturing a new shape needs no edit here.
    shapes = sorted(
        held for held in os.listdir(directory) if held.endswith(".json")
    )
    if not shapes:
        sys.exit(f"nothing was captured into {directory}")

    defects, waived = [], 0

    for held in shapes:
        label, profile, _ = held.rsplit(".", 2)

        for issue in validate(os.path.join(directory, held), profile):
            if issue.get("severity") not in ("error", "fatal"):
                continue

            where, text = describe(issue)
            reason = accepted_for(text)

            if reason:
                waived += 1
                continue

            defects.append((label, where, text))

        print(f"  checked {label} against {profile}")

    print(f"\n{len(shapes)} shape(s) validated, {waived} accepted finding(s)")

    if not defects:
        print("Every shape Ilavrita puts on the wire is valid FHIR R4")

        return 0

    print(f"\n{len(defects)} defect(s):")
    for label, where, text in defects:
        print(f"  {label}: {where}\n    {text}")

    return 1


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: conformance.py <directory of captured shapes>")

    sys.exit(main(sys.argv[1]))
