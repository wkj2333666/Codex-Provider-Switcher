# SystemError Recovery Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove or reject stock Codex app-server archive/unarchive as a zero-model soft unload for a disposable thread in `systemError`.

**Architecture:** A throwaway Go program starts the installed real Codex 0.146.0 app-server with an isolated temporary `CODEX_HOME` and Unix socket. Its custom `probe` provider targets an unbound loopback port with retries disabled, producing a deterministic terminal provider failure without contacting a model. Two WebSocket clients exercise and observe the complete thread lifecycle.

**Tech Stack:** Go 1.23, `github.com/coder/websocket`, Codex app-server JSON-RPC v2, temporary filesystem state.

## Global Constraints

- Do not read or copy the user's current `config.toml`, credentials, SQLite database, or rollout files.
- Do not connect to a real provider or send a real model request.
- Do not delete or archive an existing user thread.
- Use the installed real Codex executable resolved by the wrapper environment.
- Keep the probe outside the repository and delete it after recording results.
- Stop before production implementation if any acceptance assertion fails.

---

### Task 1: Disposable Stock App-Server Harness

**Files:**
- Create temporarily: `/tmp/cps-system-error-recovery-probe.go`
- Create at runtime: `/tmp/cps-system-error-probe-*/config.toml`
- Modify: none

**Interfaces:**
- Consumes: `CODEX_PROVIDER_SWITCHER_CODEX`, or the wrapper-resolved real Codex path when the environment variable is absent.
- Produces: a running `codex app-server --listen unix://PATH`, two initialized WebSocket connections, and a cleanup function.

- [ ] **Step 1: Write the probe harness**

The program must create a private temporary home and write this literal configuration without copying user configuration:

```toml
model = "probe-model"
model_provider = "probe"

[model_providers.probe]
name = "SystemError Probe"
base_url = "http://127.0.0.1:1/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 1000

[model_providers.probe-alt]
name = "SystemError Probe Alternate"
base_url = "http://127.0.0.1:1/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 1000
```

Start the process with an explicit environment and endpoint:

```go
command := exec.Command(realCodex, "app-server", "--listen", "unix://"+socketPath)
command.Env = append(filteredEnvironment(os.Environ()), "CODEX_HOME="+codexHome)
```

`filteredEnvironment` must remove any existing `CODEX_HOME` and
`CODEX_PROVIDER_SWITCHER_CODEX`. Poll for the Unix socket with a deadline and
fail with captured stderr if the child exits first.

- [ ] **Step 2: Initialize two real protocol clients**

Each client sends and verifies:

```json
{"method":"initialize","id":1,"params":{"clientInfo":{"name":"cps_recovery_probe","title":"CPS Recovery Probe","version":"1"}}}
{"method":"initialized","params":{}}
```

The second connection is observational: it verifies broadcast notifications
and must not subscribe to the disposable root before the explicit resume step.

- [ ] **Step 3: Run the harness and verify startup**

Run:

```bash
env GOCACHE=/tmp/cps-system-error-go-build go run /tmp/cps-system-error-recovery-probe.go
```

Expected at this checkpoint: both initialize requests succeed and the probe
prints only structured phase names, thread ids, statuses, provider ids, and
turn counts. It must never print environment values, prompts, config contents,
or app-server stderr on success.

### Task 2: Reproduce The Stuck Runtime

**Files:**
- Modify temporarily: `/tmp/cps-system-error-recovery-probe.go`

**Interfaces:**
- Consumes: initialized primary client from Task 1.
- Produces: a disposable thread id whose observed status is `systemError`, a literal pre-recovery history fingerprint, and a demonstrated provider mismatch on normal resume.

- [ ] **Step 1: Start a disposable thread**

Send:

```json
{"method":"thread/start","id":2,"params":{"model":"probe-model","modelProvider":"probe","cwd":"/tmp"}}
```

Record the returned `thread.id` and assert `modelProvider == "probe"`.

- [ ] **Step 2: Trigger the local connection failure**

Send one disposable turn:

