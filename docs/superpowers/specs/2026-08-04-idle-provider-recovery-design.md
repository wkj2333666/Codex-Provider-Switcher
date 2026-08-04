# Idle Provider Recovery Design

## Summary

Codex app-server can retain an idle thread under its previous model provider
after every switcher-managed subscriber has released the thread. A task-level
provider selection then diverges from the loaded runtime: the switcher detects
the mismatch before `turn/start`, but the existing recovery path accepts only
`systemError`. The original model turn is correctly withheld, while the task
remains dirty and every later submission repeats the same failure.

The supported deployment routes every app-server subscriber through the
switcher. Codex Desktop Remote SSH and android-ssh-codex both invoke the
unqualified `codex app-server proxy` command through the account login shell;
the installed shell path resolves that command to the switcher. Direct socket
subscribers and explicit invocation of the real Codex binary remain outside
the supported boundary.

This change generalizes the existing verified soft reload from terminal
`systemError` to quiescent provider mismatches whose fresh app-server status is
exactly `idle`. It preserves the existing thread id, history, spawned subtree,
subscriptions, crash journal, and zero-model-turn guarantee.

## Goals

- Recover a selected provider when a loaded thread is exactly `idle` and the
  verified resume result still reports the previous provider.
- Keep the original thread id, persisted history, descendants, and Desktop
  subscriptions.
- Never forward or replay the user's model turn until provider verification
  and peer restoration both succeed.
- Keep active and unknown states fail closed.
- Record a bounded, sanitized handoff stage in the existing dirty marker so a
  future failure can be located without prompts, paths, credentials, or raw
  provider responses.
- Repair legacy empty dirty markers through the same full handoff transaction.

## Non-Goals

- Supporting a client that bypasses the wrapper and subscribes directly to the
  app-server socket.
- Restarting or patching Codex app-server.
- Interrupting an active model turn.
- Replaying a failed submission.
- Changing android-ssh-codex or adding profile environment variables.

## Recovery Eligibility

The existing per-thread lock and peer protocol remain authoritative. Recovery
is considered only after:

1. every known peer reports the thread ready;
2. every known peer supports the handoff and recovery protocols;
3. the dirty marker is present;
4. every known peer acknowledges unsubscribe;
5. an internal resume returns the requested thread id but a different provider;
6. a fresh direct `thread/read` reports exactly `idle` or `systemError`.

Any other resume error, malformed response, active state, missing status, or
unknown status fails closed without archive. The supported deployment assumes
there are no non-switcher subscribers. A bypassing subscriber may observe the
archive lifecycle or be disconnected and is unsupported.

## Recovery Transaction

Both eligible statuses use the existing transaction:

```text
lock thread
  -> prepare all peers
  -> mark dirty at stage prepared
  -> unsubscribe all peers
  -> mark stage unsubscribed
  -> resume with selected provider and verify
  -> on same-id provider mismatch:
       read exact status
       require idle or systemError
       enumerate bounded spawned subtree
       persist recovery journal
       suppress matching lifecycle notifications on every peer
       archive root subtree
       unarchive descendants and root
       resume root with selected provider and verify
       end notification suppression
       clear recovery journal
  -> resubscribe detached peers with verified provider
  -> clear dirty marker
  -> forward the original turn unchanged
```

The journal remains the crash-repair authority once archive mutation is
possible. The dirty marker is operational evidence before that boundary.

## Dirty Stage Diagnostics

The current zero-length dirty marker becomes a tiny versioned JSON record with
only a schema version and enumerated stage. Existing switcher versions check
only file existence, so the format is backward compatible. Legacy empty files
decode as stage `unknown` and still force a complete handoff.

Allowed stages are bounded constants such as `prepared`, `unsubscribed`,
`resumeMismatch`, `recovering`, and `resubscribing`. Updates use a private
temporary file, file sync, atomic rename, and directory sync. Stage-write
failure is itself fail closed. No thread id is stored in the contents because
the filename already contains its digest.

## Probe Gate

Before deployment, an isolated disposable stock Codex app-server probe must
show that archive/unarchive of an idle thread:

- keeps the same thread id;
- preserves the exact history digest and turn count;
- permits a following resume with the alternate provider;
- emits no `turn/started` event and sends no model request.

The probe uses a temporary `CODEX_HOME`, local non-network providers, and no
user task. Production deployment stops if any assertion fails.

### Probe Result

Probe date: 2026-08-04. Installed Codex: `codex-cli 0.146.0`.

The initial no-turn fixture was rejected by stock app-server with `-32600 no
rollout found`: `thread/start` alone does not persist an archiveable rollout.
The final disposable fixture started one turn against a local blocking HTTP
endpoint, interrupted it through `turn/interrupt`, and required a fresh
`thread/read` status of exactly `idle` before recovery. This created one
persisted turn without contacting a real provider. Counters were snapshotted
after setup so the zero-turn and zero-provider-request assertions cover only
archive, unarchive, and alternate-provider resume.

Two fresh temporary homes passed independently:

- Exact pre-recovery `idle` status: pass in both runs.
- Root restored with the same thread id: pass in both runs.
- Alternate provider returned after restore: pass in both runs.
- History SHA-256 digest unchanged: pass in both runs.
- Turn count unchanged at one: pass in both runs.
- Additional `turn/started` notifications during recovery: pass; zero in both
  runs.
- Additional provider HTTP requests during recovery: pass; zero in both runs.

The real app-server gate therefore supports the same-id, zero-model-turn idle
soft reload used by this design.

Repository verification on the same host:

- `go test ./... -count=1`: pass across all packages.
- `go vet ./...`: pass.
- `go build ./cmd/codex-provider-switcher`: pass.
- `go test -race ./... -count=1`: not executed by the test binaries because
  the host exposes a 47-bit VMA while this Go ThreadSanitizer runtime requires
  48 bits (`FATAL: Found 47 - Supported 48`). No race-pass claim is made.

## Testing

- Unit tests for versioned dirty-stage persistence, legacy empty markers,
  permissions, atomic replacement, and invalid stage rejection.
- Recovery-client tests accepting exact `idle` and `systemError`, while
  rejecting active, unknown, and malformed statuses.
- Integration coverage for a sticky idle runtime that refuses provider change
  until archive/unarchive, including multiple cooperating proxy connections.
- Assertions for unchanged id and history, no leaked lifecycle notifications,
  no additional model turn, and original `turn/start` forwarding only after
  verified resubscription.
- Failure injection at stage writes, archive, unarchive, final resume, and
  resubscribe boundaries.
- Existing `systemError`, active-turn, mixed-version, malformed-protocol, and
  ordinary same-provider tests remain unchanged.

## Deployment

Build and install the switcher atomically without retaining an old backup.
Reconnect all live app-server proxy clients so they run the same binary. The
existing failed task keeps its legacy dirty marker; its next provider control
or model submission performs the upgraded full recovery and clears the marker
only after success.
