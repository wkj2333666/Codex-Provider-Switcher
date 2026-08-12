# SystemError Provider Recovery Design

## Summary

Codex app-server can keep a loaded thread in `systemError` after a terminal
model or transport failure. In Codex 0.146.0, an immediate `thread/resume`
provider override replaces an idle, zero-subscriber runtime, but it does not
replace a runtime whose status remains `systemError`. The switcher therefore
verifies the old provider, fails closed, and Desktop reports that the message
could not be submitted.

This change adds a switcher-owned recovery path without modifying Codex,
starting another app-server, replaying user input, or sending a model turn. It
uses stock app-server `thread/archive` and `thread/unarchive` requests as a
transactional soft unload before retrying the existing verified handoff.

## Goals

- Recover a selected provider after a terminal `systemError`.
- Keep the original thread id and persisted history.
- Never forward or synthesize a model turn to clear app-server state.
- Keep all cooperating Desktop connections subscribed after recovery.
- Hide the internal archive lifecycle from Desktop.
- Recover safely after a switcher crash or partial protocol failure.
- Continue to fail closed whenever the runtime provider cannot be verified.

## Non-Goals

- Modifying or patching Codex app-server.
- Restarting or supervising the shared app-server daemon.
- Recovering an active turn.
- Automatically resending the user's failed message.
- Virtualizing thread ids or replacing a thread with a fork.
- Supporting a non-switcher subscriber during provider handoff.

## Supported Connection Boundary

Soft reload is available by default in every intercepted `app-server proxy`
process. It has no environment variable or command-line activation gate. The
supported deployment routes every client that subscribes to the same
app-server threads through the switcher wrapper. Codex Desktop and
android-ssh-codex both use the intercepted `codex app-server proxy` path in
that deployment.

A client that directly opens the app-server socket or deliberately invokes a
real Codex binary outside the wrapper is outside the supported provider
handoff architecture. The stock app-server exposes no subscriber census, so
the switcher cannot discover or coordinate such a bypassing connection.
Requiring an environment variable would only ask the operator to restate this
deployment fact; it would not detect or prevent a bypass and therefore would
not add a technical safety guarantee.

Delegated Codex CLI commands retain their original argv and environment. The
switcher does not modify or supervise the Codex binary or app-server daemon.

Soft reload is eligible only when all of the following are true:

1. The requested provider differs from the verified effective provider.
2. Every cooperating peer reports the thread is not active.
3. Every peer supports the recovery coordination protocol before mutation.
4. The normal unsubscribe and `thread/resume` handoff returns the same thread
   id but a different provider.
5. A fresh internal `thread/read` confirms the root status is `systemError`.
6. The app-server supports every RPC used by the disposable compatibility
   probe.

Other resume failures, malformed responses, unknown status, and version drift
retain the existing static fail-closed error.

## Probe Gate

Before production code is enabled, an isolated probe against the installed
stock app-server must establish that:

- `thread/archive` unloads a thread in `systemError`;
- `thread/unarchive` restores the same thread id and persisted turns;
- a following `thread/resume` applies a different `modelProvider`;
- no `turn/start` or model request is required;
- reconnecting and reading the thread shows unchanged history;
- internal archive notifications can be consumed without leaking to Desktop.

The probe uses a disposable thread and deletes no user thread. A true spawned
agent descendant cannot be created through the public protocol without a model
turn, so subtree behavior is verified separately against the generated
protocol schema, the upstream archive implementation, and the integration fake.
If any root-thread gate cannot be demonstrated, production soft reload is not
implemented and the switcher keeps the current fail-closed behavior with a
clearer diagnostic.

## Recovery Transaction

The existing per-thread lock remains the serialization boundary. A recovery
transaction performs:

```text
prepare all peers
  -> mark handoff dirty
  -> unsubscribe all peers
  -> normal resume and provider verification
  -> on verified SystemError mismatch only:
       open an initialized internal control connection
       enumerate spawned descendants with ancestorThreadId
       persist recovery journal
       begin notification suppression on all peers
       archive the root subtree
       unarchive the pre-enumerated root and descendant ids
       resume the root with the target provider
       verify root id and provider
       end notification suppression
  -> resubscribe detached peers with verified provider
  -> persist provider selection
  -> clear recovery journal and handoff dirty marker
```

