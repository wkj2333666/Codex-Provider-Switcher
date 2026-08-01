# App-Server Default Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop specifying a provider for unsaved tasks and defer to app-server's effective configuration.

**Architecture:** An empty connection provider represents no override. Thread responses supply the effective provider used by send-time coordination; saved task selections and slash controls remain explicit overrides.

**Tech Stack:** Go 1.24, coder/websocket, GitHub Actions.

## Global Constraints

- Do not read or parse Codex `config.toml`.
- Do not define a switcher-side default provider.
- Existing saved task selections retain precedence.
- Existing daemon and proxy processes are not restarted during deployment.

---

### Task 1: Optional Provider Override

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/rewrite/rewrite.go`
- Modify: `internal/rewrite/rewrite_test.go`

- [x] **Step 1: Write RED tests** for ignored legacy environment and byte-for-byte provider methods with no override.
- [x] **Step 2: Verify RED** in config and rewrite packages.
- [x] **Step 3: Allow an empty provider and skip provider rewrites.**
- [x] **Step 4: Verify focused GREEN tests.**

### Task 2: Effective Provider Coordination

**Files:**
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`

- [x] **Step 1: Write RED tests** for empty session provider, default resume, effective response tracking, and ordinary send without handoff.
- [x] **Step 2: Verify RED** in the transport package.
- [x] **Step 3: Use the observed effective provider when no explicit selection exists.**
- [x] **Step 4: Verify focused GREEN tests.**

### Task 3: Public Contract And Release

**Files:**
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`
- Modify: `internal/repository/workflows_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `plugins/codex-provider-switcher/.codex-plugin/plugin.json`

- [x] **Step 1: Write RED tests** removing the legacy variable from help and docs.
- [x] **Step 2: Update help, docs, architecture, and plugin version.**
- [x] **Step 3: Run focused and full verification.**
- [ ] **Step 4: Commit, push, merge, release, and deploy without restarting existing processes.**
