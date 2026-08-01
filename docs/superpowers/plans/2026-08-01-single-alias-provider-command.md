# Single-Alias Provider Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a discoverable `/provider` skill command that switches the current task's provider without invoking a model and persists that choice across reconnects.

**Architecture:** Parse a strict control message at the WebSocket session boundary, resolve per-task provider state through an atomic file store, reuse the existing coordinated handoff, and synthesize Codex app-server v2 turn events downstream. Package a skills-only plugin for command discovery while retaining exact plain-text command compatibility.

**Tech Stack:** Go 1.24, coder/websocket, JSON-RPC v2, Unix file permissions and atomic rename, Codex app-server protocol v2, Codex skills/plugin manifests.

## Global Constraints

- Codex CLI/app-server minimum supported version remains `0.146.0`.
- Provider identifiers accept only ASCII letters, digits, dot, underscore, and hyphen.
- Control turns never reach app-server, consume model tokens, or enter rollout history.
- Existing fail-closed handoff, peer restoration, dirty markers, and cross-process locks remain authoritative.
- Linux amd64/arm64 and macOS amd64/arm64 builds remain dependency-free beyond current Go modules.

---

### Task 1: Provider Names and Persistent Selection Store

**Files:**
- Create: `internal/provider/provider.go`
- Create: `internal/selection/store.go`
- Create: `internal/selection/store_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `provider.Valid(string) bool`.
- Produces: `selection.Open(string) (*Store, error)`, `(*Store).Get(string) (string, bool, error)`, and `(*Store).Set(string, string) error`.
- Produces: `config.Config.StateDir string` resolved from `--state-dir`, `CODEX_PROVIDER_SWITCHER_STATE_DIR`, `CODEX_HOME`, or the socket path.

- [ ] **Step 1: Write failing config and selection tests**

Cover explicit state precedence, stock socket inference, custom socket hashing,
provider validation reuse, missing selection fallback signaling, atomic
round-trip, mode `0700`/`0600`, corrupt state rejection, and hashed filenames.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test ./internal/config ./internal/selection ./internal/provider -count=1`

Expected: FAIL because the provider and selection packages and `StateDir` do not exist.

- [ ] **Step 3: Implement minimal validated state storage**

Use SHA-256 task filenames, bounded reads, `os.CreateTemp`, `Chmod(0600)`,
`Sync`, `Close`, and `Rename`. Return static errors that contain neither task
IDs nor stored data.

- [ ] **Step 4: Run tests to verify GREEN**

Run: `go test ./internal/config ./internal/selection ./internal/provider -count=1`

Expected: PASS.

### Task 2: Strict Command Parser and Synthetic Protocol Encoder

**Files:**
- Create: `internal/transport/provider_command.go`
- Create: `internal/transport/provider_command_test.go`
- Modify: `internal/transport/jsonrpc.go`

**Interfaces:**
- Produces: `parseProviderCommand(rpcMessage) (provider string, recognized bool, err error)`.
- Produces: `encodeProviderSwitchTurn(id json.RawMessage, threadID, provider string, now time.Time) ([][]byte, error)`.

- [ ] **Step 1: Write failing parser tests**

Accept an exact `$provider sub2api` plus provider skill item and exact plain-text
`/provider openai`. Reject missing names, unsafe names, extra prose,
attachments, duplicate text, and mismatched skills. Forward ordinary mentions.

- [ ] **Step 2: Run parser tests to verify RED**

Run: `go test ./internal/transport -run 'TestParseProviderCommand' -count=1 -v`

Expected: FAIL because the parser does not exist.

- [ ] **Step 3: Implement the structured parser**

Decode `params.input` as JSON objects. Require a single command text item and,
for `$provider`, exactly one skill item named `provider`. Use `provider.Valid`
for the argument and never include rejected input in errors.

- [ ] **Step 4: Write failing encoder test against literal v2 fields**

Assert the response and six notification methods, matching IDs, `itemsView`,
timestamps, final-answer phase, null fields, completed status, and final summary
agent item using independently written literal expectations.

- [ ] **Step 5: Run encoder test to verify RED**

Run: `go test ./internal/transport -run 'TestEncodeProviderSwitchTurn' -count=1 -v`

Expected: FAIL because the encoder does not exist.

- [ ] **Step 6: Implement and verify the encoder**

Generate random opaque turn/item IDs with `crypto/rand`, marshal typed local
wire structs, and return one response followed by ordered notifications.

Run: `go test ./internal/transport -run 'Test(ParseProviderCommand|EncodeProviderSwitchTurn)' -count=1 -v`

Expected: PASS.

### Task 3: Session-Level Dynamic Routing