The control connection enables `experimentalApi` only for internal requests;
the switcher does not alter Desktop's initialize capabilities. Enumeration
pages `thread/list` with `ancestorThreadId`, rejects duplicate or malformed
ids, and caps a recovery subtree at 64 threads so the peer-control protocol
remains below its 4 KiB message limit. Failure or truncation occurs before
archive mutation and fails closed.

The first normal resume remains important: it preserves the fast path for
future Codex versions and distinguishes the known stuck-runtime case from
other compatibility failures.

## Recovery Journal

Recovery state lives in the configured persistent `StateDir` and uses a
per-thread file in a private recovery subdirectory. The `/tmp` peer namespace
is deliberately not used because a reboot must not erase repair state while a
thread remains archived. The journal contains only:

- schema version;
- root thread id;
- requested provider;
- phase;
- the complete bounded thread id set used for peer notification isolation;
- thread ids confirmed archived but not yet confirmed unarchived.

Writes use a mode-0600 temporary file, file sync, atomic rename, and directory
sync. The journal never contains prompts, history, credentials, paths from
messages, or provider configuration.

Before accepting another provider command or model turn for a journaled
thread, the lock holder repairs it by unarchiving every recorded id. It treats
an id as already unarchived only when app-server returns the exact stock
`-32600` "no archived rollout found" error for that id and `thread/read`
confirms the same id exists. It then resumes the root without a provider
override to discover the actual runtime. It clears the journal only after the
app-server confirms the root is available. An uncertain repair remains dirty
and fails closed.

## Notification Isolation

`thread/archive` and `thread/unarchive` broadcast lifecycle notifications to
all app-server connections. The coordinator therefore gains a recovery mode
for one bounded root-and-descendant id set. Every peer acknowledges that mode
before archive mutation and suppresses only matching internal notifications:

- `thread/archived` for ids recorded by the transaction;
- `thread/unarchived` for the same ids;
- `thread/closed` and `thread/status/changed` transitions caused by the
  temporary unload.

All unrelated notifications continue downstream byte-for-byte. Recovery mode
is removed after successful reload or best-effort repair.

## Descendant Threads

Codex archives the spawned subtree with a root. The switcher must not assume
that unarchiving the root restores descendants. Before mutation, the internal
control connection queries every spawned descendant with `ancestorThreadId`
and journals the complete set. Recovery restores descendants before the root,
matching Codex's subtree archive preparation order. If the complete moved set
cannot be established, recovery stops before archive. If a later archive or
restore response is uncertain, the journal retains the pre-enumerated set for
idempotent repair.

## Failure Semantics

- No provider selection is saved before final provider verification.
- No original `turn/start` is forwarded during recovery.
- A failed switch command returns a sanitized local error.
- Best-effort repair never hides uncertainty by clearing dirty state.
- Existing subscriptions are restored with the actual verified provider.
- The switcher never archives a thread unless all known peers are quiescent
  and participating in the recovery protocol.
- A known non-cooperating subscriber continues to make ordinary handoff fail
  closed before recovery. Deliberately bypassing the wrapper is unsupported.

## Testing

Tests extend the multi-connection fake app-server with `systemError`, archive,
unarchive, descendants, broadcast notifications, and injected failures.
Coverage includes:

- successful provider-only recovery with zero model turns;
- unchanged thread id and history fixture;
- no archive lifecycle visible downstream;
- descendant restoration;
- crash journals at each mutation boundary;
- idempotent repair;
- recovery enabled without environment or command-line configuration;
- ordinary mismatch without `systemError` still failing closed;
- peer capability rejection before mutation;
- selection persistence only after verified recovery;
- existing idle handoff remaining unchanged.

The release gate also runs the disposable real app-server probe, `go test
./...`, `go test -race ./...` where supported, and `go vet ./...`.

## Probe Result

Probe date: 2026-08-03. Installed Codex: `codex-cli 0.146.0`.

- SystemError reproduced: pass.
- Normal resume provider mismatch reproduced: pass.
- Root archive notification observed on both initialized clients: pass.
- Root restored with the same id: pass.
- Same root id resumed with `probe-alt`: pass.
- History digest and turn count unchanged: pass in two fresh temporary homes.
- No additional `turn/started` notification: pass; count remained one.
- Spawned descendant restoration: not claimed by the zero-model probe. A
  normal `thread/fork` was rejected as a surrogate because it is a history
  branch, not a spawned agent descendant.

The root soft-reload mechanism passes the implementation gate. The production
implementation includes pre-mutation descendant enumeration and integration
coverage for subtree restore, notification suppression, and crash repair.
