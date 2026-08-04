# Idle Provider Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recover an exact idle app-server provider mismatch with the existing same-id, zero-model-turn soft reload while preserving fail-closed behavior and recording sanitized handoff stages.

**Architecture:** Extend the global dirty marker with a backward-compatible stage record, then generalize recovery eligibility from exact `systemError` to exact `idle` or `systemError`. Reuse the existing peer suppression, subtree journal, archive/unarchive, verified resume, and resubscription transaction without adding another daemon or changing android-ssh-codex.

**Tech Stack:** Go 1.23, `github.com/coder/websocket`, Unix sockets, Codex app-server JSON-RPC v2, atomic filesystem state.

## Global Constraints

- Every supported app-server subscriber must enter through the switcher wrapper.
- Direct app-server subscribers and explicit real-Codex proxy invocation remain unsupported.
- Never interrupt an active turn or accept an unknown thread status.
- Never replay or synthesize a model turn during provider recovery.
- Preserve thread ids, history, descendants, and cooperating peer subscriptions.
- Keep recovery automatic with no environment variable or command-line gate.
- Do not retain an old installed binary backup.

---

### Task 1: Versioned Dirty Stage Record

**Files:**
- Modify: `internal/handoff/coordinator.go`
- Modify: `internal/handoff/coordinator_test.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`

**Interfaces:**
- Consumes: the existing runtime directory and per-thread dirty-marker path.
- Produces: `type DirtyStage string`, bounded exported stage constants, and `SetDirtyStage(threadID string, stage DirtyStage) error` on `Coordinator` and `handoffCoordinator`.

- [ ] **Step 1: Write failing dirty-stage tests**

Add coordinator tests that call:

```go
if err := first.MarkDirty("thr-a"); err != nil { t.Fatal(err) }
if err := first.SetDirtyStage("thr-a", DirtyStageUnsubscribed); err != nil { t.Fatal(err) }
record := readDirtyRecord(t, first.threadStatePath("dirty", "thr-a", ".state"))
if record.Version != 1 || record.Stage != DirtyStageUnsubscribed { t.Fatalf("record = %#v", record) }
```

Also assert mode `0600`, visibility through a second coordinator, rejection of an invalid stage, and that a legacy empty marker remains dirty and can be replaced by a valid stage record.

- [ ] **Step 2: Run the focused test and verify failure**

Run `go test ./internal/handoff -run 'TestDirty' -count=1`.

Expected: compile failure because `DirtyStageUnsubscribed` and `SetDirtyStage` do not exist.

- [ ] **Step 3: Implement atomic stage persistence**

Define only these values:

```go
type DirtyStage string

const (
    DirtyStagePrepared       DirtyStage = "prepared"
    DirtyStageUnsubscribed   DirtyStage = "unsubscribed"
    DirtyStageResumeMismatch DirtyStage = "resumeMismatch"
    DirtyStageRecovering     DirtyStage = "recovering"
    DirtyStageResubscribing  DirtyStage = "resubscribing"
)

type dirtyRecord struct {
    Version int        `json:"version"`
    Stage   DirtyStage `json:"stage"`
}
```

Make `MarkDirty` call `SetDirtyStage(threadID, DirtyStagePrepared)`. Validate the exact stage set, marshal the bounded record, write a mode-0600 temporary file in the runtime directory, sync it, atomically rename it over the marker, and sync the directory. Do not read marker contents in `IsDirty`; empty legacy files keep their current meaning.

Extend `handoffCoordinator` and test fakes. In `session.handoff`, update stages immediately after all peers unsubscribe, after a verified provider mismatch, before archive recovery, and before peer resubscription. Any stage-write error restores peers best-effort and fails closed.

- [ ] **Step 4: Run focused tests and verify pass**

