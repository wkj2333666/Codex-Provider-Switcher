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

## Activation Gate

The switcher records `thread/status/changed` notifications per thread. Soft
reload is eligible only when all of the following are true:

1. The requested provider differs from the verified effective provider.
2. Every cooperating peer reports the thread is not active.
3. The normal unsubscribe and `thread/resume` handoff returns the same thread
   id but a different provider.
4. At least one cooperating peer has observed the thread status as
   `systemError`.
5. The app-server supports every RPC used by a disposable compatibility probe.

Other resume failures, malformed responses, unknown status, and version drift
retain the existing static fail-closed error.

## Probe Gate

Before production code is enabled, an isolated probe against the installed
stock app-server must establish that:

- `thread/archive` unloads a thread in `systemError`;
- `thread/unarchive` restores the same thread id and persisted turns;
- a following `thread/resume` applies a different `modelProvider`;
- no `turn/start` or model request is required;
- archive notifications enumerate every active descendant moved with the root;
- each moved descendant can be restored;
- reconnecting and reading the thread shows unchanged history;
- internal archive notifications can be consumed without leaking to Desktop.

The probe uses a disposable thread and deletes no user thread. If any gate
cannot be demonstrated, production soft reload is not implemented and the
switcher keeps the current fail-closed behavior with a clearer diagnostic.

## Recovery Transaction

The existing per-thread lock remains the serialization boundary. A recovery
transaction performs:

```text
prepare all peers
  -> mark handoff dirty
  -> unsubscribe all peers
  -> normal resume and provider verification
  -> on verified SystemError mismatch only:
       begin notification suppression on all peers
       persist recovery journal
       read the root and active descendant set
       archive the root subtree
       record every archived thread id
       unarchive every recorded thread id
       resume the root with the target provider
       verify root id and provider
       end notification suppression
  -> resubscribe detached peers with verified provider
  -> persist provider selection
  -> clear recovery journal and handoff dirty marker
```

The first normal resume remains important: it preserves the fast path for
future Codex versions and distinguishes the known stuck-runtime case from
other compatibility failures.

## Recovery Journal

Recovery state lives in the existing private runtime namespace and uses a
per-thread file beside the handoff lock and dirty marker. It contains only:

- schema version;
- root thread id;
- requested provider;
- phase;
- thread ids confirmed archived but not yet confirmed unarchived.

Writes use a mode-0600 temporary file, file sync, atomic rename, and directory
sync. The journal never contains prompts, history, credentials, paths from
messages, or provider configuration.

Before accepting another provider command or model turn for a journaled
thread, the lock holder repairs it by unarchiving every recorded id, then
resumes the root without a provider override to discover the actual runtime.
It clears the journal only after the app-server confirms the root is available.
An uncertain repair remains dirty and fails closed.

## Notification Isolation

`thread/archive` and `thread/unarchive` broadcast lifecycle notifications to
all app-server connections. The coordinator therefore gains a recovery mode
for one thread. Every peer acknowledges that mode before archive mutation and
suppresses only matching internal notifications:

- `thread/archived` for ids recorded by the transaction;
- `thread/unarchived` for the same ids;
- `thread/closed` and `thread/status/changed` transitions caused by the
  temporary unload.

All unrelated notifications continue downstream byte-for-byte. Recovery mode
is removed after successful reload or best-effort repair.

## Descendant Threads

Codex archives the spawned subtree with a root. The switcher must not assume
that unarchiving the root restores descendants. The probe determines the
observable enumeration and ordering contract. Production recovery records and
restores every thread id reported as archived. If the complete moved set
cannot be established, recovery stops and leaves the journal for repair.

## Failure Semantics

- No provider selection is saved before final provider verification.
- No original `turn/start` is forwarded during recovery.
- A failed switch command returns a sanitized local error.
- Best-effort repair never hides uncertainty by clearing dirty state.
- Existing subscriptions are restored with the actual verified provider.
- The switcher never archives a thread unless all known peers are quiescent
  and participating in the recovery protocol.

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
- ordinary mismatch without `systemError` still failing closed;
- peer capability rejection before mutation;
- selection persistence only after verified recovery;
- existing idle handoff remaining unchanged.

The release gate also runs the disposable real app-server probe, `go test
./...`, `go test -race ./...` where supported, and `go vet ./...`.
