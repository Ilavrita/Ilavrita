#!/usr/bin/env bash
# Validate what Ilavrita puts on the wire against the HL7 FHIR validator.
#
# The Go suite asserts this server's behaviour against its own reading of R4.
# This asks a second implementation — the validator Inferno itself runs — whether
# the bytes are valid FHIR. It found two defects the Go suite could not: a
# lastModified written as an HTTP-date, and an element written as {}.
#
# Terminology is disabled on purpose. This server's own terminology is local and
# deterministic, and a gate that reaches tx.fhir.org answers differently on a
# day that server is slow. The one check that needs it — a BCP-13 mime type —
# cannot be resolved by the public terminology server either.
set -euo pipefail

cd "$(dirname "$0")/.."

readonly IMAGE="infernocommunity/fhir-validator-service:latest"
readonly CONTAINER="ilavrita-conformance-validator"
readonly VALIDATOR_PORT="${ILAVRITA_VALIDATOR_PORT:-4567}"
readonly SERVER_PORT="${ILAVRITA_CONFORMANCE_PORT:-8123}"

DOCKER=$(command -v docker || echo "$HOME/.docker/bin/docker")
if [ ! -x "$DOCKER" ]; then
  echo "docker is needed to run the FHIR validator; install it or set PATH" >&2
  exit 1
fi

WORKDIR=$(mktemp -d)
STARTED_VALIDATOR=""

cleanup() {
  if [ -n "${SERVER_PID:-}" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi

  # Only stop what this run started, so a validator someone left running for
  # their own loop is not taken out from under them.
  if [ -n "$STARTED_VALIDATOR" ]; then
    "$DOCKER" stop "$CONTAINER" >/dev/null 2>&1 || true
  fi

  $RM -rf "$WORKDIR" 2>/dev/null || true
}
RM=$(command -v rm)
trap cleanup EXIT

if curl -sf "http://localhost:$VALIDATOR_PORT/version" >/dev/null 2>&1; then
  echo "==> Using the validator already listening on $VALIDATOR_PORT"
else
  echo "==> Starting the FHIR validator"
  "$DOCKER" run -d --rm --name "$CONTAINER" \
    -p "$VALIDATOR_PORT:4567" -e DISABLE_TX=true "$IMAGE" >/dev/null
  STARTED_VALIDATOR=yes

  # It loads the R4 definitions before it answers anything, and says so in the
  # outcome rather than by failing, so waiting on /version is not enough.
  echo "==> Waiting for it to load the R4 definitions"
  for _ in $(seq 1 120); do
    if curl -s -X POST "http://localhost:$VALIDATOR_PORT/validate?profile=http://hl7.org/fhir/StructureDefinition/Patient" \
      -H 'Content-Type: application/json' -d '{"resourceType":"Patient","id":"x"}' 2>/dev/null |
      grep -qv "still loading"; then
      break
    fi
    sleep 5
  done
fi

echo "==> Capturing the shapes the handlers produce"
mkdir -p "$WORKDIR/wire"
ILAVRITA_WIRE_DIR="$WORKDIR/wire" go test ./apps/ilavrita/ \
  -run 'TestWireShapesAreCaptured|TestEveryServedTypeIsCaptured|TestTheBespokeShapesAreCaptured' \
  -count=1 >/dev/null

echo "==> Capturing the shapes only a running server produces"
go build -o "$WORKDIR/ilavrita" ./apps/ilavrita
"$WORKDIR/ilavrita" serve --http="127.0.0.1:$SERVER_PORT" --dir="$WORKDIR/data" >"$WORKDIR/log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$SERVER_PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done

base="http://127.0.0.1:$SERVER_PORT"
curl -s "$base/fhir/R4/metadata" -o "$WORKDIR/wire/capabilitystatement.CapabilityStatement.json"

# Every refusal shape, because an OperationOutcome is the only error body this
# server returns and a client parses it the same way it parses a resource.
curl -s "$base/fhir/R4/Organization/x" -o "$WORKDIR/wire/refusal-401.OperationOutcome.json"
curl -s "$base/fhir/R4/Organization/x/_history/1/nothing" -o "$WORKDIR/wire/refusal-501.OperationOutcome.json"
curl -s -X POST "$base/fhir/R4" -H 'Content-Type: application/fhir+json' \
  -d '{"resourceType":"Patient"}' -o "$WORKDIR/wire/refusal-400.OperationOutcome.json"
curl -s -X POST "$base/fhir/R4" -H 'Content-Type: text/plain' -d 'x' \
  -o "$WORKDIR/wire/refusal-415.OperationOutcome.json"
curl -s -H 'Accept: application/fhir+xml' "$base/fhir/R4/metadata" \
  -o "$WORKDIR/wire/refusal-406.OperationOutcome.json"

echo "==> Validating"
ILAVRITA_VALIDATOR_URL="http://localhost:$VALIDATOR_PORT" \
  python3 scripts/conformance.py "$WORKDIR/wire"