**Files:**
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/proxy.go`

**Interfaces:**
- Consumes: `selection.Store`, provider command parser, and synthetic encoder.
- Changes: `handoff(context.Context, string, string) error` accepts an explicit target provider.
- Changes: `newSessionState` receives a selection store interface.

- [ ] **Step 1: Write failing control-turn session tests**

Prove successful commands hand off, persist, emit the complete fake lifecycle,
and perform zero upstream `turn/start` writes. Prove malformed commands and
handoff/store failures emit no success lifecycle.

- [ ] **Step 2: Run session tests to verify RED**

Run: `go test ./internal/transport -run 'TestSessionProviderCommand' -count=1 -v`

Expected: FAIL because `turn/start` is always forwarded.

- [ ] **Step 3: Implement command handling and explicit handoff targets**

Resolve a recognized command before the normal send, retain the existing lock
and peer-preparation ordering, hand off to the requested provider, persist it,
then write synthetic messages downstream without setting active-turn state.

- [ ] **Step 4: Write failing persisted-route tests**

Assert a normal `turn/start` hands off to the stored provider and a
`thread/resume` is rewritten with that provider rather than the connection
default. Assert store read failure rejects both paths.

- [ ] **Step 5: Implement persisted routing and verify GREEN**

Open one store per proxy bridge. Resolve the selected provider while holding
the task lock. Rewrite `thread/resume` only after selection resolution.

Run: `go test ./internal/transport -run 'TestSession(ProviderCommand|UsesStoredProvider|ResumeUsesStoredProvider)' -count=1 -v`

Expected: PASS.

### Task 4: End-to-End WebSocket Control Turn

**Files:**
- Modify: `internal/transport/handoff_integration_test.go`

**Interfaces:**
- Consumes: the public WebSocket proxy path and fake app-server fixture.

- [ ] **Step 1: Write a failing skill-shaped integration test**

Resume a test task, send `$provider sub2api` with its `skill` input item, read the
synthetic response through `turn/completed`, and assert the fake app-server turn
counter remains zero. Send an ordinary next turn and assert it runs through
`sub2api`.

- [ ] **Step 2: Run the integration test to verify RED**

Run: `go test ./internal/transport -run 'TestProviderSkillCommandSwitchesWithoutModelTurn' -count=1 -v`

Expected: FAIL before session routing is complete, then PASS after Task 3.

- [ ] **Step 3: Run repeated transport regression**

Run: `go test ./internal/transport -run 'Test(ProviderSkillCommand|SessionProviderCommand|SendTimeProviderHandoff)' -count=20`

Expected: PASS without hangs or leaked turns.

### Task 5: Skill Plugin, Release Packaging, and Documentation

**Files:**
- Create: `plugins/codex-provider-switcher/.codex-plugin/plugin.json`
- Create: `plugins/codex-provider-switcher/skills/provider/SKILL.md`
- Create: `plugins/codex-provider-switcher/skills/provider/agents/openai.yaml`
- Modify: `.github/workflows/release.yml`
- Modify: `internal/repository/workflows_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`

**Interfaces:**
- Produces: a skills-only plugin with explicit-only `provider` invocation.
- Produces: release archives containing `plugins/codex-provider-switcher`.

- [ ] **Step 1: Scaffold and validate the skills-only plugin**

Use the plugin creator scaffold, set the release version and repository
metadata, add the explicit-only skill, then run both plugin validators.

- [ ] **Step 2: Write failing repository packaging checks**

Require the release workflow to copy the plugin directory and validate both its
manifest and provider skill before packaging.

- [ ] **Step 3: Run repository tests to verify RED**

Run: `go test ./internal/repository -count=1 -v`

Expected: FAIL until the workflow and plugin are present.

- [ ] **Step 4: Update packaging and operator documentation**

Document one SSH alias, default provider configuration, remote skill install,
`/provider openai`, `/provider sub2api`, persistence path, fake-turn behavior,
and the disappearing-on-reopen feedback message.

- [ ] **Step 5: Validate plugin and repository checks**

Run:

```text
python3 /home/wkj/.codex/skills/.system/plugin-creator/scripts/validate_plugin.py plugins/codex-provider-switcher
python3 /home/wkj/.codex/skills/.system/skill-creator/scripts/quick_validate.py plugins/codex-provider-switcher/skills/provider
go test ./internal/repository -count=1
```

Expected: PASS.

### Task 6: Full Verification, Release, and Deployment

**Files:**
- Modify: plugin manifest version only if the release version changes.

**Interfaces:**
- Produces: a tagged GitHub release and installed binary/skill on the remote host.

- [ ] **Step 1: Run fresh local verification**

Run `gofmt`, `go test ./... -count=1`, `go vet ./...`, and CGO-disabled builds
for linux/amd64, linux/arm64, darwin/amd64, and darwin/arm64.

- [ ] **Step 2: Commit, push, and merge**

Commit the scoped changes, push `codex/slash-provider-control`, create a ready
PR, wait for all six checks, and merge without rewriting unrelated history.

- [ ] **Step 3: Tag and verify release**

Create the next semantic tag, wait for the Release workflow, verify all eight
binary assets plus plugin contents and checksums.

- [ ] **Step 4: Deploy without restarting the daemon**

Install the matching linux-arm64 binary over the existing wrapper target and
install the provider skill under `$HOME/.agents/skills/provider`. Confirm daemon
PID/start time remain unchanged, `codex --version` delegates to 0.146.0, and a
new proxy connection completes WebSocket upgrade.

- [ ] **Step 5: Report the remaining Desktop UI acceptance**

Do not claim laptop UI acceptance until the single `pi` alias is reconnected and
the user has observed both provider directions in Desktop.
