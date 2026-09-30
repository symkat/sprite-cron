# Initial implementation — September 25, 2026

The [operations guide](https://sprite-cron.fly.dev/docs/operations/) covers running the service. The earlier [design](design.md)
is retained as a planning record; its proposed capabilities should not be read
as a list of completed features.

## Shipped scope

Go 1.26, SQLite in WAL mode with full synchronous writes, a single scheduler
process, an embedded browser application, and a JSON HTTP API. Accounts are
provisioned through the local CLI. Browser passwords, server-side sessions, and
opaque scoped API tokens operate without an external identity provider.

Jobs support five-field cron, IANA time zones, manual execution, exec, shell, and HTTP
modes, immutable run snapshots, deadlines, overlap prevention, summarized
misfires, and explicit retry safety. Credentials use a versioned AES-GCM keyring
separate from the database. The same application enforces permissions for both
browser and API operations.

The repository includes a container definition, Fly configuration for a single
Machine and volume, CI checks, online backup and offline restore commands, and
maintenance mode. Restore validates the copied database before replacement and
invalidates restored local sessions/API tokens.

Shell jobs execute a single script argument through `/bin/bash -lc` by default,
with `/bin/sh -lc` also supported. They load login profiles and reuse the exec
adapter for output, cancellation, and recovery. Scripts are expanded on the Sprite.

Target application authentication is optional, including for public Sprite URLs.
Application header names are checked for HTTP syntax only; there is no name
blocklist. The application credential overrides a matching default request header,
including Authorization and run identity headers. Overriding Authorization on an
authenticated Sprite URL may prevent Sprite edge authentication. The HTTP transport
and Sprite proxy still apply their own protocol handling. Job-level JSON headers
retain their reserved-header restrictions; use a target credential for secrets.

Credential edits in the browser keep the selected ID and kind fixed. Job edits
use the existing job ID and revision, so changing the display name preserves the
job and its history.

## Decisions made while building

- **Direct exec adapter.** The SDK was investigated before implementation. Its
  inspected lifecycle behavior required patches for the disconnect lifetime,
  cancellation, and shutdown handling. A narrow Gorilla WebSocket implementation
  provides the specific protocol needed here without a maintained SDK fork.
  Metadata and termination use Go's HTTP client; job HTTP requests use a client
  that rejects redirects and non-public network destinations.
- **Conservative recovery.** Save a session ID before proceeding. Reconnect only
  to that session. Missing exit evidence remains unknown; completed sessions
  cannot be assumed to support durable replay. Remote timeout/cancellation
  confirmation uses the kill endpoint's progress stream.
- **Single process ownership.** A filesystem lock protects one database; there
  is no distributed lease service. Independent copies must never be used as
  simultaneous active schedulers.
- **Bounded misfire processing.** Missed windows are summarized into their latest
  occurrence, with skip or coalesce behavior. The service does not materialize
  an unbounded minute-by-minute backlog after a long outage.
- **Shared trusted team.** Browser role permissions are team-wide. Target
  allowlists restrict API tokens. This does not isolate mutually untrusted users
  who have exec authority over the same Sprite.

## Validation performed

The offline suite exercises DST changes, configuration rejection, transactional
queue updates, occurrence and manual-trigger uniqueness, immutable snapshots,
optimistic job updates, overlap with unknown outcomes, restart recovery,
retry limits/identity, scoped access, role changes, CSRF and mixed-auth rejection,
credential encryption/rotation, backups, offline restore and access revocation,
HTTP status/redirect/output behavior, WebSocket replay attachment, and termination
requests. `make check` runs formatting checks, vet, and the race detector.

The opt-in live test uses a temporary local SQLite database, creates jobs through
the actual scoped HTTP API, dispatches with the scheduler and execution adapter,
and reads results back through the API. Against `test-sprite-cron`, it verified:

| Test | Observed result |
| --- | --- |
| `/bin/printf sprite-cron-live-ok` | Succeeded, exact output, saved remote session ID |
| Authenticated `GET /health` on the managed Sprite HTTP service | Succeeded, HTTP 200, expected health JSON |
| `/bin/sleep 20` with a 3-second deadline | Timed out, remote termination confirmed, saved session ID |

No supplied Sprite token is stored in this repository. The live test reads a
protected token file only when explicitly enabled. The pre-existing test HTTP
service was left running.

A Fly deployment has not been performed. The Docker build is included in CI;
the development environment did not have a Docker daemon for a local image
build. Neither that CI run nor a Fly rollout is claimed as completed validation.

## Deferred capabilities and limits

No high availability, multiple scheduler replicas, multi-region failover,
distributed leader election, per-browser-user target grants, external identity
provider, MFA, asynchronous HTTP completion protocol, application-managed
exactly-once effects, or metrics exporter. No persistent output streaming to the
browser: results are captured when an attempt finishes, and attempt metadata is
available with run details. Past attempt output is retained internally for
bounded retention but has no separate download endpoint.

Credential and target creation/update are implemented, including administrator-owned
`credentials:write` and `targets:write` API tokens. Credential writes are global,
write-only, encrypted, and audited; target allowlists do not scope shared credentials.
Credential listing remains browser-administrator-only; deleting/garbage-collecting
those resources is deferred. Targets can be created and updated through the administrator
API using browser sessions or scoped `targets:write` bearer tokens; the initial browser form registers new targets. Misfire and retention limits
are fixed rather than dynamically configurable. Audit listing is limited to the
most recent 200 events. Runs use offset pagination. The Jobs panel displays 25
rows by default, optionally 50, in newest-created order with Previous/Next controls.
Job pagination happens in the browser over the existing API array (at most 1,000
jobs), preserving the REST response format and job names in run history.
Forms and fields request autocomplete off, with password-manager ignore hints;
browsers and extensions may override those preferences.

The source research probe is stored as `docs/research/sprites_sdk_test.go.txt` so
it remains reproducible evidence without becoming part of the service's Go test
module or importing the investigated SDK into production dependencies.
