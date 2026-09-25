# Live Sprite validation

Tested September 25, 2026 against the user-authorized `test-sprite-cron`, reporting
Sprite version `0.0.2-beta.3`. This supplements the
[SDK source review](sprites-go-sdk-research.md) with live observations.
[Structured evidence](research/live-results-2026-09-25.json) records phase results.

## Decision after live testing

Keep the recommendation: use a pinned, corrected Go SDK behind an execution
adapter, with direct HTTP for termination progress and for Sprite HTTP jobs.
The happy path works in both direct and control modes. The SDK lifecycle gaps
are real, and direct API access does not fix server-side recovery limitations.

In particular, we must support **unknown outcomes**. Reconnection is useful while
a session is active; a recently completed session was already unavailable for
attachment. If a future requirement is to recover the final result reliably
after a scheduler outage, add a cooperative runner on the Sprite that durably
records run IDs and outcomes. Do not promise that guarantee from session APIs
alone. Such a runner is additional product scope, not implemented by these tests.

## Scope and method

Initial inspection found no services or exec sessions. The Sprite HTTP URL was
already authenticated (`sprite` auth, `admins` private access); those settings
were not changed. All remote mutations were confined to our short-lived commands,
a temporary directory, and a temporary HTTP service.

Tested SDK revisions:

- Current source: `d5d8f95cf4a35be923bda318f4fcbd40c1a9c439`.
- Latest tag from the source review: `v0.2.1`, commit
  `3ff87fc9eb917d6d47e2ca9ef832cfbbe7a217e9`.

Go 1.26.0 was used for both. Basic execution was tested on both revisions, with
actual connection mode observed as `direct` and `control`. Detailed lifecycle
experiments used the current-source revision and direct API calls. Test commands
had natural completion or a remote `timeout` bound; recorded session IDs were
used for cleanup. No unrelated sessions were signalled.

The one-off harness and credential file stayed outside Git. The token was sent
only to the official API and the metadata-discovered HTTPS Sprite origin. It was
not passed into remote commands or the HTTP application. Captured evidence and
the repository were scanned for the token and its secret components before the
local token file was removed.

## Results

| Area | Live observation | Implication |
| --- | --- | --- |
| Basic exec | Both SDK revisions/modes preserved argument boundaries, `/tmp` working directory, separate stdout/stderr, and exit code 7. | SDK is a useful exec foundation. |
| Environment | With an explicit extra environment entry, PATH/HOME were still present and the marker arrived. Rechecked with a Python executable directly, bypassing shell defaults. | The reference's replacement wording does not describe this observation; pin behavior with integration tests. |
| Session listing | The server returned an object containing `sessions`, and SDK decoding succeeded. | The SDK/documentation mismatch is not a decoder failure on this server. |
| Context cancellation | In direct and control modes, SDK `Wait` returned `context canceled`; the command subsequently wrote its marker and remained active. | Cancellation is not remote termination. |
| Explicit kill | Direct kill API produced signal, exited, and complete events. | Parse the progress stream; status 200 alone is insufficient evidence. |
| Process tree | After explicit SIGTERM, the test Python process and its sleep child were absent from `/proc`. | Process-group termination worked for this ordinary tree; do not generalize to deliberately detached descendants. |
| Output writer failure | A writer rejected output; SDK `Run` returned nil with exit zero. | Track capture failure independently of remote exit. |
| In-progress reconnect | Direct API and SDK attachment resumed observation and obtained the final exit. | Recovery of a still-active session is feasible. |
| Replay offset | Both SDK and raw requests supplying `output_offset=6` received the entire prefix again. | Do not assume offset support just because the SDK has a setter. |
| Replay streams | Original stdout `ABCDEF` and stderr `ghi` replayed together as stdout `ABCDEFghi`. | Replayed output cannot be assumed to preserve original stream labels. |
| Completed-session attach | A direct attachment one second after exit returned 410. | Session attachment is not a durable result store. |
| HTTP POST exec | Returned HTTP 200, `application/octet-stream`, and stream markers, including process exit 7. | HTTP status does not convey command success; this is not a JSON result endpoint on the tested server. |
| HTTP bearer auth | The supplied Sprite token worked on the Sprite URL; omitting it returned 302 with redirects disabled. | Treat redirects as authentication/configuration failures, not successful job completion. |
| HTTP application auth | Custom application header arrived; the Sprite bearer Authorization header was absent in the handler. | Use a separate application header when needed; do not depend on receiving the edge token. |
| HTTP deduplication | Reusing an idempotency key returned the same stored effect, including after a lost response and explicit service restart. | Application-owned durable deduplication works; the platform does not supply this merely by accepting the header. |
| Service after explicit stop | Requests produced 502 and then a 15-second timeout; service state was failed. Explicit restart restored HTTP 200. | Do not rely on a request to recover a manually stopped/failed service without further platform validation. |

The raw POST result, for the harmless output test, was equivalent to:

```text
0x02 + "post-stderr" + 0x01 + "post-stdout" + 0x03 + 0x07
```

That is a captured example, not a complete framing specification. Output arrival
order and HTTP transport chunking can vary. We have not established how arbitrary
binary payloads must be delimited; a production decoder needs the actual protocol
contract. WebSocket message boundaries make the exec protocol better specified
for our present adapter design.

