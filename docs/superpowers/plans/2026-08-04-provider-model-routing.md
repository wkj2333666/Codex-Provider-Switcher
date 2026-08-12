# Provider Model Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route each selected provider through its configured model so Desktop GLM tasks send and report `glm-5.2` instead of `gpt-5.6-sol`.

**Architecture:** Load an optional strict `models.json` from the existing private switcher state directory into an immutable provider-model catalog. Carry a `modelroute.Route` through request rewriting, handoff coordination, verification, recovery, and status reporting while preserving provider-only behavior when no model is mapped.

**Tech Stack:** Go 1.24, standard-library JSON and filesystem APIs, coder/websocket, Codex app-server v2 JSON-RPC.

## Global Constraints

- Do not modify Android, SSH aliases, shell PATH, provider credentials, Codex config parsing, or the task database.
- Never replay or reconstruct user input.
- Missing `models.json` preserves existing behavior; malformed configuration fails closed.
- `model/list`, unknown methods, upstream messages, and binary messages remain transparent.
- Mixed old/new proxy processes must fail before subscription mutation.
- Codex CLI 0.146.0 remains the minimum supported version.

---

### Task 1: Strict Provider-Model Catalog

**Files:**
- Create: `internal/modelroute/catalog.go`
- Create: `internal/modelroute/catalog_test.go`
- Modify: `internal/transport/proxy.go`
- Test: `internal/transport/proxy_test.go`

**Interfaces:**
- Produces: `modelroute.Route{Provider string, Model string}`.
- Produces: `modelroute.Load(stateDirectory string) (*Catalog, error)`.
- Produces: `(*Catalog).Resolve(provider string) Route` and `modelroute.ValidModel(string) bool`.
- Consumes later: one immutable `*modelroute.Catalog` per proxy connection.

- [ ] **Step 1: Write catalog tests that catch permissive or unsafe loading**

Cover a missing file, the three-entry deployment map, an empty object, duplicate
provider keys, invalid provider ids, empty/whitespace/control/oversized model
ids, trailing JSON, oversized files, symlinks, and non-regular files. Assert
literal resolved routes such as `Route{Provider: "glm", Model: "glm-5.2"}`.

- [ ] **Step 2: Run the catalog tests and verify RED**

Run: `go test ./internal/modelroute`

Expected: FAIL because `internal/modelroute` does not exist.

- [ ] **Step 3: Implement the minimal strict loader**

Use `os.Lstat`, a 16 KiB read limit, `json.Decoder.Token`, explicit duplicate
key detection, a required top-level object, and a second decode requiring EOF.
Return an empty catalog only for `os.ErrNotExist`. Validate provider ids with
`provider.Valid`; validate model ids as non-empty UTF-8, at most 256 bytes, and
free of Unicode whitespace/control characters.

- [ ] **Step 4: Add a proxy test proving malformed models fail before upgrade**

Create `<stateDir>/models.json` with malformed JSON, call `Run` with that state
directory, and assert the returned error is the static configuration error and
contains none of the file contents.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `go test ./internal/modelroute ./internal/transport`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/modelroute internal/transport/proxy.go internal/transport/proxy_test.go
git commit -m "feat: load provider model routes"
```

### Task 2: Route-Aware JSON-RPC Rewriting

**Files:**
- Modify: `internal/rewrite/rewrite.go`
- Modify: `internal/rewrite/rewrite_test.go`
- Modify: `internal/transport/jsonrpc.go`
- Modify: `internal/transport/jsonrpc_test.go`

**Interfaces:**
- Consumes: `modelroute.Route`.
- Produces: `rewrite.Line(line []byte, route modelroute.Route) ([]byte, error)`.
- Produces: `responseThreadRoute(rpcMessage) (threadID string, route modelroute.Route, ok bool)`.

- [ ] **Step 1: Write failing rewrite tests**

Prove mapped routes overwrite both `modelProvider` and `model` on
`thread/start`, `thread/resume`, and `thread/fork`; overwrite only `model` on
`turn/start`; preserve every unrelated field; leave unmapped `turn/start`
byte-for-byte; and leave `model/list` byte-for-byte.

- [ ] **Step 2: Run rewrite tests and verify RED**

Run: `go test ./internal/rewrite`

Expected: FAIL because the current API accepts only a provider and never
rewrites `turn/start` or `model`.

- [ ] **Step 3: Implement route-aware rewriting**

Use JSON object decoding and `json.Marshal` for routing fields. Provider-bearing
thread methods require a non-empty provider. `turn/start` is rewritten only
when `Route.Model` is non-empty. Preserve current `thread/list` behavior.

- [ ] **Step 4: Write and pass response parsing tests**

Test complete mapped responses, provider-only legacy responses, missing model,
malformed fields, and RPC errors. The parser returns a route with an optional
model; callers decide whether a model is required.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `go test ./internal/rewrite ./internal/transport -run 'TestResponseThreadRoute|TestLine'`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/rewrite internal/transport/jsonrpc.go internal/transport/jsonrpc_test.go
git commit -m "feat: rewrite provider model routes"
```

### Task 3: Atomic Handoff And Crash Recovery

**Files:**
- Modify: `internal/handoff/coordinator.go`
- Modify: `internal/handoff/coordinator_test.go`
- Modify: `internal/recovery/store.go`
- Modify: `internal/recovery/store_test.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/recovery_client.go`
- Modify: `internal/transport/recovery_client_test.go`
- Modify: `internal/transport/proxy.go`

