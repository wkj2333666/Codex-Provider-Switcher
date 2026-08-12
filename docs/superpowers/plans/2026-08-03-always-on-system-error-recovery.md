# Always-On SystemError Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make verified `systemError` provider recovery available in every switcher proxy without an environment variable or command-line activation gate.

**Architecture:** Keep the existing verified recovery transaction, persistent journal, peer notification isolation, and fail-closed boundaries unchanged. Remove only the configuration gate: every provider handoff checks peer recovery capability before mutation, and every same-thread provider mismatch is inspected for exact `systemError` eligibility.

**Tech Stack:** Go 1.23, JSON-RPC v2, Unix sockets, `github.com/coder/websocket`, table-driven and multi-connection integration tests.

## Global Constraints

- Do not add a replacement enable or disable environment variable.
- Do not add a recovery command-line flag.
- Do not modify, patch, replace, restart, or supervise Codex.
- Recovery remains eligible only after a same-thread provider mismatch and a fresh exact `systemError` status.
- Never replay the failed message, synthesize `turn/start`, change the root thread id, or alter persisted history.
- Preserve crash-journal repair, descendant restoration, notification suppression, and existing fail-closed behavior.
- A direct app-server subscriber that bypasses the switcher is outside the supported provider handoff architecture.

---

### Task 1: Enable Recovery In Every Proxy Session

**Files:**
- Modify: `internal/transport/handoff_integration_test.go`
- Modify: `internal/transport/proxy.go`
- Modify: `internal/transport/session.go`

**Interfaces:**
- Consumes: existing `recoveryJournals`, `handoffCoordinator.PrepareRecoveryAll`, and `recoverSystemError` transaction.
- Produces: `bridge(ctx, downstream, upstream, provider, socket, selections, recoveries)` with no recovery boolean; `session.handoff` always performs the pre-mutation recovery capability check.

- [ ] **Step 1: Make the successful recovery integration test use default configuration**

Replace the recovery-only helper call in `TestRunRecoversSystemErrorProviderWithoutModelTurn`:

```go
connection, done := dialProviderProxy(t, ctx, server.socket, "openai")
```

Replace the remaining `dialProviderProxyWithRecovery` call with `dialProviderProxy`, then delete `dialProviderProxyWithRecovery`. The existing assertions must continue to require the same root id and history, restored descendants, no downstream archive lifecycle, the selected provider, and zero additional model turns.

- [ ] **Step 2: Run the integration tests to verify RED**

Run:

```bash
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./internal/transport -run 'TestRunRecoversSystemErrorProviderWithoutModelTurn|TestRunDoesNotRecoverIdleMismatchWithNonCooperatingSubscriber'
```

Expected: `TestRunRecoversSystemErrorProviderWithoutModelTurn` fails with the existing handoff error because the default `config.Config` leaves recovery disabled. The idle mismatch regression must still fail closed rather than archive.

- [ ] **Step 3: Remove the session gate without changing recovery eligibility**

Change `bridge` to remove the boolean parameter and assignment:

```diff
@@
     provider, socket string,
-    exclusiveRecovery bool,
     selections providerSelections,
     recoveries recoveryJournals,
@@
     current.selections = selections
-    current.exclusiveRecovery = exclusiveRecovery
     current.recoveries = recoveries
```
```

Remove `exclusiveRecovery bool` from `session`. In `handoff`, always check peer support before creating the dirty marker:

```go
if err := current.coordinator.PrepareHandoffAll(ctx, threadID); err != nil {
    return errors.New("provider handoff capability check failed")
}
if err := current.coordinator.PrepareRecoveryAll(ctx, threadID); err != nil {
    return errors.New("provider recovery capability check failed")
}
```

Keep recovery limited to the existing classified provider mismatch:

```go
if err := current.internalResume(ctx, threadID, targetProvider); err != nil {
    if !errors.Is(err, errProviderMismatch) || current.recoveries == nil {
        current.restoreAfterHandoffFailure(threadID)
        return err
    }
    if err := current.recoverSystemError(ctx, threadID, targetProvider); err != nil {
        current.restoreAfterHandoffFailure(threadID)
        return err
    }
}
```

In `serveConnection`, pass only `selections` and `recoveryStore` after the socket.

- [ ] **Step 4: Run transport tests to verify GREEN**

Run:

```bash
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./internal/transport
```

Expected: PASS, including successful default recovery, idle mismatch rejection, non-cooperating subscriber rejection, journal repair, and zero-turn assertions.

- [ ] **Step 5: Commit the always-on session behavior**

```bash
git add internal/transport/handoff_integration_test.go internal/transport/proxy.go internal/transport/session.go
git commit -m "fix: enable system error recovery by default"
```

### Task 2: Remove The Recovery Configuration Surface

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`

**Interfaces:**
- Consumes: `config.ParseProxy(args, getenv)` and the existing transparent wrapper classification.
- Produces: `config.Config` containing only `Provider`, `Socket`, and `StateDir`; no recovery environment name, field, flag, or help entry.

