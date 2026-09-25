"""Hold this build's validator to a second implementation's verdict.

Both validators judge the same guide examples; a disagreement is the finding.
Ours refusing what HL7's accepts is a bug in ours, and the reverse is a rule we
do not apply yet. Either way somebody has to look.

Needs docker and a guide fetched by scripts/ig/fetch.sh.
"""

import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

IMAGE = "infernocommunity/fhir-validator-service:latest"
CONTAINER = "ilavrita-ig-crosscheck"
PORT = os.environ.get("ILAVRITA_IG_VALIDATOR_PORT", "4568")
GUIDE = os.environ.get("ILAVRITA_PROFILE_DIR", ".ig/ndhm/package")
EXAMPLES = os.environ.get("ILAVRITA_IG_EXAMPLES", ".ig/ndhm/examples")
VALIDATOR = f"http://localhost:{PORT}"


def docker(*arguments):
    binary = shutil.which("docker") or os.path.expanduser("~/.docker/bin/docker")

    return subprocess.run([binary, *arguments], capture_output=True, text=True)


def post(files):
    context = {"sv": "4.0.1", "igs": ["/ig"], "txServer": None}
    body = json.dumps({"cliContext": context, "filesToValidate": files}).encode()
    sent = urllib.request.Request(
        VALIDATOR + "/validate", data=body,
        headers={"Content-Type": "application/json"}, method="POST",
    )

    with urllib.request.urlopen(sent, timeout=1800) as answer:
        return json.loads(answer.read())


def wait_until_loaded():
    probe = [{"fileName": "p.json", "fileContent": '{"resourceType":"Patient"}', "fileType": "json"}]

    for _ in range(90):
        try:
            if "still loading" not in json.dumps(post(probe)):
                return
        except (OSError, urllib.error.URLError):
            # The container accepts connections before it answers, so a reset
            # here means not ready rather than not coming.
            pass

        time.sleep(10)

    sys.exit("the validator never finished loading the guide")


def examples():
    held = []

    for name in sorted(os.listdir(EXAMPLES)):
        if not name.endswith(".json"):
            continue

        with open(os.path.join(EXAMPLES, name), encoding="utf-8") as content:
            body = content.read()

        try:
            if not json.loads(body).get("resourceType"):
                continue
        except json.JSONDecodeError:
            continue

        held.append({"fileName": name, "fileContent": body, "fileType": "json"})

    return held


def refused_by_hl7(outcomes):
    refused = set()

    for outcome in outcomes:
        name = (outcome.get("fileInfo") or {}).get("fileName", "?")
        issues = outcome.get("issues") or (outcome.get("outcome") or {}).get("issue") or []

        if any(issue.get("severity") in ("error", "fatal") for issue in issues):
            refused.add(name)

    return refused


def refused_by_ilavrita():
    run = subprocess.run(
        ["go", "test", "./packages/validate/", "-run", "GuidesOwnExamples", "-v", "-count=1"],
        capture_output=True, text=True,
        env={**os.environ, "ILAVRITA_PROFILE_DIR": GUIDE},
    )

    return {
        line.split(".json:")[0].split()[-1] + ".json"
        for line in run.stdout.splitlines()
        if ".json:" in line and "guide_test.go" in line
    }


def main():
    if not os.path.isdir(GUIDE):
        sys.exit(f"no guide at {GUIDE}; run scripts/ig/fetch.sh")

    docker("rm", "-f", CONTAINER)
    started = docker(
        "run", "-d", "--rm", "--name", CONTAINER, "-p", f"{PORT}:4567",
        "-e", "DISABLE_TX=true", "-v", f"{os.path.abspath(GUIDE)}:/ig:ro", IMAGE,
    )
    if started.returncode != 0:
        sys.exit(f"could not start the validator: {started.stderr.strip()}")

    try:
        wait_until_loaded()
        held = examples()
        print(f"==> {len(held)} examples, two validators")

        answer = post(held)
        theirs = refused_by_hl7(answer.get("outcomes") or [answer])
        ours = refused_by_ilavrita()
    finally:
        docker("rm", "-f", CONTAINER)

    only_ours = sorted(ours - theirs)
    only_theirs = sorted(theirs - ours)

    for name in only_ours:
        print(f"we refuse what HL7 accepts: {name}")
    for name in only_theirs:
        print(f"HL7 refuses what we accept: {name}")

    if only_ours or only_theirs:
        sys.exit(f"{len(only_ours) + len(only_theirs)} example(s) the two validators judge differently")

    print(f"both validators agree on all {len(held)} examples ({len(ours)} refused by each)")


if __name__ == "__main__":
    main()