**Interfaces:**
- Changes: handoff `Resubscribe` and `ResubscribeAll` consume
  `modelroute.Route` instead of a bare provider.
- Changes: capability probe becomes `prepareHandoffV3` so older live peers fail
  before unsubscribe.
- Changes: `recovery.Journal` adds `Model string` and uses version 2 for mapped
  routes while continuing to read version 1 provider-only journals.
- Session stores verified provider and optional model per loaded thread.

- [ ] **Step 1: Write failing coordinator and recovery-store tests**

Prove resubscribe sends provider and model to every peer, V3 rejects a V2-only
peer before mutation, mapped version-2 journals round trip, invalid models fail,
and existing version-1 journals remain valid only with an empty model.

- [ ] **Step 2: Run focused tests and verify RED**

Run: `go test ./internal/handoff ./internal/recovery`

Expected: FAIL because the control protocol and journal do not carry models.

- [ ] **Step 3: Implement coordinator and journal route propagation**

Add `model` to the bounded control request, validate it with
`modelroute.ValidModel`, change the capability method to `prepareHandoffV3`,
and preserve provider-only routes with an omitted model. Add journal v2 without
weakening strict unknown-field decoding.

- [ ] **Step 4: Write failing session tests**

Cover mapped initial thread requests, mapped Desktop resume, same-provider model
mismatch triggering handoff, internal resume provider/model verification,
mapped peer resubscription, mapped normal `turn/start`, status output, switch
confirmation, recovery journal creation, and recovery repair using the journal
route. Include negative tests where app-server returns the right provider but
the wrong model; no original user turn may reach app-server.

- [ ] **Step 5: Run session tests and verify RED**

Run: `go test ./internal/transport -run 'Model|Route|ProviderStatus|ProviderSwitch|Recovery'`

Expected: FAIL because sessions still track and verify providers only.

- [ ] **Step 6: Implement session route ownership**

Resolve a route from the selected provider, compare the mapped model when
deciding whether to hand off, rewrite mapped normal turns, verify mapped resume
responses, propagate the exact route to peers and recovery, and track the
verified model for feedback. Preserve existing output exactly when no model is
mapped.

- [ ] **Step 7: Run focused and package tests and verify GREEN**

Run: `go test ./internal/handoff ./internal/recovery ./internal/transport`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/handoff internal/recovery internal/transport
git commit -m "feat: switch providers with mapped models"
```

### Task 4: End-to-End GLM Route, Documentation, And Deployment

**Files:**
- Modify: `internal/transport/handoff_integration_test.go`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/architecture.md`
- Deploy: `$CODEX_HOME/codex-provider-switcher/models.json`
- Deploy: `$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher`
- Deploy: `$HOME/.agents/skills/provider`

**Interfaces:**
- The fake app-server records both provider and model for resume and turn
  requests and returns both in thread responses.
- The production deployment maps `openai` and `sub2api` to `gpt-5.6-sol`, and
  `glm` to `glm-5.2`.

- [ ] **Step 1: Write a failing GLM integration test**

Start with a GPT route, issue the exact provider skill command to switch to
`glm`, assert the fake app-server receives `{modelProvider:"glm",model:"glm-5.2"}`
on resume, then send a normal turn containing a conflicting GPT model and assert
the forwarded turn uses `glm-5.2` while its decoded input remains semantically
identical.

- [ ] **Step 2: Run the integration test and verify RED**

Run: `go test ./internal/transport -run TestRunSwitchesProviderAndMappedModel`

Expected: FAIL until the fake server and production bridge carry the model map.

- [ ] **Step 3: Complete the integration path and verify GREEN**

Run: `go test ./internal/transport -run TestRunSwitchesProviderAndMappedModel`

Expected: PASS with the recorded GLM route.

- [ ] **Step 4: Update user and architecture documentation**

Document `models.json`, the need to map every switchable provider, Desktop's
current-model versus picker distinction, fail-closed validation, V3 peer
compatibility, and the provider/model verification sequence. Keep the README
deployment instructions compact.

- [ ] **Step 5: Run repository verification**

Run: `gofmt -w` on changed Go files, `git diff --check`, `go test ./...`,
`go vet ./...`, and `go build -trimpath -o /tmp/codex-provider-switcher ./cmd/codex-provider-switcher`.

Expected: every command exits 0. Run `go test -race ./...`; if the host TSAN VMA
limitation recurs before tests start, report it separately rather than treating
it as a code failure.

- [ ] **Step 6: Commit and push**

```bash
git add README.md README.zh-CN.md docs/architecture.md internal/transport/handoff_integration_test.go
git commit -m "docs: explain provider model mappings"
git push origin system-error-provider-recovery
```

- [ ] **Step 7: Deploy without backups**

Build a staged binary under the install directory, atomically replace the
installed binary, replace the provider skill directory, and write the strict
three-provider `models.json` with mode 0600. Do not retain an old binary backup.
Reconnect or restart only the Desktop proxy connections needed to load the new
binary and catalog; do not alter the separately managed app-server daemon.

- [ ] **Step 8: Verify live deployment**

Confirm the installed binary version/commit, the model map permissions and
resolved values without printing credentials, wrapper delegation, proxy
interception, and app-server process continuity. Ask the user to switch one
idle Desktop task to GLM and confirm the current-thread model label; do not
claim the picker contains GLM.
