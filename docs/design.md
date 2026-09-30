# Sprite Cron: design and research plan

Status: original design record. Research checked September 25, 2026.

An initial implementation now exists. The [documentation site](https://sprite-cron.fly.dev/docs/) describes current
behavior; [implementation notes](implementation.md) record scope and departures
from this plan, including the direct exec adapter. Proposals below are not all
implemented.

## Recommendation

Build a Go service running continuously on one Fly Machine, with SQLite on a Fly
volume. Users configure jobs, schedules, Sprite targets, and named credentials
through an authenticated management API and a small web UI. A durable scheduler
creates run records; a bounded worker pool dispatches them through either the
Sprite exec API or an HTTP endpoint running inside a Sprite.

Start with a self-hosted application for one trusted team. Support several users
in that team, multiple Sprites, multiple organizations, and multiple credentials.
Defer public signup, billing, and isolation between unrelated customers. This
keeps the first version manageable without assuming there is only one Sprite or
one API key.

The central reliability rule is: **a lost connection does not prove a job failed
to run**. Record uncertain outcomes explicitly. Exactly-once external side
effects require cooperation from the job; a scheduler alone cannot guarantee them.

## What the research establishes

These are documented platform capabilities. Choices in later sections are our
proposals. The [live validation report](live-sprite-validation.md) records
observations on Sprite `0.0.2-beta.3`, including differences from the reference.
Remaining research spikes must verify behavior on the versions we deploy.

| Finding | Design implication | Source |
| --- | --- | --- |
| The API base is `https://api.sprites.dev/v1`, authenticated with a bearer token. | Keep authentication in the execution adapter. | [API overview](https://sprites.dev/api) |
| Get Sprite returns an ID, organization, HTTP URL, and URL authentication settings. Names are organization-relative. | Discover the URL; identify targets by organization and Sprite identity. | [Sprite API](https://sprites.dev/api/sprites) |
| Exec supports WebSocket and HTTP POST. WebSocket exposes session IDs, output, and exit status; sessions can be attached to and killed. Non-TTY disconnect survival defaults to 10 seconds and is configurable. | Prefer WebSocket for execution tracking and recovery; set disconnect behavior explicitly. | [Exec API](https://sprites.dev/api/sprites/exec) |
| Sprite HTTP URLs use bearer-token authentication by default; public access is optional. | Default to authenticated targets and allow separate HTTP credentials. | [CLI URL/auth reference](https://docs.sprites.dev/cli/commands/) |
| Incoming HTTP can wake a Sprite; a service configured with an HTTP port starts on request and receives traffic. | The target application must be installed as a Sprite service. | [Services](https://docs.sprites.dev/concepts/services/) |
| An official Go SDK provides an `exec.Cmd`-style interface. | Evaluate the SDK before implementing wire protocols. | [sprites-go](https://github.com/superfly/sprites-go) |
| Fly volumes are local to a host, attach to one Machine, and do not replicate automatically. | SQLite is appropriate for an initial singleton, with backups and accepted downtime. | [Fly volumes](https://docs.fly.io/volumes/overview/) |
| Fly Proxy autostart responds to traffic. | Keep the scheduler running; an internal deadline cannot wake a stopped Machine through incoming traffic. | [Autostop/autostart](https://fly.io/docs/launch/autostop-autostart/) |
| Fly app secrets are injected at runtime. | Store the credential encryption key separately from SQLite. | [Fly secrets](https://docs.fly.io/apps/secrets/) |

Do not assume API tokens are scoped to individual Sprites or that exec and HTTP
must use different tokens. The docs demonstrate organization tokens; verify
current token scope, expiry, and HTTP access policy during integration testing.
Multiple credential records must work regardless of token scope.

## User workflow and first-version scope

1. A machine operator creates a local account using the CLI. Sign in with that
   username and password; an administrator adds a named Sprite credential.
2. Register a Sprite by name and credential. Fetch its organization, ID, and URL;
   show the resolved target before enabling jobs.
3. Create a job: choose exec or HTTP, enter a schedule and time zone, configure
   its payload and timeout, and preview its next five scheduled times.
4. Save disabled, run manually if desired, then enable the schedule.
5. Inspect run history, bounded output, errors, and the next scheduled execution.
   Pause, edit, manually retry, or request cancellation as needed.

Include job CRUD, pause/resume, run-now, schedule preview, credential rotation,
run history, audit events, and basic health/metrics. All job execution happens on
Sprites; the Fly Machine only coordinates work.

Defer DAGs, dependencies between jobs, arbitrary webhook destinations, automatic
Sprite provisioning, deployment of user code, interval/seconds schedules,
interactive terminals, and asynchronous HTTP completion protocols. The user is
responsible for installing the command or HTTP handler on the target Sprite.

## Architecture

```mermaid
flowchart LR
    U[Authenticated user] --> A[Go management API and web UI]
    A --> D[(SQLite on Fly volume)]
    S[Scheduler loop] --> D
    D --> W[Bounded execution workers]
    W --> E[Sprite exec adapter]
    W --> H[Sprite HTTP adapter]
    E --> P[Command on Sprite]
    H --> Q[HTTP service on Sprite]
    W --> D
```

Use one binary with packages for scheduling, storage, execution adapters,
credentials, and management. Prefer standard-library HTTP and templates for a
small UI. Evaluate `github.com/robfig/cron/v3` for parsing and computing the next
occurrence, not as the durable run store. Its documented five-field syntax and
time-zone support fit this proposal. Pin dependencies after the compatibility
spike. [Cron package documentation](https://pkg.go.dev/github.com/robfig/cron/v3)

Proposed starting capacity: 1,000 configured jobs and 10 concurrent attempts,
with configurable per-target limits. These are test targets, not performance
claims. Benchmark before promising capacity. Use one SQLite writer, short
transactions, foreign keys, WAL mode, a busy timeout, and durable commits. Never
hold a database transaction open while calling a Sprite.

## Target and credential model

Separate the Sprite, its authentication, and the job:

- **Credential:** ID, label, kind, encrypted value, key version, optional expiry,
  disabled state, creation/update timestamps, and validation result. Initial
  kinds: Sprite bearer token and application HTTP header secret.
- **Target:** local ID, Sprite name, resolved Sprite ID and organization, discovered
  HTTPS origin, management/exec credential reference, optional HTTP credential
  reference, auth mode, validation timestamp, and configuration revision.
- **Job:** target reference, optional authorized credential overrides, execution
  configuration, schedule, time zone, policy settings, and enabled state.

A credential can serve many targets; one target can reference different exec and
HTTP credentials. An HTTP job may additionally need an application secret, such
as `X-Job-Token`, distinct from Sprite edge authentication. Do not assume two
independent bearer tokens can occupy the same Authorization header. The live
test observed the custom application header arriving while
the Sprite bearer Authorization header was absent; use a separate application
header and retest that forwarding contract on the deployed version.

The management/exec token is needed to validate and discover the target. For
HTTP, default to that token unless the user explicitly selects another. The
supplied test token successfully authenticated both transports in live testing.
Make public HTTP mode explicit, send no Sprite token in that mode, and still require
application authentication for a mutating job endpoint.

Treat the metadata URL as authoritative; never construct it from the Sprite
name. Verify supplied URL overrides against that origin in v1. Refresh metadata
on validation or configuration change, not on every tick. A changed Sprite ID
under the same name requires revalidation before dispatch: replacing a Sprite
must not silently redirect existing jobs.

Use a Fly secret for a versioned encryption key ring and authenticated encryption
(e.g. AES-GCM with fresh nonces and credential IDs as associated data) for stored
credentials. Never return plaintext credentials through list/read APIs. Keep
keys out of the database, logs, images, and repository. Back up the key ring
separately; an encrypted database backup alone cannot restore credentials.

Rotate by validating a replacement credential and atomically switching the
reference. New attempts use the current credential; record its version for
audit. Keep old credentials only as long as necessary for tracked sessions, then
revoke and remove them. Never try every saved key after an authorization failure.

## Job configuration examples

These examples illustrate the proposed management representation, not an
implemented configuration format. Credentials are references, never token values.

```yaml
name: nightly-report
schedule: "0 2 * * *"
timezone: UTC
target_id: reporting-sprite
execution:
  type: exec
  argv: ["/usr/bin/python3", "/home/sprite/jobs/report.py"]
  working_directory: /home/sprite/jobs
policy:
  timeout: 15m
  overlap: forbid
  misfire: skip
  start_grace: 60s
  max_attempts: 1
```

```yaml
name: refresh-index
schedule: "*/15 * * * *"
timezone: America/New_York
target_id: search-sprite
execution:
  type: http
  method: POST
  path: /jobs/refresh-index
  headers:
    Content-Type: application/json
  body: '{"collection":"products"}'
  success_statuses: [200, 204]
policy:
  timeout: 2m
  overlap: forbid
  misfire: coalesce
  start_grace: 60s
  max_attempts: 3
  retry_safe: true
```

The second example assumes the handler implements durable idempotency before
retries are enabled.

## Scheduling semantics

Use five fields: minute, hour, day of month, month, and day of week. Store an
explicit IANA time zone, default UTC; bundle time-zone data in the binary. Reject
seconds fields, inline time-zone prefixes, and schedules with no next occurrence.
Document standard cron's day-of-month/day-of-week OR behavior when both are
restricted. Return actionable validation errors and a next-occurrence preview.

Store all execution instants in UTC. Proposed DST policy: nonexistent local
times are skipped; repeated local times run at both distinct UTC instants. Verify
this behavior against the chosen parser and show both offsets in previews.

Each scheduled occurrence has a unique `(job_id, schedule_revision,
scheduled_at_utc)` key. A run ID identifies the logical invocation, while attempt
IDs identify dispatch attempts. Retries keep the same run ID and scheduled time.
Manual invocations have their own IDs and an API idempotency key to prevent
accidental duplicate submissions.

On a short periodic tick, the scheduler uses a transaction to materialize due
runs and advance the persisted `next_run_at`. A uniqueness constraint makes
repeated ticks harmless. A crash commits both changes or neither. Wake promptly
on configuration updates; periodically recheck wall time to handle clock changes.
Use monotonic clocks for in-process elapsed time and persist absolute deadlines
for restart recovery.

Define missed schedules and concurrency explicitly:

| Setting | Proposed behavior |
| --- | --- |
| `start_grace` | A due run may start up to 60 seconds late by default. Recheck before dispatch. |
| `misfire: skip` (default) | Record older occurrences as skipped; resume with eligible/future occurrences. |
| `misfire: coalesce` | When several occurrences are missed, create at most one catch-up run for the latest missed occurrence, then resume normally. |
| Long downtime | Process bounded batches and summarize skipped ranges/counts; never allocate an unbounded backlog. |
| `overlap: forbid` (default) | If a prior run is active, awaiting retry, or unresolved, record the new occurrence as skipped for overlap. |
| `overlap: allow` | Allow concurrent runs up to configured job, target, and global limits. |
| Capacity exhausted | Keep due work queued only within its grace/catch-up deadline; expire it with an explicit reason afterward. |
| Pause | Stop new scheduled work and cancel queued scheduled runs; active work continues unless separately cancelled. |
| Edit | Create a new revision, cancel old queued scheduled runs, and calculate the next time from the edit. Active runs keep their snapshots. |
| Resume | Schedule from the resume time; do not backfill the paused interval. |

For coalescing, set a fresh bounded dispatch deadline when materializing the
catch-up run. Do not repeatedly coalesce the same window: advance the schedule
cursor in the same transaction. Run-now obeys concurrency limits and is available
while a schedule is paused. Pausing does not erase run history.

## Durable execution and failure handling

Persist an immutable execution snapshot when creating a run: command or request,
target identity, nonsecret settings, credential references, policy, and schedule
revision. Workers claim pending attempts transactionally and mark them
`dispatching` **before** network I/O. Persist the session ID as soon as available.
Only a confirmed command exit or an agreed HTTP completion response is success.

Proposed run states: `queued`, `dispatching`, `running`, `retry_wait`, `succeeded`,
`failed`, `skipped`, `cancelled`, `timed_out`, and `unknown`. Keep the execution
outcome separate from cancellation requests and observed remote liveness. A
local timeout is not confirmation that the remote process stopped.

| Failure point | Recovery |
| --- | --- |
| Before a run transaction commits | Recompute from the persisted schedule cursor. |
| Queued, before the worker claims it | Another tick/worker can claim the existing record. |
| Claimed, before or during dispatch | If dispatch cannot be ruled out, treat as uncertain, even if the process might have crashed before sending. |
| Exec session ID recorded, stream lost | Try to reconnect to that exact session; never start a new command merely to reconnect. |
| Remote started, session ID or final result not saved | Mark `unknown`; absence from the session list is not proof of failure. |
| HTTP request sent, response lost | Mark `unknown`; retry only under an explicit idempotency contract. |
| Final result received, database write fails | Stop new dispatch if storage is unhealthy; retain/report uncertainty on recovery. |

Default to one attempt. Offer capped exponential backoff with jitter, persisted
retry timestamps, a total retry deadline, and a maximum attempt count. Retry
known pre-dispatch transient failures. Retry post-dispatch failures, 429/5xx,
nonzero exits, or uncertain outcomes only for jobs explicitly declared safe to
repeat; honor a bounded `Retry-After`. Authentication/configuration failures need
intervention. Audit manual retries and warn when the prior outcome is unknown.

Send a stable logical run ID as `Idempotency-Key` and `X-Sprite-Cron-Run-ID` for
HTTP; provide equivalent run metadata to exec jobs. A repeated header is not
itself deduplication: the Sprite application must durably store the key and
coordinate it with its side effects. For operations against another service,
propagate an idempotency key supported by that service where possible.

Keep unresolved runs blocking `overlap: forbid` until reconciliation confirms
completion/termination, or an operator explicitly accepts the duplicate/overlap
risk. Lease expiration permits investigation, not automatic redispatch. A
singleton process lock guards against a second scheduler on the same database;
independent copied databases are never supported as active schedulers.

## Exec adapter

The [SDK research](sprites-go-sdk-research.md) recommends a pinned SDK behind
our own adapter, with lifecycle fixes as an acceptance gate. Local probes found
that context cancellation does not send termination, disconnect lifetime is not
exposed, and shutdown has a data race. Use direct HTTP for kill-progress handling;
retain a focused direct WebSocket adapter as the fallback if SDK fixes grow large.
Use non-TTY execution, explicit argv, bounded stdout/stderr capture, and an observed exit code. Shell syntax requires an
explicit shell invocation; never interpolate user values into a shell command.

Set an explicit disconnect survival window (proposed 60 seconds) and attempt
reattachment promptly. Live tests confirmed the parameter affects survival, but
its enforcement was not a precise deadline; use independent job timeouts.
Configuring the disconnect window requires an SDK addition or direct exec
handling; the inspected SDK has no corresponding command option. Persist the
overall deadline. A timeout/cancellation requests remote termination using the recorded session ID, follows progress,
and escalates when supported. If termination cannot be established, retain
unknown remote liveness and the overlap block. Do not equate a closed socket,
context cancellation, or HTTP 200 from a kill request with confirmed termination.

Live tests verified working directory and observed PATH/HOME remaining present
when an extra environment variable was supplied, despite replacement wording in
the reference. Pin and retest that behavior. Reconnection replay ignored the
requested output offset and combined earlier stderr into stdout on the tested
server; mark replay as potentially duplicated and stream labels as uncertain.
A completed-session attach returned 410 after one second. Persist exits promptly;
without durable completion evidence, preserve an unknown outcome. Stronger result
recovery would require a cooperative runner storing run IDs and outcomes on the
Sprite, which is additional scope.

Always provide explicit bounded output writers or `io.Discard`. Live testing
confirmed that an output writer error can be ignored by `Run`; record capture
failure separately from remote exit. The HTTP POST exec alternative is distinct
from calling the user's HTTP server. Live testing returned a binary stream and
HTTP 200 even for exit 7; its full framing contract needs verification before
using it as a fallback.

## HTTP adapter and handler contract

Use Go's HTTP client with TLS verification, connection/response deadlines, a
bounded body, and redirects disabled. Combine only a validated relative path
with the target's approved HTTPS origin. Reject userinfo, host overrides,
absolute/network-path URLs, and reserved authentication/forwarding headers.

V1 handlers finish work before returning one of the configured success statuses.
Default success is 2xx except 202. Treat 202 as `unknown` with an unsupported
asynchronous-response reason, since it may mean work was accepted. Later we can
add an explicit operation ID and authenticated status/cancellation protocol;
acceptance alone must never be labeled completed work.

A request timeout or disconnect cannot cancel application work reliably. The
handler must enforce its own deadline and implement idempotency when enabled.
Use a dedicated job endpoint, not a generic command-execution or file-serving
endpoint. Require method/body semantics to be deliberately defined by its owner.

## Storage and management

Proposed tables:

| Table | Purpose |
| --- | --- |
| `users`, `sessions` | Local accounts, password hashes, roles, disabled state, and hashed expiring browser sessions. |
| `api_tokens` | Token digest, owner, label, scopes, target restrictions, expiry, revocation, and usage timestamps. |
| `credentials`, `credential_versions` | Encrypted values, labels, and rotation history. |
| `targets` | Verified Sprite identity, origin, and allowed credential bindings. |
| `jobs`, `job_revisions` | Current schedule cursor and immutable configuration revisions. |
| `runs` | Logical occurrences, execution snapshots, status, and overlap ownership. |
| `attempts` | Dispatch state, session ID, deadlines, response/exit details, and bounded output. |
| `audit_events` | Actor, action, object, timestamp, and redacted changes. |

Index enabled jobs by next occurrence, attempts by status/retry time, and run
history by job/time. Uniqueness constraints enforce occurrence and manual-request
idempotency. Keep migrations versioned and take a consistent backup before
schema changes. Redacted configuration exports should be portable but are not a
replacement for run-state backups.

Suggested API resources: `/api/targets`, `/api/credentials`, `/api/jobs`,
`/api/jobs/{id}/runs`, `/api/runs/{id}`, `/api/runs/{id}/cancel`, and
`/api/schedules/preview`. Use optimistic concurrency for edits and pagination for
history. Run-now is a POST; all state-changing operations require authorization.
The UI should show schedule/time zone, next occurrence, last outcome, lateness,
and unresolved runs without exposing secret values.

## Local authentication and management API tokens

Authentication is self-contained: no OIDC provider, external identity service,
email delivery, or public registration is required. Account administration is a
local CLI operation requiring shell access and OS permission to the application's
database. Application administrator privileges alone do not permit creating
users or changing account roles through HTTP.

### Accounts and browser login

Proposed commands, run on the volume-owning Machine:

```sh
sprite-cron users add kate --role admin
sprite-cron users add automation --role operator --service-account
sprite-cron users list
sprite-cron users set-role kate --role admin
sprite-cron users reset-password kate
sprite-cron users disable automation
```

Interactive password commands use a hidden terminal prompt, never a password
argument. Human accounts have a username and password; email is unnecessary.
Service accounts have no password and cannot log in through the browser. Both
account types can own API tokens. No default account or default password exists;
with no users, management stays locked until the first account is created locally.

The CLI uses the configured database path and the same migrations, validation,
and transaction layer as the daemon. It can operate while the daemon is running,
with SQLite serializing short writes. It must fail on a missing/wrong database
rather than silently initialize a second one. Restrict database file permissions
and record local administration events, including that the action came from the
CLI. Shell/database access is the administrative trust boundary; OS-level audit
is needed to attribute multiple people sharing the same OS account.

Hash passwords with a maintained Argon2id implementation, a unique random salt,
and stored algorithm parameters. Start at OWASP's minimum of 19 MiB memory, two
iterations, and parallelism one; benchmark and increase the work factor within
the Machine's resource budget. Limit concurrent password verification and rate
limit login by account and source, with generic failure responses. Never store
reversible passwords. [OWASP password storage guidance](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)

After login, issue an opaque random session identifier and store only its digest
server-side. Use a Secure, HttpOnly, SameSite cookie and CSRF protection on
cookie-authenticated mutations. Proposed limits: 30 minutes idle and 12 hours
absolute lifetime. Rotate the session on login; invalidate it on logout. Password
changes/resets revoke all browser sessions and API tokens for that account;
account disabling also revokes both. Check current account status and permissions
on every request so local changes apply without restarting the daemon.
[OWASP session guidance](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)

Provide a logged-in password-change page requiring the current password. Account
recovery and administrative resets use the CLI; there is no email reset flow.

### API tokens for automation

These are **Sprite Cron API tokens**, issued by this service. They authenticate
clients adding/managing jobs; they are distinct from the upstream Sprite API
credentials used by workers. Never accept an upstream Sprite token as management
authentication or send a Sprite Cron token to a Sprite.

Example token issuance and revocation (proposed CLI):

```sh
sprite-cron tokens create --user automation --name deploy-pipeline --scopes jobs:read,jobs:write --targets reporting-sprite --expires-in 90d
sprite-cron tokens list --user automation
sprite-cron tokens revoke TOKEN_ID
```

Generate an opaque token such as `scron_<public-id>_<random-secret>` with 32 bytes
of cryptographic randomness in the secret. Show the full value once at creation;
store only its SHA-256 digest plus metadata and compare digests in constant time.
A fast digest is appropriate for this high-entropy random secret, unlike a human
password. The public ID locates the record and can appear in audit events; the
secret must never appear in logs, URLs, later API responses, or exports.

Clients send the token over HTTPS as `Authorization: Bearer <token>` when calling
endpoints such as `POST /api/jobs`. Validate its digest, expiration, revocation,
owner status, scopes, and resource restrictions on every request. Tokens do not
need external validation or a JWT signing service. Reject requests presenting
both a bearer token and a browser session rather than mixing their permissions.
Cookie authentication requires CSRF protection; bearer-only requests do not use
ambient browser credentials. Do not enable permissive cross-origin access.

Initial scopes:

| Scope | Allows |
| --- | --- |
| `jobs:read` | List/read job configuration and preview schedules. |
| `jobs:write` | Create, edit, pause/resume, and delete schedules on allowed targets. |
| `runs:read` | Read run history and captured output on allowed targets. |
| `runs:trigger` | Run-now and explicit retry on allowed targets. |
| `runs:cancel` | Request cancellation on allowed targets. |
| `targets:read` | Read nonsecret metadata for allowed targets. |

Effective authority is the intersection of the owner's current role, the token's
scopes, and its explicit target allowlist. An administrator may deliberately
issue an all-target token; adding targets does not expand a specific allowlist.
Validate both old and new targets when moving a job. Apply these checks to list,
detail, history, and mutation endpoints, not just job creation. Bind execution to
the target's approved upstream credentials; a token cannot choose arbitrary
credential overrides. In v1, tokens cannot administer users, credentials, target
bindings, or other tokens, even if their owner is an administrator.

`jobs:write` is execution authority: creating or enabling a schedule can run
arbitrary commands on an allowed target without `runs:trigger`. The latter only
controls immediate/manual execution. Document this distinction when issuing a
token. Readers cannot receive effective write or execution permissions.

The CLI can create tokens for an existing account. Human users can also create,
list, and revoke their own tokens in the UI after recent password verification;
issuance cannot exceed their role. Administrative UI revocation is allowed, but
issuing tokens for other accounts remains a local CLI action. Token-management
HTTP routes require a browser session and CSRF protection; an API token cannot
mint replacements or escalate itself.

Default to a 90-day expiry, require an explicit expiry at issuance, and show
expiry/last-use metadata. Rotate by creating a replacement, updating the caller,
then revoking the old token. Revocation takes effect on the next request; it does
not undo accepted jobs or terminate already-running work. Jobs remain configured
when the creating token expires or is revoked; disabling an account likewise
blocks access but does not pause its schedules. Offer deliberate job pause and
cancellation actions for incident response. Audit the actor and token ID on
accepted changes and manual runs.

Backups contain password/token/session digests. A restore may resurrect previously
revoked credentials: clear browser sessions and revoke all management API tokens
as part of the restore procedure, then issue fresh tokens locally. Reconcile
account disablement and role changes before reopening management access.

## Security boundary

Job authors can execute code with the authority of the credentials assigned to
their targets. In v1, everyone with an operator role is trusted within the team;
readers cannot edit or run jobs, and administrators control credentials and target
bindings. Independent customer isolation would require a separate design.

Use the local accounts, browser sessions, and scoped API tokens described above,
with shared authorization checks for the UI and management API.
Require TLS on management access. Keep diagnostic/metrics endpoints private and
public health responses minimal. Do not expose environment dumps or arbitrary
filesystem reads.

Prevent SSRF and credential leakage: pin exec to the official API origin; pin
HTTP to the verified Sprite origin; block private, loopback, link-local, and
metadata destinations at dial time, including DNS rebinding. Never forward
credentials on redirects. Do not permit arbitrary custom origins in v1.

Treat response bodies, command output, argv, and errors as potentially sensitive.
Redact known credentials, avoid logging full request URLs or headers, escape UI
output, and disable raw debug dumps. Redaction cannot detect every secret a job
prints; restrict access to logs and allow capture to be disabled. Secret command
inputs should use a dedicated secret mechanism, not literal argv or logged query
parameters. Sprite API credentials must never be injected into the executed job.

## Fly.io deployment and operations

Start with one Machine in one region and a volume mounted at `/data`, storing
`/data/sprite-cron.db`. Disable automatic stopping (`auto_stop_machines = "off"`
for a configured HTTP service), explicitly deploy one Machine, and configure
restart behavior. Do not allow deployment tooling to create a second independent
scheduler. Use a shutdown procedure that stops claims, drains bounded work, and
leaves unfinished attempts available for reconciliation.
[Fly configuration reference](https://fly.io/docs/reference/configuration/)

A singleton accepts maintenance and host-failure downtime. Fly recommends
redundancy for availability, and daily volume snapshots are not a primary backup
strategy. Take application-consistent encrypted backups to independent object
storage, monitor backup age, and test restoring the database plus encryption
keys. Run SQLite migrations on the volume-owning Machine before scheduling;
Fly release-command Machines do not mount the volume.
[Fly storage constraints](https://docs.fly.io/volumes/overview/)

Proposed initial operating settings, adjustable after load tests:

- 1 shared CPU, 512 MiB RAM, and a 3 GiB volume.
- 10 concurrent attempts globally, 2 per target, and a bounded pending queue.
- 1 MiB retained output per attempt, 64 KiB HTTP request bodies, and 30-day history.
- A global output storage budget (initially 512 MiB), pruning closed runs before
  that budget is exhausted; never let per-run limits alone fill the volume.
- Hourly consistent backups, with an initial recovery-point target of one hour
  and recovery-time target of one hour, both requiring a measured restore drill.

Expose metrics for dispatch lag, queue depth, active/unknown runs, failures,
credential errors, reconnects, disk usage, and backup age. Health must include a
scheduler heartbeat and database health, not just an HTTP listener. Alert on
missed heartbeats, uncertain outcomes, repeated failures, and low storage.

Restore with dispatch disabled. Reconcile restored in-flight records against
remote activity; a stale backup may predate completed work and cannot prove it
safe to replay. Advance scheduling from an explicitly selected recovery cutoff
and require deliberate catch-up decisions. Never run the old and restored
scheduler concurrently.

For higher availability, move durable state to shared PostgreSQL and add atomic
claims, leader election/fencing, and worker leases before adding Machines. A
second SQLite volume alone gives neither shared state nor duplicate prevention.
Keep exactly-once limitations even after this migration. If strict availability
is needed at launch, select PostgreSQL from the start.

## Implementation sequence and acceptance criteria

### 1. Compatibility and failure research

Use a disposable test Sprite in the later implementation phase. Pin the SDK and
record observed behavior and versions:

- Validate token scope, multiple tokens, rotation/revocation, and the same token
  against exec and authenticated HTTP, including URL access settings.
- Verify name/organization/ID discovery and replacement detection.
- Exercise non-TTY exec, environment, directory, output, nonzero exits, session
  IDs, reconnect, output replay, disconnect lifetime, and process-group killing.
- Interrupt exec before/after session-ID receipt and before result persistence;
  establish what completed-session information survives and for how long.
- Exercise cold HTTP startup, app authentication headers, response loss, timeout,
  redirect rejection, 202, and a handler implementing durable idempotency.

The [source review and local probes](sprites-go-sdk-research.md) and
[live experiments](live-sprite-validation.md) provide the initial compatibility
record and adapter recommendation. Complete the remaining live checks and
lifecycle fixes before finalizing the implementation choice. Treat observations
as version-specific; do not build reliability promises on unverified behavior
or undocumented API limits.

### 2. Durable scheduling core

Implement migrations, job revisions, parser/preview, run materialization,
claiming, and policies with a fake clock and fake executor. Acceptance: repeated
ticks and crash injection do not duplicate occurrence records; UTC/DST, clock
changes, leap days, missed schedules, overlap, pause/edit/resume, bounded catch-up,
and concurrent run-now requests have explicit tested outcomes.

### 3. Execution adapters and recovery

Implement exec and HTTP, bounded output, credentials, retries, cancellation, and
restart reconciliation. Acceptance: integration tests cover both transports,
nonzero exits, slow starts, network interruption, token failure, and disk-write
failure. Kill/restart the scheduler around every dispatch/result boundary and
verify uncertain work is not blindly repeated.

### 4. Management API and UI

Implement local account/password CLI commands, login/roles, browser sessions,
scoped API tokens, job and target forms, schedule previews, credential rotation,
audit, and run history. Acceptance: bootstrap and recovery work without external
authentication services; HTTP cannot create accounts; passwords and management
tokens are stored only as hashes/digests. Test expiry, revocation, disabled users,
role downgrades, password resets, target restrictions, and scope enforcement,
including job moves and list/history reads. API tokens cannot mint tokens or
alter upstream credential bindings. CSRF and SSRF tests pass, secret values never
appear in API reads/logs, and output is safely rendered. Both a browser user and a
scoped automation token can create jobs end to end.

### 5. Fly deployment and operational validation

Add the container, Fly configuration, shutdown/restart handling, backups,
metrics, and an operations guide. Acceptance: schedules survive deployment;
downtime follows misfire policy; a restore drill meets the chosen targets;
capacity/output tests stay within the configured resource budget. Confirm there
is exactly one active scheduler before enabling production jobs.

## Decisions to revisit together

The proposal supplies defaults so planning can proceed; these are the most
useful decisions to settle before implementation:

1. Is this for one trusted team, or must unrelated customers have isolated
   accounts? Default: one team with administrator/operator/reader roles.
2. Is occasional deployment/host downtime acceptable? Default: singleton SQLite;
   use PostgreSQL and redundant Machines if availability is a launch requirement.
3. Should missed work be skipped or caught up? Default: skip, with per-job
   coalescing; no unbounded replay.
4. Are HTTP jobs synchronous? Default: yes. Async work needs a defined status and
   cancellation contract before it can be tracked accurately.
5. Which initial workloads should drive the integration tests? Obtain workload
   duration, output size, and duplicate-execution tolerance before finalizing
   deployment sizing and retry defaults. Authentication uses local accounts
   provisioned through the CLI, with separate scoped tokens for automation.