Run `go test ./internal/handoff ./internal/transport -run 'Dirty|Stage' -count=1`.

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/handoff/coordinator.go internal/handoff/coordinator_test.go internal/transport/session.go internal/transport/session_test.go
git commit -m "feat: record provider handoff stages"
```

### Task 2: Exact Idle Recovery Eligibility

**Files:**
- Modify: `internal/transport/recovery_client.go`
- Modify: `internal/transport/recovery_client_test.go`
- Modify: `internal/transport/session.go`

**Interfaces:**
- Consumes: `recoveryClient.readThread(ctx, rootID)` and existing bounded subtree enumeration.
- Produces: `inspectRecoverableSubtree(ctx context.Context, rootID string) ([]string, string, error)`, returning ordered ids and the exact accepted status.

- [ ] **Step 1: Write failing eligibility tests**

Refactor the recovery fixture to accept a root status and add:

```go
tests := []struct {
    status string
    wantOK bool
}{
    {status: "idle", wantOK: true},
    {status: "systemError", wantOK: true},
    {status: "active", wantOK: false},
    {status: "", wantOK: false},
    {status: "mystery", wantOK: false},
}
```

For accepted rows, assert the returned status and descendants-before-root order. For rejected rows, assert no `thread/list` follows `thread/read`.

- [ ] **Step 2: Run the focused test and verify failure**

Run `go test ./internal/transport -run 'RecoverableSubtree|RecoveryClient' -count=1`.

Expected: compile or assertion failure because only exact `systemError` is accepted.

- [ ] **Step 3: Implement the minimal classifier**

Replace `inspectSystemErrorSubtree` with:

```go
func (client *recoveryClient) inspectRecoverableSubtree(ctx context.Context, rootID string) ([]string, string, error) {
    thread, err := client.readThread(ctx, rootID)
    if err != nil || (thread.Status != "idle" && thread.Status != "systemError") {
        return nil, "", errors.New("recovery requires quiescent thread")
    }
    status := thread.Status
    // Keep the complete bounded descendant enumeration unchanged.
    return append(ids, rootID), status, nil
}
```

Rename `recoverSystemError` to `recoverProviderMismatch`, call the generalized inspector, set `DirtyStageRecovering` before saving the recovery journal, and leave the archive transaction unchanged.

- [ ] **Step 4: Run focused tests and verify pass**

Run `go test ./internal/transport -run 'RecoverableSubtree|RecoveryClient|SystemError' -count=1`.

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/transport/recovery_client.go internal/transport/recovery_client_test.go internal/transport/session.go
git commit -m "fix: recover exact idle provider mismatches"
```

### Task 3: Sticky Idle Multi-Peer Integration

**Files:**
- Modify: `internal/transport/handoff_integration_test.go`
- Modify: `docs/architecture.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Interfaces:**
- Consumes: the generalized recovery transaction from Task 2.
- Produces: fake app-server `stickyIdle bool` behavior and end-to-end regression coverage.

- [ ] **Step 1: Write the failing production regression**

Add `stickyIdle bool` and `softReloaded bool` to `handoffAppServer`. In `handleResume`, retain the old provider when `stickyIdle && !softReloaded`; set `softReloaded = true` only after the root is archived and unarchived.

Replace the old unsupported idle-mismatch rejection test with `TestRunRecoversIdleProviderMismatchWithoutModelTurn`. Use two switcher clients, history `idle-history-fixture`, and one descendant. Assert:

```go
if response.errorCode != 0 { t.Fatalf("idle recovery response = %#v", response) }
if provider != "sub2api" || archiveCalls != 1 { t.Fatalf("provider=%q archive=%d", provider, archiveCalls) }
if fmt.Sprint(history) != fmt.Sprint([]string{"idle-history-fixture"}) { t.Fatalf("history=%v", history) }
if calls := server.turnStartCallCount(); calls != 0 { t.Fatalf("turn calls=%d", calls) }
if subscribers := server.subscriberCount(); subscribers != 2 { t.Fatalf("subscribers=%d", subscribers) }
```

Read both clients through the provider-control lifecycle and assert neither sees archive, unarchive, close, or status-change notifications.

- [ ] **Step 2: Run the regression and verify behavior**

Run `go test ./internal/transport -run 'TestRunRecoversIdleProviderMismatchWithoutModelTurn' -count=1`.

Expected before Task 2: JSON-RPC `-32090` and zero archive calls. Expected after Task 2: PASS.

- [ ] **Step 3: Add failure-boundary assertions**

Require that active and unknown statuses never call archive, a failed resubscribe retains dirty stage `resubscribing`, and a successful retry clears the recovery journal and dirty marker.

- [ ] **Step 4: Update user and architecture documentation**

Document automatic exact `idle` and `systemError` recovery, the all-subscribers-through-wrapper boundary, unchanged id/history, no replay, and unsupported direct subscribers. Remove wording that idle mismatch always fails closed.

- [ ] **Step 5: Run transport and repository tests**

Run `go test ./internal/transport ./internal/handoff ./internal/repository -count=1`.

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/transport/handoff_integration_test.go docs/architecture.md README.md README.zh-CN.md
git commit -m "test: cover idle provider recovery"
```

