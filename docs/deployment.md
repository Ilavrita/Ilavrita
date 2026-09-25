# Deployment

> Ilavrita does not enforce authorization, tenancy or audit yet. Deploy it with
> synthetic data only. See [known limitations](known-limitations.md).

## From source

Requires Go 1.27+.

```bash
git clone https://github.com/Ilavrita/Ilavrita.git
cd Ilavrita
make bootstrap
make build
./bin/ilavrita serve --http=127.0.0.1:8090 --dir=./ilavrita_data
```

`make bootstrap` fetches the pinned PocketBase fork. A clone without submodules
will not compile, because the `replace` directive in `go.mod` has nothing to
resolve to.

## Container

```bash
docker run --rm -p 8090:8090 -v ilavrita-data:/data ghcr.io/ilavrita/ilavrita:latest
```

Or with Compose:

```bash
docker compose up --build
```

Images are published per release for `linux/amd64` and `linux/arm64`, with build
provenance attached.

## Configuration

Settings come from the environment. Copy [`.env.example`](../.env.example) and
edit it; never commit a filled-in copy.

| Variable | Purpose |
| --- | --- |
| `ILAVRITA_HTTP_ADDR` | Network binding |
| `ILAVRITA_BASE_URL` | Externally reachable base URL, used to build Bundle links |
| `ILAVRITA_DATA_DIR` | SQLite database and locally stored files |
| `ILAVRITA_LOG_LEVEL` | `debug`, `info`, `warn` or `error` |
| `ILAVRITA_FILE_STORAGE` | `local` or `s3` |
| `ILAVRITA_PROFILE_DIR` | An implementation guide to hold resources to |
| `ILAVRITA_TERMINOLOGY_DIR` | CodeSystem and ValueSet JSON to check codes against |

Configuration is not wired up yet; the server currently takes PocketBase's own
`serve` flags.

The last two change what the server will accept, not only what it reports.
Validation runs on every create and update, so pointing `ILAVRITA_PROFILE_DIR`
at a guide makes the server refuse a resource that names one of that guide's
profiles in `meta.profile` and then breaks it. Resources already stored are not
re-checked, so a guide added to a running install takes effect on the next write
and an existing row can be one its own server would now refuse. Set it before
loading data, or expect to find that out through a failing update.

A resource naming a profile this install does not hold is a warning, not a
refusal. See [profiles.md](profiles.md) for what is and is not checked.

## Checking a deployment

```bash
curl http://127.0.0.1:8090/healthz
curl http://127.0.0.1:8090/version
curl -H 'Accept: application/fhir+json' http://127.0.0.1:8090/fhir/R4/metadata
```

`/version` reports the version and the commit it was built from, which is how a
running instance is matched to a source tag.

## Production notes

TLS terminates at Ilavrita or at a reverse proxy you trust. Do not expose it
without TLS.

The database is a single SQLite file under the data directory, so a backup is a
consistent copy of that directory plus any external object storage. Backup,
restore and migration tooling does not exist yet, which is one reason this is not
a production release.

Ilavrita scales vertically on one node. Horizontal scale is a later phase.
