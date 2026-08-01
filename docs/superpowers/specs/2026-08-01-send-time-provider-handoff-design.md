# Send-Time Provider Handoff Design

## Status

This document defines automatic provider handoff when Codex Desktop sends the
next `turn/start` for an existing thread. It extends the v0.2.1 WebSocket proxy
without replacing or restarting the shared app-server daemon.

## Verified App-Server Behavior

Codex app-server 0.146.0 keeps a loaded thread bound to its current
`modelProvider` while it has subscribers or an active turn. A resume request
with a different provider normally rejoins that runtime and the override is
ignored.

The same version has a specific handoff path in
`request_processors/thread_processor.rs`: when a resume override differs and
the loaded thread has no subscribers, has `idle` status, and has no running
agent, app-server shuts down and removes the cached runtime, reloads persisted
history, and applies the new provider. This path does not wait for the normal
30-minute no-subscriber unload grace period.

Public `thread/unsubscribe` only affects the requesting app-server connection.
There is no public `thread/close` request, so every connection subscribed to the
target thread must cooperate before the different-provider resume can take the
handoff path.

References:

- <https://github.com/openai/codex/blob/rust-v0.146.0/codex-rs/app-server/src/request_processors/thread_processor.rs>
- <https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>

## User Semantics

Listing or opening a thread does not claim its provider. Existing behavior is
preserved for `thread/list` and ordinary `thread/resume`.

The connection that sends the next `turn/start` requests ownership for its
configured provider:

```text
Desktop B sends turn/start(thread T)
  -> lock T across switcher processes
  -> verify no cooperating connection reports an active turn for T
  -> unsubscribe every cooperating connection from T
  -> internally resume T on B with provider B
  -> verify app-server reports provider B
  -> send the original turn/start
```

After B's turn completes, a later turn from A repeats the process and can move
the same persisted thread back to provider A. The thread id and persisted turn
history do not change.

## Scope And Non-Goals

The feature coordinates transparent wrapper processes for one Unix user and
one app-server socket. It does not:

- restart or signal app-server;
- create a second daemon;
- edit SQLite or rollout JSONL files;
- fork or alias thread ids;
- interrupt an active turn;
- coordinate clients that bypass the switcher;
- promise simultaneous control of one thread from different providers.

The existing trusted same-user threat model remains unchanged.

## Runtime Namespace

All switcher processes targeting the same validated app-server socket derive a
short runtime directory:

```text
/tmp/cps-<uid>-<socket-sha256-prefix>/
```

The directory is mode 0700. The socket hash prevents coordination between
different app-server daemons while keeping Unix socket paths below macOS path
limits.

Each proxy connection creates one randomly named Unix control socket in this
directory. Live control sockets form the peer registry; no long-lived
coordinator daemon or persistent ownership database is introduced. Dead socket
entries are ignored and removed only after a failed connect confirms there is
no listener.

Per-thread lock files use the SHA-256 of the thread id. An OS `flock` serializes
resume and turn-start transitions across wrapper processes. File locks are
released automatically if a process exits.

## Connection Session

`internal/transport` replaces the generic two-pump bridge with a session object
that owns:

- serialized upstream WebSocket writes;
- internal JSON-RPC request ids and response waiters;
- Desktop request metadata needed to interpret app-server responses;
- the latest resume template per thread;
- effective provider per thread on this connection;
- locally active thread ids;
- the per-connection control socket.

Server notifications and ordinary Desktop responses remain byte-for-byte
pass-through. Responses to switcher-internal requests are consumed by the
session and never exposed to Desktop.

Internal request ids are random string ids under a reserved switcher prefix.
They cannot collide with Desktop ids in practice, and the pending map still
requires an exact id match before suppressing a response.

## Request Tracking

Downstream JSON text messages are classified without changing unrelated
messages:

- `thread/start`, `thread/resume`, and `thread/fork` still receive the
  connection provider.
- `thread/list` still receives `modelProviders: []`.
- `thread/resume` stores its rewritten params as the future internal resume
  template for that thread.
- app-server responses to `thread/start` and `thread/resume` record the returned
  thread id and effective `modelProvider`.
- `turn/start` extracts a string `params.threadId` and enters the handoff path.

Every ordinary `thread/resume` and every `turn/start` acquires the same
per-thread cross-process lock. A resume therefore cannot add a subscriber in
the middle of another connection's handoff.

When the connection already knows that the effective provider equals its own
provider, `turn/start` is sent directly under the per-thread lock. Otherwise it
performs a handoff first. Missing or malformed `threadId` fails closed with a
sanitized routing error.

## Peer Control Protocol

The local control protocol is length-bounded JSON over Unix stream sockets. It
supports two commands:

```json
{ "method": "prepare", "threadId": "..." }
{ "method": "unsubscribe", "threadId": "..." }
```

The initiating session first sends `prepare` to every peer. A peer checks its
local active set and returns `busy` or `ready` without changing app-server
state. Because the initiator already holds the per-thread lock, a ready peer
cannot begin a new turn before the second phase.

Only after every peer reports ready does the initiator send `unsubscribe` to
every peer. The peer checks the active set again, sends an internal
`thread/unsubscribe` request, clears its local effective-provider state for the
thread, and returns one of `unsubscribed`, `notSubscribed`, or `notLoaded`.

