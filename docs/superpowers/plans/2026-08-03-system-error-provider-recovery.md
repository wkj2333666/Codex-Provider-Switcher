# SystemError Provider Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recover provider switching for a persisted Codex thread stuck in `systemError` without replaying input, starting a model turn, changing thread history, or modifying Codex.

**Architecture:** Keep normal unsubscribe/resume handoff as the first path. When and only when `CODEX_PROVIDER_SWITCHER_RECOVERY=exclusive`, a verified same-id provider mismatch is followed by an initialized internal app-server connection that confirms `systemError`, enumerates descendants, journals the transaction, archives and restores the subtree, and verifies the requested provider. Peer sessions suppress only the bounded archive lifecycle while existing subscription restoration remains unchanged.

**Tech Stack:** Go 1.23, `github.com/coder/websocket`, JSON-RPC v2, Unix sockets, atomic JSON journal files.

## Global Constraints

- Recovery is disabled unless `CODEX_PROVIDER_SWITCHER_RECOVERY=exclusive` exactly.
- Do not modify, patch, replace, restart, or supervise the Codex binary or app-server daemon.
- Do not replay a failed message or call `turn/start` during provider recovery.
- Preserve the root thread id, persisted history, and all cooperating subscriptions.
- Keep the existing non-cooperating-subscriber handoff test passing in default mode.
- Persist repair state under `Config.StateDir`, not `/tmp`.
- Cap the root plus descendants at 64 ids and fail before archive on malformed, duplicate, or truncated enumeration.

---

### Task 1: Explicit Recovery Configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`

**Interfaces:**
- Consumes: `CODEX_PROVIDER_SWITCHER_RECOVERY`.
- Produces: `config.Config.ExclusiveRecovery bool` passed unchanged in `transport.Options`.

- [ ] **Step 1: Write failing configuration tests**

Add table cases proving only the exact value `exclusive` enables recovery; unset and other values keep it false. Extend the wrapper proxy test to assert the flag reaches `transport.Options` and delegated commands are still executed with their original environment.

- [ ] **Step 2: Run the focused tests and verify RED**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/config ./cmd/codex-provider-switcher`. Expected: compile failure because `ExclusiveRecovery` does not exist.

- [ ] **Step 3: Add the minimal configuration field**

Parse the environment without adding a wrapper CLI grammar token:

```go
const recoveryEnvironment = "CODEX_PROVIDER_SWITCHER_RECOVERY"

type Config struct {
    Provider          string
    Socket            string
    StateDir          string
    ExclusiveRecovery bool
}
```

Set the field from `getenv(recoveryEnvironment) == "exclusive"`; all other values are false.

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run the same command and require PASS.

### Task 2: Bounded Internal Recovery Client

**Files:**
- Create: `internal/transport/recovery_client.go`
- Create: `internal/transport/recovery_client_test.go`

**Interfaces:**
- Consumes: app-server socket, root thread id, requested provider.
- Produces: `newRecoveryClient(context.Context, string)`, `inspectSubtree`, `archive`, `unarchive`, `readThread`, `resume`, and `close` operations with verified typed results.

- [ ] **Step 1: Write a failing real-WebSocket protocol test**

Use the existing Unix HTTP test server pattern. Require the client to initialize with `capabilities.experimentalApi=true`, page `thread/list` with `ancestorThreadId`, return a literal child-first/root-last set, reject duplicates and a 65th id before any archive request, and correlate responses while consuming unrelated notifications.

- [ ] **Step 2: Run the test and verify RED**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/transport -run 'TestRecoveryClient'`. Expected: compile failure because the client API is absent.

- [ ] **Step 3: Implement the minimal client**

Dial the configured Unix socket with compression disabled and the existing 64 MiB limit. Initialize as `codex_provider_switcher_recovery`, send `initialized`, use private monotonic ids, accept only matching JSON-RPC responses, and expose sanitized errors. `inspectSubtree` must require root `systemError`, enumerate all pages, validate non-empty unique ids, and return descendants before root.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run the Task 2 test command and require PASS.

### Task 3: Persistent Recovery Journal And Peer Isolation

**Files:**
- Create: `internal/recovery/store.go`
- Create: `internal/recovery/store_test.go`
- Modify: `internal/handoff/coordinator.go`
- Modify: `internal/handoff/coordinator_test.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`

**Interfaces:**
- Produces: `recovery.Open(stateDir)`, `Load(threadID)`, `Save(Journal)`, `Clear(threadID)`; coordinator methods `BeginRecoveryAll(ctx, threadID, ids)` and `EndRecoveryAll(ctx, threadID)`; session notification filter keyed by the exact recovery id set.