## Disconnect lifetime experiment

We started three non-TTY commands using direct WebSockets. Each printed a ready
marker before disconnect. They then wrote files at approximately 5, 15, and 30
seconds, proving progress independently of session-list entries. The commands
were bounded by a remote 45-second timeout. The only changed API argument was
`max_run_after_disconnect`: omitted, `1s`, or `60s`.

Measured from the experiment's start; all three disconnects occurred within
0.20 seconds:

| Setting | At 3 seconds | At 12 seconds | At 22 seconds | At 33 seconds |
| --- | --- | --- | --- | --- |
| Omitted | Listed active | Listed active; 5-second marker | Absent; 5- and 15-second markers | Absent; no 30-second marker |
| `1s` | Listed active | Absent; 5-second marker | Absent; no 15-second marker | Absent |
| `60s` | Listed active | Listed active; 5-second marker | Listed active; 15-second marker | Listed active; 30-second marker |

We then explicitly killed the remaining `60s` session and received completion.
The experiment confirms that the parameter influences survival, but enforcement
was not an exact wall-clock deadline: the `1s` job reached its 5-second write,
and the omitted/default job reached its 15-second write. Periodic cleanup could
explain this, but that is an inference, not a verified implementation detail.
We did not measure the actual expiration of a full 60-second window.

Use this setting as a recovery allowance, not as the job's hard timeout. Enforce
job deadlines separately and require termination evidence before unblocking
non-overlapping work. The SDK still needs an explicit option to configure this
parameter; the live server supporting it does not remove that SDK gap.

## Replay and completion experiment

A non-TTY process wrote six stdout bytes (`ABCDEF`), three stderr bytes (`ghi`),
then later two stdout bytes (`JK`) and exited 4. After receiving the first nine
bytes, the observer disconnected. An SDK attachment requested output offset 6.
It returned stdout `ABCDEFghiJK`, empty stderr, and exit 4.

A separate raw WebSocket experiment sent `stdin=false`, `tty=false`, and
`output_offset=6` to `/exec/{session_id}`. Session metadata explicitly reported
`tty: false`. Replay still arrived as one stdout frame containing `ABCDEFghi`,
followed by live stdout `JK` and exit 4. This isolates the observed replay behavior
from the SDK's option serialization. The response advertised signal capability;
it did not establish output-offset support.

A post-completion attachment returned 410. This is one immediate-retention
observation, not a universal retention-duration claim. Persist exit information
promptly. If it is lost, preserve `unknown` unless another durable source resolves
it. Reattached output should be marked as replayed/possibly duplicated, with
original stderr separation potentially unavailable.

## HTTP application experiment

Created a temporary Python service through `sprite-env services create` with an
HTTP port. Its endpoints returned only test data: health, a small JSON result,
202, or a relative redirect. It did not serve files, inspect environment variables,
or return authentication header values. Header presence was reported only as
booleans. Its logs did not record incoming requests.

The handler required a separate application header and used SQLite to store an
idempotency key and a simulated effect counter in one transaction. Results:

1. First request produced effect 1. Repeating its key returned effect 1 with a
   duplicate flag.
2. A second key committed effect 2, then delayed its response for two seconds.
   The client timed out after roughly 0.31 seconds.
3. Retrying that key returned effect 2 with the duplicate flag; it did not create
   another effect.
4. After an explicit service restart, the original key still returned effect 1.

This demonstrates an application contract for database-contained effects. It does
not make arbitrary external side effects exactly-once: those need their own
idempotency or transactional coordination.

The 202 and 302 endpoints preserved those statuses with redirects disabled.
The scheduler should retain the proposed distinction between acceptance and
completion, and should never follow an unexpected redirect carrying credentials.

An explicit stop left this service failed (exit 143). The next request returned
502, followed by a request timing out at 15 seconds. Explicit restart restored
normal responses. This is **not** a test of a naturally hibernated Sprite waking
up: do not treat explicit service stop, failed-service recovery, and VM wake as
the same operation. True cold/hibernated wake still needs a separate controlled
experiment.

## Cleanup and remaining uncertainty

Deleted the temporary service and remote test directory. Final inspection showed
no services and no exec sessions; the test directory was absent. Removing the
platform-managed service log was denied because that path is read-only, so that
platform log may remain. The test handler did not log requests or credential
values. The local credential file was removed after the evidence scan.

The following remain unverified:

- True cold/hibernated wake, prolonged outages, and cross-version compatibility.
- Token scope boundaries, expiration, rotation, or revocation; only the supplied
  token's successful access was tested, and it was not altered or revoked.
- Offset replay in control mode, exact disconnect expiry, long-term completed
  session retention, and server behavior under high load.
- Detached/daemonized descendants, ignored signals, and kill escalation under
  failure. The process-tree result covers one ordinary child relationship.
- Exactly-once external side effects or a durable remote runner.

The previously detected local SDK shutdown race remains a release blocker; these
live tests did not fix it or certify the SDK race-free. Test the patched revision
under the race detector and repeat these live compatibility cases before using
it for production scheduling.
