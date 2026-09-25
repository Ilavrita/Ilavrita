"""Hold this build's validator to a second implementation's verdict.

Both validators judge the same guide examples; a disagreement is the finding.
Ours refusing what HL7's accepts is a bug in ours, and the reverse is a rule we
do not apply yet. Either way somebody has to look.

Needs docker and a guide fetched by scripts/ig/fetch.sh.
"""

import collections
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

IMAGE = "infernocommunity/fhir-validator-service:latest"
CONTAINER = "ilavrita-ig-crosscheck"
PORT = os.environ.get("ILAVRITA_IG_VALIDATOR_PORT", "4568")
GUIDE = os.environ.get("ILAVRITA_PROFILE_DIR", ".ig/ndhm/package")
TARBALL = os.environ.get("ILAVRITA_IG_TARBALL", ".ig/ndhm/package.tgz")
EXAMPLES = os.environ.get("ILAVRITA_IG_EXAMPLES", ".ig/ndhm/examples")
VALIDATOR = f"http://localhost:{PORT}"

BASE_PROFILE = "http://hl7.org/fhir/StructureDefinition/"


def docker(*arguments):
    binary = shutil.which("docker") or os.path.expanduser("~/.docker/bin/docker")

    return subprocess.run([binary, *arguments], capture_output=True, text=True)


def ask(path, body=None, content_type="application/fhir+json", timeout=600):
    sent = urllib.request.Request(
        VALIDATOR + path, data=body, method="POST" if body is not None else "GET",
        headers={"Content-Type": content_type} if body is not None else {},
    )

    with urllib.request.urlopen(sent, timeout=timeout) as answer:
        return json.loads(answer.read())


def validate(content, profile):
    return ask(f"/validate?profile={urllib.parse.quote(profile, safe='')}", content.encode())


def wait_until_ready():
    for _ in range(90):
        try:
            validate('{"resourceType":"Patient"}', BASE_PROFILE + "Patient")

            return
        except (OSError, urllib.error.URLError):
            # The container accepts connections before it answers, so a reset
            # here means not ready rather than not coming.
            time.sleep(10)

    sys.exit("the validator never came up")


def load_the_guide():
    """Hand the guide to the validator, which fetches packages rather than reading disk.

    A mounted directory is not a loaded guide: the service resolves profiles
    from packages it was given through this endpoint and from nowhere else.
    """
    with open(TARBALL, "rb") as package:
        ask("/igs", package.read(), content_type="application/gzip")


# A resource the guide's Patient profile refuses and the base Patient accepts:
# the profile makes identifier required, the base leaves it optional. The answer
# names the element only if the guide is loaded AND applied.
CONTROL_PROFILE = "https://nrces.in/ndhm/fhir/r4/StructureDefinition/Patient"
CONTROL_ELEMENT = "Patient.identifier"


def check_the_guide_is_applied():
    """Refuse to report agreement the validator was never in a position to give.

    A validator without the guide accepts every example, which is
    indistinguishable from agreeing with us. So ask it something only a loaded
    guide answers, and stop if it cannot.
    """
    try:
        answer = validate('{"resourceType":"Patient"}', CONTROL_PROFILE)
    except urllib.error.HTTPError as refused:
        sys.exit(f"the validator does not hold {CONTROL_PROFILE}: HTTP {refused.code}")

    if CONTROL_ELEMENT not in json.dumps(answer):
        sys.exit(
            f"the validator did not apply {CONTROL_PROFILE}, so its agreement means nothing.\n"
            f"Expected an issue naming {CONTROL_ELEMENT}. Answer was:\n" + json.dumps(answer)[:2000]
        )


def examples():
    held = {}

    for name in sorted(os.listdir(EXAMPLES)):
        if not name.endswith(".json"):
            continue

        with open(os.path.join(EXAMPLES, name), encoding="utf-8") as content:
            body = content.read()

        try:
            resource = json.loads(body)
        except json.JSONDecodeError:
            continue

        if not resource.get("resourceType"):
            continue

        claimed = [
            url for url in (resource.get("meta") or {}).get("profile") or []
            if isinstance(url, str) and url
        ] or [BASE_PROFILE + resource["resourceType"]]

        held[name] = (body, claimed)

    return held


