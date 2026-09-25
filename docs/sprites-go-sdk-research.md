# Sprite Go SDK versus direct API access

Research date: September 25, 2026. This is source review and local protocol testing,
not a live-platform certification. See the [system design](design.md).

Follow-up: [live validation](live-sprite-validation.md) tested the authorized
Sprite on `0.0.2-beta.3`. It confirms cancellation and output-capture gaps,
shows that the live session-list shape matches the SDK, and identifies replay
and completed-session recovery limits. The local findings below remain the
record of the original source/protocol review.

Implementation follow-up: the initial service selected the focused direct
WebSocket fallback so it can explicitly control disconnect lifetime and remote
termination without maintaining an SDK fork. See [implementation notes](implementation.md).
The recommendation below preserves the original research decision and conditions.

## Recommendation

**Prefer the Go SDK as the foundation for exec, behind our own adapter, provided
we fix and verify the lifecycle gaps below. Use Go's `net/http` for Sprite HTTP
jobs and for narrow API operations that need stronger result handling.** Do not
adopt an unmodified SDK and assume `exec.Cmd`-like behavior is a reliability
contract.

The SDK removes useful work: WebSocket framing, stdin/stdout/stderr handling,
exit decoding, session attachment, capability negotiation, and API metadata
models. Our scheduler still owns durable runs, deadlines, retries, deduplication,
and the meaning of an unknown outcome. Reimplementing all the protocol handling
would not solve those distributed-systems problems.

However, the current SDK is not sufficient as-is for the proposed 60-second
disconnect window and reliable cancellation. Local probes also found a shutdown
data race. Treat lifecycle fixes as a gate before production execution, not
optional improvements after launch.

Proposed implementation choice:

| Responsibility | Choice |
| --- | --- |
| Target discovery and metadata | SDK `GetSprite`, with bounded/custom HTTP transport and normalized errors. |
| Exec start, streaming, exit, and attach | Pinned SDK, initially with control connections disabled, plus reviewed lifecycle fixes. |
| Termination confirmation | Small direct HTTP adapter parsing the kill endpoint's progress stream; reconcile with session/exit evidence. |
| Session listing | SDK only after live schema verification; a strict compatibility decoder if both documented and SDK shapes occur. |
| Calling a user's HTTP job endpoint | Dedicated standard-library HTTP client; this is not the exec API. |
| Scheduling, state, retry decisions | Our own durable scheduler, independent of SDK internals. |

If a small, maintainable SDK patch cannot deliver the required command lifecycle,
use a focused direct WebSocket exec adapter. Do not choose HTTP POST exec solely
because it looks simpler; its completion and recovery behavior needs equivalent
verification.

## Versions and method

Cloned the official public repository and inspected implementation files, tests,
module requirements, tags, and CI configuration. Exact snapshots:

| Snapshot | Commit | Commit date | Required Go version |
| --- | --- | --- | --- |
| Latest tag found, `v0.2.1` | `3ff87fc9eb917d6d47e2ca9ef832cfbbe7a217e9` | 2026-09-09 | 1.25.8 |
| `main` at research time | `d5d8f95cf4a35be923bda318f4fcbd40c1a9c439` | 2026-09-24 | 1.26.0 |

