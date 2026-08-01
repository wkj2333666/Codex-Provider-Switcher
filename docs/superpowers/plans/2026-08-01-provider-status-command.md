# Provider Status And Explicit Switch Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only provider query and replace implicit provider switching with explicit status/switch commands.

**Architecture:** Parse strict provider actions at the WebSocket boundary, render runtime and persisted selection separately, reuse coordinated handoff only for switch, and synthesize both results locally. App-server remains authoritative for the runtime provider and default configuration.

**Tech Stack:** Go 1.24, coder/websocket, JSON-RPC v2, Codex app-server protocol v2.

## Global Constraints

- The only user-facing commands are `/provider status` and `/provider switch <name>`; `$provider` is accepted only as Desktop's skill-encoded internal form.
- Status never resumes, switches, or writes provider state.
- Switch success requires an app-server response matching the requested provider.
- Control turns never invoke a model or enter rollout history.
- Provider identifiers retain the existing ASCII grammar.
- Corrected release version is `0.5.1`.

---

### Task 1: Strict Action Parser And Generic Synthetic Turn

**Files:**
- Modify: `internal/transport/provider_command.go`
- Modify: `internal/transport/provider_command_test.go`

**Interfaces:**
- Produces: `providerCommand{action providerCommandAction, provider string}`.
- Produces: `encodeProviderControlTurn(id, threadID, commandText, feedback, now)`.

- [ ] Add failing parser tests for status/switch across skill, compact slash, and spaced slash forms; reject legacy and malformed forms.
- [ ] Run `go test ./internal/transport -run 'TestParseProviderCommand' -count=1` and confirm RED.
- [ ] Implement the minimal structured action parser and confirm GREEN.
- [ ] Add failing synthetic lifecycle tests for canonical command text and arbitrary sanitized feedback.
- [ ] Generalize the encoder and confirm parser/encoder tests are GREEN.

### Task 2: Read-Only Status And Explicit Switch Routing

**Files:**
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/handoff_integration_test.go`

**Interfaces:**
- Consumes: parsed provider actions and generic synthetic lifecycle.
- Produces: deterministic status feedback from effective runtime and durable selection.

- [ ] Add failing session tests for verified runtime, selection mismatch, unknown runtime, state read failure, and zero app-server writes.
- [ ] Run focused session tests and confirm RED.
- [ ] Implement status formatting and action-specific routing without handoff on status.
- [ ] Update switch tests to the explicit grammar and confirm GREEN.
- [ ] Add an end-to-end skill-shaped status then switch test proving both control turns cause zero model turns.
- [ ] Run the repeated transport regression 20 times.

### Task 3: Public Contract And Release Metadata

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `plugins/codex-provider-switcher/.codex-plugin/plugin.json`
- Modify: `plugins/codex-provider-switcher/skills/provider/SKILL.md`
- Modify: `plugins/codex-provider-switcher/skills/provider/agents/openai.yaml`
- Modify: `internal/repository/workflows_test.go`

**Interfaces:**
- Produces: discoverable explicit status/switch help and plugin version `0.5.1`.

- [ ] Add failing repository assertions for the new grammar, status copy, removed legacy examples, and version.
- [ ] Update plugin/skill/docs and confirm repository tests are GREEN.
- [ ] Run both official plugin and skill validators.

### Task 4: Verification, Review, Release, And Deployment

**Files:**
- No additional source files unless review finds a defect.

**Interfaces:**
- Produces: reviewed `v0.5.1` release and atomically installed wrapper/skill.

- [ ] Run gofmt, full tests, vet, diff checks, and four CGO-disabled platform builds.
- [ ] Review the complete diff for protocol accuracy, fail-closed behavior, secret handling, compatibility, and missing tests; fix findings inline.
- [ ] Push a ready PR, wait for all CI, merge, tag `v0.5.1`, and verify all eight release assets.
- [ ] Atomically deploy Linux ARM64 binary and provider skill without restarting existing daemons or proxies.
- [ ] Verify stock and Android-shaped WebSocket handshakes and report remaining UI acceptance only.