- [ ] **Step 1: Write failing tests for the removed environment surface**

Replace `TestParseProxyEnablesOnlyExplicitExclusiveRecovery` with:

```go
func TestParseProxyIgnoresRemovedRecoveryEnvironment(t *testing.T) {
    socket := unixSocket(t, "app-server.sock")
    result, err := ParseProxy([]string{"--socket", socket}, environment(map[string]string{
        "CODEX_PROVIDER_SWITCHER_RECOVERY": "exclusive",
    }))
    if err != nil {
        t.Fatal(err)
    }
    want := Config{Socket: socket, StateDir: customStateDir(socket)}
    if result.Config != want {
        t.Fatalf("Config = %#v, want %#v", result.Config, want)
    }
}
```

Add a help regression in `main_test.go`:

```go
func TestUsageDoesNotAdvertiseRemovedRecoveryEnvironment(t *testing.T) {
    var stdout bytes.Buffer
    code := run(context.Background(), "codex-provider-switcher", nil, dependencies{stdout: &stdout})
    if code != 0 {
        t.Fatalf("run(help) = %d", code)
    }
    if strings.Contains(stdout.String(), "CODEX_PROVIDER_SWITCHER_RECOVERY") {
        t.Fatalf("usage advertises removed recovery environment: %q", stdout.String())
    }
}
```

Update `TestRunWrapperInterceptsAppServerProxy` to omit the recovery environment and assert only the provider and socket fields.

- [ ] **Step 2: Run focused tests to verify RED**

Run:

```bash
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./internal/config ./cmd/codex-provider-switcher
```

Expected: the legacy environment changes the current `ExclusiveRecovery` field and the usage test finds the old environment entry.

- [ ] **Step 3: Delete the configuration gate**

Remove:

```go
const recoveryEnvironment = "CODEX_PROVIDER_SWITCHER_RECOVERY"
```

Reduce the configuration type to:

```go
type Config struct {
    Provider string
    Socket   string
    StateDir string
}
```

Remove the `ExclusiveRecovery` initializer from `ParseProxy`, and remove `CODEX_PROVIDER_SWITCHER_RECOVERY=exclusive` from the CLI usage text. Do not filter this variable from delegated native Codex commands; delegation must continue to preserve the caller's original environment byte-for-byte.

- [ ] **Step 4: Format and run focused tests to verify GREEN**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go cmd/codex-provider-switcher/main.go cmd/codex-provider-switcher/main_test.go
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./internal/config ./cmd/codex-provider-switcher
```

Expected: PASS.

- [ ] **Step 5: Commit the configuration removal**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/codex-provider-switcher/main.go cmd/codex-provider-switcher/main_test.go
git commit -m "refactor: remove recovery activation setting"
```

### Task 3: Align User Documentation And Run The Release Gate

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/architecture.md`
- Test: `internal/repository/workflows_test.go`

**Interfaces:**
- Produces: deployment instructions with no recovery export or uninstall cleanup; architecture documentation describing automatic exact-`systemError` recovery and the supported wrapper boundary.

- [ ] **Step 1: Update both user guides**

Delete the recovery export blocks and all instructions to check or unset the old variable. Replace them with concise statements that recovery is automatic after a verified terminal `systemError`, preserves task id and history, and never resends the failed prompt. State that direct app-server subscribers which bypass the switcher are unsupported.

- [ ] **Step 2: Update architecture documentation**

Replace the activation-gate paragraph with the supported connection boundary from the approved spec. Change the handoff flow from "explicit exclusive mode only" to "verified systemError mismatch only", and state that every new peer must support recovery coordination before mutation.

- [ ] **Step 3: Run documentation and repository behavior checks**

Run:

```bash
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./internal/repository
rg -n 'CODEX_PROVIDER_SWITCHER_RECOVERY|exclusive recovery setting|exclusive mode' README.md README.zh-CN.md docs/architecture.md
git diff --check
```

Expected: repository tests PASS, `rg` returns no matches, and `git diff --check` exits zero.

- [ ] **Step 4: Run the complete quality gate**

Run serially to keep `/tmp` usage bounded:

```bash
env GOCACHE=/tmp/cps-always-on-go-build go test -count=1 ./...
env GOCACHE=/tmp/cps-always-on-go-build go vet ./...
env GOCACHE=/tmp/cps-always-on-go-build go build -trimpath ./cmd/codex-provider-switcher
git diff --check
```

Run `go test -race -count=1 ./...` only where the host ThreadSanitizer supports its VMA layout. If this host repeats `ThreadSanitizer: unsupported VMA range`, report that host limitation without treating it as a detected race.

- [ ] **Step 5: Commit the documentation update**

```bash
git add README.md README.zh-CN.md docs/architecture.md
git commit -m "docs: document automatic provider recovery"
```

- [ ] **Step 6: Verify the final branch state**

Run:

```bash
git status -sb
git log --oneline --decorate -6
```

Expected: a clean `system-error-provider-recovery` worktree containing the approved design commit, this plan, and the three implementation commits.