The SDK is MIT-licensed. Current main adds bounded/redacted connection diagnostics
and copies the default WebSocket dialer rather than mutating the shared pointer
used in v0.2.1. The lifecycle findings below remain relevant to both snapshots.
Prefer a reviewed immutable revision containing these fixes over a floating
`main` dependency. Re-evaluate available releases when implementation begins.
[Tagged module](https://github.com/superfly/sprites-go/blob/3ff87fc9eb917d6d47e2ca9ef832cfbbe7a217e9/go.mod),
[current module](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/go.mod),
[license](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/LICENSE).

Tests used Go 1.26.0 on Linux/amd64. SDK source, dependencies, and toolchains were
kept outside the application tree. No real tokens, remote commands, or live
Sprite mutations were needed. Local servers bound ephemeral loopback ports.
The checked-in [research probes](research/sprites_sdk_test.go.txt) are reproducible
experiments, not the beginning of the application implementation.

## What the SDK does well

- `Command`/`CommandContext` accept separate arguments; `Dir`, `Env`, and I/O
  writers let us configure execution without constructing a shell string.
- Non-TTY framing separates stdout, stderr, and exit. Missing exit information
  produces an error and an exit code of -1 in the tested direct path.
- `AttachSessionContext` and `SetOutputOffset` offer a starting point for recovery
  of known sessions and output replay.
- `GetSprite` supplies identity and URL metadata. Existing response types reduce
  hand-maintained JSON models.
- Signals, server-version compatibility, and optional control connections are
  implemented already. These are meaningful maintenance savings over owning the
  complete protocol.

Sources: [command implementation](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec.go),
[WebSocket implementation](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go),
[session operations](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/session.go),
[metadata operations](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/management.go).

## Findings that affect our scheduler

### 1. Context cancellation does not establish remote termination

The README and `CommandContext` comment imply that cancelling the context kills
the process. In the inspected direct WebSocket path, `Wait` selects on context
cancellation and returns its error, while socket reading happens separately.
There is no corresponding automatic signal request in that path.

**Local observation, both snapshots:** after cancellation, `Wait` returned
`context.Canceled`, no kill request or WebSocket signal was observed, and the
connection stayed open during a 250 ms observation window. This establishes that
we cannot infer termination or even immediate detachment from `Wait` returning.
It does not establish how long a real server will retain the command.

`Cmd.Signal` can also fall back to HTTP using the command's already-cancelled
context. For timeout cleanup, use a fresh bounded cleanup context and the stored
session ID. Keep execution outcome and remote liveness separate. A lifecycle fix
must expose deterministic detach/close and wait for local I/O shutdown without
mistaking detachment for killing the process.
[Command context and signal code](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec.go),
[WebSocket wait and read loop](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

### 2. The disconnect lifetime is not a public command setting

The API documents `max_run_after_disconnect`, including a 10-second default for
non-TTY commands. Neither inspected SDK version exposes a corresponding command
field/setter or adds that option in its direct/control exec argument builders.
The wire-options probe confirmed its absence from a normal direct request.

The design's 60-second window therefore requires a small SDK addition or direct
WebSocket handling. Do not enable TTY just to change disconnect behavior: that
changes output semantics. Do not hide the option in the client's base URL; that
is global, brittle, and does not configure the control argument builder.
[Exec reference](https://sprites.dev/api/sprites/exec),
[SDK URL builder](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec.go),
[control argument builder](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

### 3. Session IDs require a callback; start is not durable acknowledgement

Public `Cmd` has no session-ID accessor in these snapshots. Its internal
WebSocket object has one, but callers cannot reach that object. The supported
escape hatch is `TextMessageHandler`: parse a `session_info` message and persist
its ID. The local probe observed this callback before completion.

Install the handler before `Start`. It runs on the receive path, so keep it
bounded and never call back into command methods that might contend with Start's
lock. Arrange prompt durable persistence and surface failures to the supervisor;
do not silently drop session events in an overflowing queue. A crash before the
ID is committed still creates an unknown outcome. `Start` returning successfully
is not proof that the ID has been received or stored.

There is no automatic durable reconnect manager. Attach to the recorded ID;
never identify a session by matching command text. Verify the meaning of
`output_offset` against the server, retain a durable output cursor, and distinguish
intentional truncation from replay gaps. Session-list absence does not prove
that a command never ran.
[Command and callback plumbing](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec.go),
[message handling](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

### 4. Signal success is weaker than termination success

`SignalSession` posts to the kill endpoint with `timeout=0s`. It accepts HTTP 200
or 410 and does not parse the successful response body. A local server returning
HTTP 200 with an NDJSON error event still produced a nil SDK error.

Use a small direct HTTP call when we need progress/termination confirmation.
Bound the response, parse event types, and preserve uncertainty if the stream
ends without adequate completion evidence. A successful signal send does not
prove that all child processes or external side effects have stopped.
[Signal implementation](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/client.go),
[kill endpoint contract](https://sprites.dev/api/sprites/exec).

### 5. Output needs explicit protection

`Output` and `CombinedOutput` accumulate output in memory without an application
size cap. More surprisingly, although public comments describe nil output as
discarded, the lower-level loop substitutes the scheduler process's stdout and
stderr. Always provide bounded writers, or explicitly use `io.Discard`.

The local probe found that a writer returning a storage error was ignored and
`Run` still returned success on remote exit zero. Our writer must separately
record capture failures and truncation; the supervisor must not infer successful
storage from `Run`. Continue draining after the retention limit so the remote
process does not block. Track remote execution success separately from output
capture failure. Blocking indefinitely inside a writer can stall lifecycle
handling, so use bounded buffering and explicit failure propagation.
[Output helpers](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec.go),
[output dispatch](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

### 6. Custom HTTP policy does not automatically cover WebSockets

`WithHTTPClient` controls REST calls, but exec creates a separate Gorilla dialer.
A mock transport that rejects every HTTP request was never called during a
successful direct exec probe. `WithNetDialContext` provides a shared TCP hook,
but is not a complete HTTP policy hook: it does not carry a custom HTTP client's
redirect, TLS, timeout, or response-size settings into the WebSocket dialer.

Keep the API origin fixed, validate target names, explicitly configure direct
HTTP policies, and test dial restrictions on every path. If combining
`WithNetDialContext` with a custom non-`*http.Transport` round tripper, note that
construction replaces that transport with a default transport before applying
the dialer. Compose instrumentation/limits deliberately rather than assuming
all options combine transparently. The constructor also wraps the supplied HTTP
client's transport: provide an owned client, not a shared global instance.
[Client construction](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/client.go),
[WebSocket dialing](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

### 7. Default control mode adds behavior we do not initially need

`client.Sprite(name)` probes a control connection using an internal timeout
(default two seconds), so it is not merely a local handle constructor. Exec can
use pooled control connections, with fallback to direct mode. Attach also has
server-version and legacy-endpoint handling. The server version is cached on the
client, even though version discovery is Sprite-specific.

For the first version, use `WithDisableControl()` and cache owned clients by
credential version **and target**, not one client shared indiscriminately across
all Sprites. This keeps version state isolated and reduces the initial test
surface. Measure connection cost before enabling control mode. Do not mistake
`SetControlMode(false)` for the global disabling option.

`Client.Close` closes control pools; it is not a public per-command detach API
for active direct connections. Explicit command cleanup still needs fixing.
[Client behavior](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/client.go),
[control discovery](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/sprite.go),
[control option](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/exec_options.go).

### 8. Session response shapes differ between source and documentation

The API examples show an array of sessions. SDK `ListSessions` decodes an object
containing `sessions`; the local probes accepted that object and rejected the
array. An object missing a valid sessions array can be treated as an empty list.
This is a demonstrated contract mismatch, not proof that the live service is
broken. Verify its actual versioned response before choosing a decoder; reject
malformed data rather than treating it as proof of no sessions.

Also avoid `Session.IsSessionActive()` as a liveness authority: it factors in
recent activity, so a quiet process can fail that heuristic. A lack of output
is not a completed process.
[Session decoder and activity helper](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/session.go),
[session examples](https://sprites.dev/api/sprites/exec).

### 9. Race detection found a connection-shutdown problem

The missing-exit probe under `-race` reported concurrent access between
`wsCmd.Close` calling Gorilla's `SetWriteDeadline` and the adapter's write loop
sending data. It reproduced on both snapshots in isolated 20-run tests. This
happened with a normal server close while the SDK could still send stdin EOF;
our test did not call concurrent client command methods.

This is a local SDK concurrency finding, not a live-platform outage claim.
Serialize shutdown with writes and verify goroutine cleanup before production.
Do not suppress the race detector or call the SDK safe merely because ordinary
unit tests pass. The retained reproduction should become a regression test for
any patched version.
[Close and writer code](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/websocket.go).

## SDK versus writing the protocol ourselves

| Approach | Advantages | Costs and decision |
| --- | --- | --- |
| Unmodified SDK | Fast happy-path implementation; official models and protocol code. | Does not satisfy lifecycle requirements found here. Reject as the production contract. |
| SDK plus small fixes and direct REST helpers | Reuses complex streaming code while letting us enforce durable state and lifecycle. | Requires a pinned revision, a small patch surface, and regression tests. Recommended first implementation path. |
| Direct REST plus our own WebSocket exec | Complete control over lifetime, events, errors, and transport policy. | We own framing, keepalive, replay, attach compatibility, cancellation, and concurrency. Fallback if SDK fixes become extensive. |
| HTTP POST exec only | Simpler request initiation and potentially fewer dependencies. | Current reference does not fully establish result framing/session recovery for our needs. Do not select without a live compatibility spike. |

Direct access does not require avoiding dependencies entirely: a focused adapter
would still use a maintained WebSocket library. The decision is which layer of
protocol maintenance we own, not whether Go can send HTTP requests.

Sprite HTTP jobs remain ordinary HTTPS requests to the discovered Sprite origin.
The SDK's TCP proxy helpers are a different transport and do not automatically
implement our HTTP job contract, body limits, idempotency, or application auth.
We do not need to introduce that tunnel for v1.
[Proxy implementation](https://github.com/superfly/sprites-go/blob/d5d8f95cf4a35be923bda318f4fcbd40c1a9c439/proxy.go).

## Proposed adapter boundary and acceptance gate

Keep SDK types out of storage and scheduling. Define application-owned operations
for validating a target, starting an attempt, attaching to a saved session,
requesting termination, and detaching local observation. Emit typed events for
session acknowledgement, output, remote exit, and transport failure. Persist
those events according to the durable execution design.

Before committing to the SDK in production:

1. Add explicit disconnect-lifetime configuration and deterministic detach/close;
   fix the shutdown race and verify no leftover sockets/goroutines.
2. Persist session IDs from callbacks; verify fast-exit and failed-persistence
   cases. Add a typed accessor/event upstream if it reduces unsafe callback use.
3. Use a separate cleanup context and verified kill-stream handling. Keep timeout,
   signal acceptance, confirmed exit, and unknown liveness distinct.
4. Normalize SDK errors using `errors.As`/`errors.Is`, not string matching.
   Current connection diagnostics wrap structured errors; the SDK's direct
   `IsAPIError` assertion does not unwrap every wrapper. Keep raw error output
   private and redacted; not all API methods have identical diagnostic handling.
5. Explicitly bound both output streams, surface capture errors, and keep SDK
   debugging disabled. Do not put secrets in command arguments or query strings.
6. Run race-enabled loopback regression tests, then live integration tests for
   non-TTY reconnect, output offsets, environment inheritance, cold start,
   completed-session retention, credential rotation, and process-tree cleanup.

Propose small upstream fixes when implementation is authorized; no issue or PR
has been sent as part of this research. If needed, use a clearly identified
pinned fork with its patch list recorded. Never edit the Go module cache or
silently depend on a moving branch. If the fixes expand into a redesign of the
SDK execution engine, implement the narrow direct adapter instead.

## Test results and reproduction

Seven research test functions, with two session-shape subtests, passed normally
on both snapshots. Passing here means the observed behavior was reproduced,
including the limitations; it does not mean those limitations are acceptable.
The current-main upstream suite passed before adding the probes. The v0.2.1
suite plus probes passed normally. Live lifecycle tests were not exercised in
this initial stage; see the subsequent [live report](live-sprite-validation.md).

The current-main research suite failed with `-race` on connection shutdown; an
isolated 20-run missing-exit reproduction failed on both current main and v0.2.1.
Race timing can vary.
Use the commands below to reproduce against a fresh, disposable SDK checkout:

```sh
git clone https://github.com/superfly/sprites-go.git /tmp/sprites-sdk-review
cd /tmp/sprites-sdk-review
git checkout --detach d5d8f95cf4a35be923bda318f4fcbd40c1a9c439
cp /path/to/sprite-cron/docs/research/sprites_sdk_test.go.txt ./sprite_cron_research_test.go
env -u SPRITES_TEST_TOKEN GOTOOLCHAIN=go1.26.0 go test -run '^TestResearch' -v -count=1 -timeout=30s .
env -u SPRITES_TEST_TOKEN GOTOOLCHAIN=go1.26.0 go test -race -run '^TestResearchMissingExitIsNotSuccess$' -count=20 -timeout=30s .
```

Repeat with commit `3ff87fc9eb917d6d47e2ca9ef832cfbbe7a217e9` to compare the
latest tag. The probes deliberately disable control mode and do not exhaustively
test that mode. They cannot answer server-side retention, idempotency, permission,
or termination guarantees; those remain explicit live-test requirements.
