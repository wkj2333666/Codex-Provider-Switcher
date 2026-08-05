# Collaboration Model Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent Codex collaboration-mode and settings updates from overriding a verified provider/model route.

**Architecture:** Extend the existing JSON rewrite boundary with one fail-closed model-field helper. Route saved-task `thread/settings/update` requests through the session's selection catalog while keeping unsaved tasks transparent.

**Tech Stack:** Go 1.24, JSON-RPC over WebSocket, `encoding/json`, existing transport and rewrite tests.

## Global Constraints

- Preserve provider control input and never replay user input.
- Keep tasks without saved provider selections transparent.
- Fail closed on malformed model-bearing request structures.
- Do not change Codex credentials, task storage, or Desktop model listing.
- Run `go clean -cache -testcache` after every Go build or test command.

---

### Task 1: Rewrite Every Authoritative Model Field

**Files:**
- Modify: `internal/rewrite/rewrite.go`
- Test: `internal/rewrite/rewrite_test.go`

**Interfaces:**
- Consumes: `modelroute.Route` and a JSON-RPC request payload.
- Produces: `rewrite.Line([]byte, modelroute.Route) ([]byte, error)` that updates top-level and collaboration-mode model fields.

- [ ] Add table tests for `turn/start`, `thread/start`, and `thread/settings/update` with stale `collaborationMode.settings.model` values.
- [ ] Add malformed and null collaboration-mode cases.
- [ ] Run the focused tests and confirm they fail because the nested model is unchanged or settings update is untouched.
- [ ] Add a private helper that rewrites both model locations while preserving unrelated fields.
- [ ] Run the focused tests and confirm they pass.

### Task 2: Apply Saved Routes To Settings Updates

**Files:**
- Modify: `internal/transport/session.go`
- Test: `internal/transport/session_test.go`

**Interfaces:**
- Consumes: a parsed `thread/settings/update`, the saved selection store, model catalog, and per-thread coordinator lock.
- Produces: `handleThreadSettingsUpdate(context.Context, rpcMessage, []byte) error`.

- [ ] Add a failing session test showing a saved `kimi -> k3` route replaces both stale settings model fields.
- [ ] Add a passing-transparency assertion for a task without a selection.
- [ ] Dispatch settings updates to a dedicated handler, resolve only saved routes, and call `rewrite.Line` under the existing thread lock.
- [ ] Run transport tests and confirm they pass.

### Task 3: Document And Verify

**Files:**
- Modify: `docs/architecture.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Interfaces:**
- Consumes: implemented routing behavior.
- Produces: concise operator documentation and verified release artifacts.

- [ ] Document collaboration-mode precedence and saved settings-update enforcement.
- [ ] Run `gofmt`, `go test ./...`, `go vet ./...`, the host build, and Linux/Darwin builds.
- [ ] Clean Go build/test caches after each Go command and inspect `/tmp` for task-specific leftovers.
- [ ] Review the final diff for unrelated changes, commit, push, deploy without backups, and verify the installed version.