```json
{"method":"turn/start","id":3,"params":{"threadId":"THREAD_ID","input":[{"type":"text","text":"probe"}]}}
```

Wait for `turn/completed`, then `thread/status/changed` if it arrives after the
turn. Call `thread/read` with `includeTurns: true` and require
`thread.status.type == "systemError"`. Record a SHA-256 digest of the canonical
JSON `thread.turns` value and its array length.

- [ ] **Step 3: Characterize the current mismatch**

Unsubscribe the primary connection, then send:

```json
{"method":"thread/resume","id":5,"params":{"threadId":"THREAD_ID","modelProvider":"probe-alt","model":"probe-model"}}
```

Assert the response still reports `modelProvider == "probe"`; if it reports
`probe-alt`, the installed Codex already recovers the state and the production
workaround is unnecessary.

### Task 3: Root Archive/Unarchive Recovery Gate

**Files:**
- Modify temporarily: `/tmp/cps-system-error-recovery-probe.go`

**Interfaces:**
- Consumes: the stuck disposable thread and pre-recovery history digest from Task 2.
- Produces: pass/fail evidence for same-id reload, history preservation, broadcast lifecycle visibility, and zero extra model turns.

- [ ] **Step 1: Confirm the descendant boundary**

Use the generated 0.146.0 schema to confirm that `thread/fork` creates a
history branch (`forkedFromId`) rather than a spawned agent descendant
(`parentThreadId`). A true spawned descendant requires agent runtime behavior
and cannot be manufactured by this zero-model probe. Production planning must
therefore use an experimental control connection with `thread/list`
`ancestorThreadId` plus fake-server subtree tests; this probe does not claim to
validate descendant restoration.

- [ ] **Step 2: Archive the root and collect lifecycle notifications**

Send `thread/archive` for the root. Require a matching `thread/archived`
notification on both initialized clients, and call `thread/loaded/list` to
verify the root is no longer loaded.

- [ ] **Step 3: Restore every archived id**

Send `thread/unarchive` for the root and require the response and one
`thread/unarchived` notification to contain the original root id.

- [ ] **Step 4: Resume with the alternate provider**

Send root `thread/resume` with `modelProvider: "probe-alt"`. Assert:

```text
returned thread.id == original root id
returned modelProvider == probe-alt
returned thread.status.type != systemError
```

- [ ] **Step 5: Verify history and zero-model invariants**

Call `thread/read` with `includeTurns: true` and require the turns array length
and SHA-256 digest exactly match the pre-recovery values. Count every
`turn/started` notification and require it equals one, the intentionally failed
probe turn; archive, unarchive, and resume must add none.

### Task 4: Record The Gate Result

**Files:**
- Modify: `docs/superpowers/specs/2026-08-03-system-error-provider-recovery-design.md`
- Delete: `/tmp/cps-system-error-recovery-probe.go`

**Interfaces:**
- Consumes: structured probe output and app-server exit status.
- Produces: a documented implementation decision and no retained throwaway source.

- [ ] **Step 1: Run the complete probe twice**

Run the Task 1 command twice with fresh temporary homes. Both runs must produce
identical phase outcomes while thread ids may differ.

- [ ] **Step 2: Record exact acceptance results**

Add a dated `Probe Result` section to the design spec listing:

```text
Codex version
SystemError reproduced
Normal resume mismatch reproduced
Root archive notification observed on both clients
Root restored with the same id
Same root id resumed with probe-alt
History digest and turn count unchanged
No additional turn started
```

Each item must be `pass` or `fail`; no partial or assumed result is allowed.

- [ ] **Step 3: Remove the throwaway probe**

Delete `/tmp/cps-system-error-recovery-probe.go` only after the results are
recorded. The temporary homes are created with `os.MkdirTemp` and removed by
the probe cleanup path.

- [ ] **Step 4: Decide the next plan**

If every item passes, write a separate production implementation plan covering
status tracking, peer notification suppression, journaling, repair, and fake
app-server integration tests. If any item fails, stop production work and
revise the design around the observed failure.
