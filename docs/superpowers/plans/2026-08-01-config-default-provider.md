# Codex Config Default Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the unsaved-task provider from Codex `config.toml` instead of `CODEX_PROVIDER_SWITCHER_PROVIDER`.

**Architecture:** `internal/config` resolves the Codex home and parses one bounded TOML document. The direct flag overrides the document; persistent task selections remain unchanged in `internal/transport`.

**Tech Stack:** Go 1.24, `pelletier/go-toml/v2`, GitHub Actions.

## Global Constraints

- Missing config or `model_provider` means built-in provider `openai`.
- Invalid explicit configuration fails closed without leaking paths or values.
- `CODEX_PROVIDER_SWITCHER_PROVIDER` has no effect.
- Existing daemon and proxy processes are not restarted during deployment.

---

### Task 1: Config Resolution

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `THIRD_PARTY_NOTICES`

**Interfaces:**
- Consumes: `ParseProxy(args []string, getenv func(string) string)`
- Produces: `Config.Provider` from `--provider`, TOML `model_provider`, or `openai`

- [ ] **Step 1: Write failing tests** for explicit config, implicit `openai`, ignored legacy environment, flag precedence, invalid TOML, invalid provider, and resolution from `CODEX_HOME` and socket layout.
- [ ] **Step 2: Run RED** with `go test ./internal/config -count=1 -v` and confirm failures are caused by environment-based resolution.
- [ ] **Step 3: Implement bounded TOML resolution** with `pelletier/go-toml/v2` and sanitized errors.
- [ ] **Step 4: Run GREEN** with `go test ./internal/config -count=1 -v`.
- [ ] **Step 5: Add the dependency license** and run `go mod tidy`.

### Task 2: Public Contract

**Files:**
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`
- Modify: `internal/repository/workflows_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `plugins/codex-provider-switcher/.codex-plugin/plugin.json`

**Interfaces:**
- Consumes: the new config resolution contract
- Produces: help and installation instructions with no provider environment variable

- [ ] **Step 1: Write failing CLI and repository tests** that reject the legacy environment variable from public docs and require `model_provider` semantics.
- [ ] **Step 2: Run RED** with `go test ./cmd/codex-provider-switcher ./internal/repository -count=1`.
- [ ] **Step 3: Update help, docs, architecture, notices checks, and plugin version.**
- [ ] **Step 4: Run GREEN** for the focused packages.

### Task 3: Verify And Release

**Files:**
- Modify only files required by failing verification.

**Interfaces:**
- Produces: merged release and deployed binary

- [ ] **Step 1: Run full tests, vet, formatting, repeated transport tests, and four CGO-disabled cross-builds.**
- [ ] **Step 2: Commit, push, open a ready PR, and wait for all CI checks.**
- [ ] **Step 3: Merge, tag the next semantic release, and verify all eight assets.**
- [ ] **Step 4: Install the arm64 release atomically, validate a real WebSocket handshake, and confirm daemon/proxy PID and start times did not change.**