- [ ] **Step 1: Write failing journal tests**

Require mode-0700 directory creation, mode-0600 files, atomic replacement, bounded decoding, thread-id/hash isolation, round-trip of schema/root/provider/remaining ids, and corrupt journal fail-closed behavior.

- [ ] **Step 2: Verify journal RED, implement, and verify GREEN**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/recovery`, implement only the tested store, then rerun and require PASS.

- [ ] **Step 3: Write failing peer suppression tests**

Require every peer to acknowledge `beginRecoveryV1` before mutation, reject malformed or more than 64 ids, suppress matching `thread/archived`, `thread/unarchived`, `thread/closed`, and `thread/status/changed`, forward unrelated ids byte-for-byte, and remove suppression after `endRecoveryV1`.

- [ ] **Step 4: Verify peer RED, implement, and verify GREEN**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/handoff ./internal/transport -run 'Recovery|Suppress'`, add the bounded peer messages and session filter, then rerun and require PASS.

### Task 4: Transactional Handoff Recovery

**Files:**
- Modify: `internal/transport/proxy.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/handoff_integration_test.go`

**Interfaces:**
- Consumes: `Config.ExclusiveRecovery`, recovery client, journal store, coordinator suppression.
- Produces: normal handoff unchanged; exclusive `systemError` recovery with zero model turns; idempotent pre-command repair.

- [ ] **Step 1: Write the failing end-to-end recovery test**

Extend the fake app-server with literal history, `systemError`, descendants, archive/unarchive broadcasts, and request counters. The test must show a normal resume mismatch, then assert same root id/provider target/history, every descendant restored, zero `turn/start`, no archive lifecycle downstream, restored peer subscriptions, and selection persisted only after success.

- [ ] **Step 2: Run the integration test and verify RED**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/transport -run TestRunRecoversSystemErrorProviderWithoutModelTurn`. Expected: failure response `-32090` and no archive call.

- [ ] **Step 3: Implement the minimal transaction**

Classify normal resume verification into success, same-id provider mismatch, and all other errors. Only the mismatch in exclusive mode opens the control client. Confirm `systemError`, enumerate before mutation, begin peer suppression, save the full remaining-id journal, archive root, unarchive ids child-first/root-last while shrinking the journal, resume root with the target provider on the sender connection, verify id/provider, end suppression, then use the existing resubscribe and dirty-marker completion path.

- [ ] **Step 4: Add failing repair and failure-boundary tests**

Seed journals representing pre-archive, archived, partly restored, and fully restored crashes. Inject archive, unarchive, resume, peer-end, and journal-clear failures. Require a later command to repair first, never forward its turn while repair is uncertain, retain journal/dirty state on uncertainty, and clear both only after root availability is verified.

- [ ] **Step 5: Implement idempotent repair and verify GREEN**

Before ordinary routing after the thread lock, load a journal. Begin suppression for its ids and unarchive each remaining id. Accept an error only when it is stock code `-32600` with exact message `no archived rollout found for thread id <id>` and `thread/read` confirms that same id exists; `thread/read` alone is insufficient because stock Codex includes archived rollouts. Resume the root without a provider override, update effective provider, end suppression, clear the journal, and continue through normal handoff. Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/transport` and require PASS.

- [ ] **Step 6: Re-run the non-cooperating subscriber regression**

Run `env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./internal/transport -run TestRunRejectsHandoffWithNonCooperatingSubscriber`. Require PASS with default recovery disabled.

### Task 5: User Documentation And Release Verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/architecture.md`
- Modify: `internal/repository/workflows_test.go` only if the existing behavioral repository checks require new user-facing facts.

**Interfaces:**
- Produces: concise deployment guidance for the exclusive ownership contract and recovery behavior.

- [ ] **Step 1: Document the opt-in boundary**

Add the single environment export beside existing deployment exports, explain that it is appropriate only when all clients for these threads use the switcher, and state that recovery restores provider state but never resends the failed prompt.

- [ ] **Step 2: Run formatting and the complete quality gate**

Run `gofmt -w` on changed Go files, then:

```bash
env GOCACHE=/tmp/cps-system-error-go-build go test -count=1 ./...
env GOCACHE=/tmp/cps-system-error-go-build go test -race -count=1 ./...
env GOCACHE=/tmp/cps-system-error-go-build go vet ./...
git diff --check
```

All commands must pass before completion is claimed.