The initiating session contacts all peer sockets, including itself. Connection
refusal from a stale socket is treated as a dead peer. Timeout, malformed
response, `busy`, or an app-server JSON-RPC error aborts the handoff. The
two-phase check prevents a known active turn from causing partial unsubscription;
an unexpected second-phase failure still fails closed and is repaired by the
next resume or handoff attempt.

## Handoff Verification

After every peer acknowledges unsubscribe, the sender issues an internal
`thread/resume` on its own upstream connection. It reuses the last Desktop
resume params for that thread when available, overwrites `modelProvider`, and
uses a minimal `{threadId, modelProvider}` request otherwise.

The handoff succeeds only when the internal response contains the requested
thread id and effective `modelProvider` equal to the connection provider. This
guards against:

- a non-switcher subscriber that remained attached;
- an active app-server turn not visible to the registry;
- app-server version drift;
- shutdown failure in the loaded-thread replacement path.

On mismatch or error, the original `turn/start` is not forwarded. The switcher
returns a JSON-RPC error using the original Desktop request id with a static
message. It keeps the WebSocket alive so the user can retry after the active
turn finishes.

## Active Turn Protection

A session marks a thread active before forwarding its `turn/start`, while it
still holds the per-thread lock. It clears the mark when:

- app-server returns an error for that `turn/start` request;
- a `turn/completed` notification for the thread arrives;
- the connection closes.

Control requests never unsubscribe an active session. The feature does not
send `turn/interrupt` and does not steal a thread with an in-progress approval,
tool call, or model turn.

## Failure And Privacy Rules

Coordination is fail closed. No original `turn/start` reaches app-server unless
the effective provider is already correct or the verified handoff succeeds.

Diagnostics never contain prompts, JSON bodies, thread ids, socket paths,
provider credentials, or internal responses. Control messages are bounded,
runtime sockets are local and mode-protected, and all operations honor context
cancellation and short timeouts.

## Testing

Unit tests cover JSON-RPC classification, response correlation, internal id
suppression, lock cancellation, runtime namespace isolation, stale peer cleanup,
busy peers, and sanitized errors.

A multi-connection WebSocket-over-UDS integration server models app-server
0.146.0 loaded-thread behavior. Tests prove:

- A resumes T with provider A and B can open T without switching it.
- B's next `turn/start` unsubscribes A and B, cold-resumes T with provider B,
  and starts the turn under B without changing the thread id.
- After completion, A's next turn switches the same thread back to A.
- An active A turn makes B's send fail without interrupting A or forwarding B's
  turn.
- A non-cooperating subscriber causes effective-provider verification to fail.
- Switcher-internal requests and responses never reach either Desktop client.

Existing framing, close, short-I/O, message-size, wrapper, repository, race,
macOS, and cross-build gates remain required.

## Compatibility

Automatic handoff requires Codex app-server 0.146.0 or newer behavior that
replaces an idle zero-subscriber loaded thread when resume overrides differ.
Against older servers, verification detects the unchanged provider and rejects
the turn instead of silently routing it incorrectly.

The feature is enabled by default because it only changes `turn/start` when a
provider mismatch is observed. No new installation service is required.

## Post-Release Peer Resynchronization Amendment

Every `turn/start`, including the same-provider fast path, runs `PrepareAll`
while holding the per-thread lock. The sending session marks the thread active
before its upstream write, so another cooperating session that acquires the
lock next observes `busy` even before app-server broadcasts `turn/started`.

Provider changes use a three-phase subscription transition:

```text
prepare all -> unsubscribe all -> sender cold resume
            -> resubscribe detached peers -> forward turn/start
```

Each peer remembers whether the coordinator actually detached its app-server
connection. After the sender establishes the new effective provider, a
`resubscribe` control request makes only those detached peers issue an internal
`thread/resume` using that effective provider. Codex 0.146.0 automatically
reattaches the requesting connection's thread listener. Internal resume
responses remain hidden from Desktop, but all previously open Desktop views
are subscribed again before any new turn starts, so none miss turn events.

Explicit Desktop unsubscribe is not treated as coordinator detachment and is
never undone. A failed or uncertain coordinator unsubscribe retains detached
state for repair on the next successful handoff. Failure to resubscribe any
detached peer aborts the original turn before it reaches app-server.

The deployment check compares `/proc/<daemon-pid>/exe`, its digest, and the
installed real Codex binary. On the reference host the running daemon is
Codex 0.146.0; the 0.144.5 binary belongs to the outer Codex agent sandbox and
is not the app-server behind the switcher socket.

## Failure Recovery Amendment

The runtime namespace contains a per-thread dirty marker using the same digest
as the handoff lock. A cross-provider transition probes `prepareHandoffV2`,
creates the marker before the first unsubscribe, and removes it only after the
sender resume and every peer resubscribe succeed. Every `turn/start` treats a
dirty thread as requiring a complete handoff even when the local effective
provider already matches the selected provider.

Every failure after the marker is created runs a best-effort `restore` control
operation against all live peers. A detached peer sends a minimal
`thread/resume` containing only `threadId`, accepts the app-server's actual
effective provider, restores its listener, and updates its local provider
state. Restore does not request a provider change. One unavailable peer does
not prevent restore attempts for later peers, and the dirty marker remains so
the next sender retries the full transition.

`prepareHandoffV2` is a protocol capability boundary. Older peers reject it
before subscription state changes, so a mixed v0.3.1/v0.3.2 deployment cannot
enter a handoff that lacks global dirty and restore support.
