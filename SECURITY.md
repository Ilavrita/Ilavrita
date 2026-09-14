# Security policy

Ilavrita is designed to hold patient data. A vulnerability here can expose
health information, so we treat reports seriously and we would rather hear about
a false alarm than miss a real one.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private reporting:
[Report a vulnerability](https://github.com/Ilavrita/Ilavrita/security/advisories/new).

If you cannot use GitHub, write to <security@ilavrita.health>.

Please include what you can:

- the affected version or commit;
- what an attacker gains;
- steps to reproduce, ideally a minimal case;
- whether the finding is already public.

Use synthetic data in reports. Never send real patient data, and redact any you
encountered while testing.

## What to expect

| Stage | Target |
| --- | --- |
| Acknowledgement | 3 working days |
| Initial assessment | 10 working days |
| Fix or documented mitigation for a confirmed critical issue | 30 days |

We will keep you updated, credit you in the advisory unless you prefer
otherwise, and tell you when the fix ships.

## Scope

In scope: this repository, the [Ilavrita PocketBase fork](https://github.com/Ilavrita/pocketbase),
and official container images under the `ilavrita` namespace.

Particularly interested in: cross-tenant access of any kind, authorization
bypass, injection, unsafe file access, secret exposure, and patient data
reaching logs, error responses or backups.

Out of scope: findings against a deployment you do not operate, denial of
service through unbounded request volume, missing hardening headers with no
demonstrated impact, and vulnerabilities in dependencies that are already public
and carry no Ilavrita-specific exploit path — report those upstream.

## Supported versions

Ilavrita has not reached a stable release. Until it does, only the default branch
receives fixes.

## What Ilavrita does not claim

Running Ilavrita does not make an organisation compliant with HIPAA, GDPR, EHDS
or any other regime, and Ilavrita holds no certification under any of them.
Ilavrita provides technical controls. Compliance depends on your deployment,
your policies and your organisation.