# Why a finding of HL7's is not held against this build. Everything else is a
# disagreement somebody has to look at, so a class that quietly grows shows up
# in the count rather than in nothing.
#
# Each reason is matched on the validator's own message. A wording change breaks
# the match and the finding reappears as unexplained, which is the safe way for
# this to fail.
ACCEPTED = (
    (
        "no terminology server: CI runs the validator offline, so a required "
        "binding to an external code system cannot be confirmed",
        (
            "was not found in the value set",
            "The System URI could not be determined",
            "Unable to expand",
            "Unable to validate code",
        ),
    ),
    (
        "downstream of the above: an entry whose only fault is an unconfirmable "
        "code matches no slice of its bundle. Every entry named this way was "
        "checked alone and validates apart from terminology",
        ("Unable to find a profile match",),
    ),
    (
        "vital signs by code: HL7's validator applies R4's vital-signs profiles "
        "to an Observation whose code is one of theirs. This build applies the "
        "profiles a resource declares in meta.profile and no others",
        tuple("(from http://hl7.org/fhir/StructureDefinition/" + name for name in (
            "vitalsigns", "resprate", "heartrate", "oxygensat", "bodytemp",
            "bodyheight", "bodyweight", "headcircum", "bmi", "bp",
        )),
    ),
    (
        "extension context: this build does not check where an extension may appear",
        ("is not allowed to be used at this point",),
    ),
)


def why_accepted(text):
    for reason, patterns in ACCEPTED:
        if any(pattern in text for pattern in patterns):
            return reason

    return None


def refused_by_hl7(held):
    """Every profile an example claims, judged one at a time.

    The endpoint takes a single profile, so an example claiming two is two
    questions. Refusing any one of them is refusing the example.

    Returns the examples refused for a reason this build does not accept, and
    how many findings each accepted reason covered.
    """
    refused, absorbed = {}, collections.Counter()

    for index, (name, (body, claimed)) in enumerate(sorted(held.items()), start=1):
        for profile in claimed:
            answer = validate(body, profile)

            for issue in answer.get("issue") or []:
                if issue.get("severity") not in ("error", "fatal"):
                    continue

                text = (issue.get("details") or {}).get("text", "")
                reason = why_accepted(text)

                if reason:
                    absorbed[reason] += 1
                else:
                    refused.setdefault(name, []).append(text)

        if index % 25 == 0:
            print(f"    {index}/{len(held)} judged by HL7", flush=True)

    return refused, absorbed


def refused_by_ilavrita():
    run = subprocess.run(
        ["go", "test", "./packages/validate/", "-run", "GuidesOwnExamples", "-v", "-count=1"],
        capture_output=True, text=True,
        env={**os.environ, "ILAVRITA_PROFILE_DIR": GUIDE},
    )

    named = {
        line.split(".json:")[0].split()[-1] + ".json"
        for line in run.stdout.splitlines()
        if ".json:" in line and "guide_test.go" in line
    }

    # A test that did not run refuses nothing, which reads as agreement. A build
    # error or a panic lands here, and it is not a verdict.
    if run.returncode != 0 and not named:
        sys.exit("our validator did not run:\n" + (run.stderr or run.stdout).strip())

    return named


def main():
    for needed in (GUIDE, TARBALL, EXAMPLES):
        if not os.path.exists(needed):
            sys.exit(f"no {needed}; run scripts/ig/fetch.sh")

    docker("rm", "-f", CONTAINER)
    started = docker("run", "-d", "--rm", "--name", CONTAINER, "-p", f"{PORT}:4567", "-e", "DISABLE_TX=true", IMAGE)
    if started.returncode != 0:
        sys.exit(f"could not start the validator: {started.stderr.strip()}")

    try:
        wait_until_ready()
        load_the_guide()
        check_the_guide_is_applied()

        held = examples()
        print(f"==> {len(held)} examples, two validators", flush=True)

        theirs, absorbed = refused_by_hl7(held)
        ours = refused_by_ilavrita()
    finally:
        docker("rm", "-f", CONTAINER)

    for reason, count in sorted(absorbed.items()):
        print(f"    {count:4d} finding(s) accepted — {reason}")

    only_ours = sorted(ours - set(theirs))
    only_theirs = sorted(set(theirs) - ours)

    # Ours refusing what HL7 accepts is a bug in ours, and no reason excuses it.
    for name in only_ours:
        print(f"we refuse what HL7 accepts: {name}")

    for name in only_theirs:
        print(f"HL7 refuses what we accept: {name}")
        for text in theirs[name][:3]:
            print(f"        {text[:160]}")

    if only_ours or only_theirs:
        sys.exit(f"{len(only_ours) + len(only_theirs)} example(s) the two validators judge differently")

    print(f"both validators agree on all {len(held)} examples ({len(ours)} refused by each)")


if __name__ == "__main__":
    main()