### Task 4: Stock App-Server Gate, Full Verification, And Deployment

**Files:**
- Create temporarily: `/tmp/cps-idle-recovery-probe.go`
- Modify: `docs/superpowers/specs/2026-08-04-idle-provider-recovery-design.md`
- Delete: `/tmp/cps-idle-recovery-probe.go`

**Interfaces:**
- Consumes: installed real Codex 0.146.0 and a temporary isolated `CODEX_HOME`.
- Produces: idle soft-reload evidence and an atomically installed switcher binary.

- [ ] **Step 1: Build the disposable probe**

Create a temporary Go program that starts real `codex app-server --listen unix://...` with a private temporary `CODEX_HOME`. Configure two local providers with unbound loopback endpoints, initialize two protocol clients, and create an idle thread with `thread/start` but no `turn/start`.

Record the original id and SHA-256 of `thread.turns`. Archive and unarchive the root, resume with the alternate provider, and require:

```text
same thread id
alternate provider returned
exact history digest and turn count preserved
zero turn/started notifications
zero provider HTTP requests
```

- [ ] **Step 2: Run the probe twice and remove it**

Run `env GOCACHE=/tmp/cps-idle-go-build go run /tmp/cps-idle-recovery-probe.go` twice with fresh homes.

Expected: every assertion prints `pass`. Delete the temporary source after recording results.

- [ ] **Step 3: Record the exact probe result**

Append date, Codex version, and each pass/fail assertion to the design spec. Stop before deployment if any item fails.

- [ ] **Step 4: Run complete verification**

Run:

```bash
gofmt -w internal/handoff/coordinator.go internal/handoff/coordinator_test.go internal/transport/session.go internal/transport/session_test.go internal/transport/recovery_client.go internal/transport/recovery_client_test.go internal/transport/handoff_integration_test.go
go test ./...
go vet ./...
go build ./cmd/codex-provider-switcher
```

Run `go test -race ./...` only if host Go supports the required aarch64 VMA width; otherwise record the existing host TSAN limitation and do not claim a race pass.

- [ ] **Step 5: Commit final evidence**

```bash
git add docs/superpowers/specs/2026-08-04-idle-provider-recovery-design.md
git commit -m "docs: record idle recovery probe"
```

- [ ] **Step 6: Install without backup and reload proxies**

Build to a private staged file under `$HOME/.local/lib/codex-provider-switcher`, mode it `0755`, and atomically rename it over `codex-provider-switcher`. Do not create or retain an old binary backup. Reconnect every live Desktop and Android proxy so `/proc/<pid>/exe --version` reports the new commit.

- [ ] **Step 7: Verify the failed production task**

Use a provider control command on thread `019fbdf1-c6a3-7062-a95f-8ef05a5d45df` so no model turn is sent. Require verified runtime and selected provider `sub2api`, no recovery journal, and no dirty marker. Do not replay the earlier `2`; after repair, the user can submit it once.
