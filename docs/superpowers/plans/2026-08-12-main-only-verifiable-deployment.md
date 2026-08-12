# Main-Only Verifiable Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:test-driven-development` for each behavior change and `superpowers:verification-before-completion` before declaring success.

**Goal:** Make `main` the only deployable source, embed auditable build provenance in every switcher binary, and replace ad-hoc local installation with one guarded atomic deployment command.

**Architecture:** Reconcile the existing recovery branch with `main`, then add a small build-information surface to the command package. A repository-owned Bash script validates Git identity before it tests, builds, inspects, and atomically installs a static Linux arm64 binary. Repository tests exercise both the command output and deployment/release policy without touching the user's real installation.

**Tech Stack:** Go 1.25, Bash, Git, GitHub Actions, Go's `os/exec` integration tests.

---

### Task 1: Reconcile the feature history with main

**Files:**
- Merge: `docs/superpowers/specs/2026-08-12-main-only-verifiable-deployment-design.md`
- Merge: all files changed on `system-error-provider-recovery`

**Step 1: Confirm both worktrees are clean**

Run `git status --short --branch` in the main checkout and feature worktree. Stop if either contains unrelated changes.

**Step 2: Merge main into the isolated feature worktree**

Run `git merge main` from `.worktrees/system-error-provider-recovery`. Preserve the feature implementation and the newer main-only deployment specification if documentation conflicts.

**Step 3: Establish the merged baseline**

Run:

```bash
go test ./...
go clean -cache -testcache
```

Expected: all tests pass and Go caches are cleaned even if the test command fails.

### Task 2: Add self-describing build metadata

**Files:**
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`

**Step 1: Write failing command tests**

Add tests that set `version`, `commit`, `source`, and `builtAt`, then assert:

- `--version` contains the version and seven-character commit;
- `--build-info` emits one JSON object with all four complete fields;
- default values remain explicit and valid;
- invoking the executable as `codex --build-info` delegates to the real Codex instead of intercepting it.

**Step 2: Run the focused tests and confirm failure**

Run:

```bash
go test ./cmd/codex-provider-switcher -run 'TestRun(Version|BuildInfo|WrapperDelegatesBuildInfo)'
go clean -cache -testcache
```

Expected: tests fail because the additional variables and `--build-info` behavior do not exist.

**Step 3: Implement the smallest build-info surface**

Declare link-time defaults:

```go
var (
    version = "dev"
    commit  = "unknown"
    source  = "unknown"
    builtAt = "unknown"
)
```

Add a JSON-tagged build-info struct, a direct-command `--build-info` case, usage text, safe short-SHA formatting, and JSON encoding error handling. Preserve wrapper delegation for every non-proxy command.

**Step 4: Run focused and package tests**

Run each test command followed immediately by `go clean -cache -testcache`:

```bash
go test ./cmd/codex-provider-switcher -run 'TestRun(Version|BuildInfo|WrapperDelegatesBuildInfo)'
go clean -cache -testcache
go test ./cmd/codex-provider-switcher
go clean -cache -testcache
```

Expected: all tests pass.

### Task 3: Add guarded local deployment

**Files:**
- Create: `scripts/deploy-local.sh`
- Create: `internal/repository/deploy_local_test.go`

**Step 1: Write failing repository tests for source guards**

Create temporary Git repositories and run `scripts/deploy-local.sh --check-source` with `CPS_SOURCE_DIR` pointed at each fixture. Test acceptance of a clean, synchronized `main` and rejection of:

- a non-main branch;
- tracked or untracked dirt;
- missing `origin/main`;
- main ahead of or behind `origin/main`.

Also inspect the script for static Linux arm64 settings, all four linker variables, cache-cleaning traps, same-directory staging, atomic rename, installed metadata validation, and absence of backup creation.

**Step 2: Run the repository tests and confirm failure**

```bash
go test ./internal/repository -run 'TestDeployLocal'
go clean -cache -testcache
```

Expected: tests fail because `scripts/deploy-local.sh` does not exist.

**Step 3: Implement the deployment script**

Implement strict Bash behavior with these invariants:

- derive the repository from the script, with `CPS_SOURCE_DIR` accepted only as an explicit test/automation override;
- reject arguments other than optional `--check-source`;
- validate clean `main` and exact `HEAD == refs/remotes/origin/main` before any build;
- run test and vet gates and clean Go caches after every Go command;
- build `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` with `version=main-<short>`, full commit, `source=main`, and UTC RFC 3339 build time;
- compare candidate JSON to the exact expected JSON before installation;
- stage with mode `0755` in the install directory and rename atomically without a backup;
- validate the installed JSON and remove candidate/stage artifacts through a trap;
- never stop or restart a running proxy.

**Step 4: Run focused repository tests**

```bash
go test ./internal/repository -run 'TestDeployLocal'
go clean -cache -testcache
```

Expected: all deployment-policy tests pass.

### Task 4: Stamp release builds and document the supported path

**Files:**
- Modify: `.github/workflows/release.yml`
- Modify: `internal/repository/workflows_test.go`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Step 1: Write failing release-workflow assertions**

Require release linker values for `main.version`, `main.commit`, `main.source`, and `main.builtAt`, plus a native `--build-info` verification step.

**Step 2: Run the focused test and confirm failure**

```bash
go test ./internal/repository -run 'TestReleaseContainsRequiredTargetsChecksumsAndPermissions'
go clean -cache -testcache
```

Expected: the test fails on the missing linker values.

**Step 3: Update workflow and concise user docs**

Compute one RFC 3339 UTC build timestamp for each release build, stamp the tag, `GITHUB_SHA`, source `release`, and timestamp, then inspect native Linux amd64 metadata. Update both READMEs to make `scripts/deploy-local.sh` the only source deployment command, state its main/clean/pushed guards, show `--build-info`, and explain that Remote SSH must reconnect to load the replacement.

**Step 4: Run focused tests**

```bash
go test ./internal/repository
go clean -cache -testcache
```

Expected: all repository-policy tests pass.

### Task 5: Verify, integrate, push, and deploy

**Files:**
- Verify: entire repository

**Step 1: Format and inspect changes**

Run `gofmt` on changed Go files and `git diff --check`.

**Step 2: Run full quality gates**

Run every Go command separately and clean caches after each:

```bash
go test ./...
go clean -cache -testcache
go vet ./...
go clean -cache -testcache
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/codex-provider-switcher
go clean -cache -testcache
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/codex-provider-switcher
go clean -cache -testcache
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/codex-provider-switcher
go clean -cache -testcache
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/codex-provider-switcher
go clean -cache -testcache
```

Write cross-build outputs to a temporary directory and remove it before completion.

**Step 3: Commit the implementation branch**

Commit only the reviewed deployment and provenance changes. Confirm the feature worktree is clean.

**Step 4: Merge into main and push**

Merge `system-error-provider-recovery` into `main` with `--no-ff`, rerun `go test ./...` followed by cache cleaning, and push `main`. Confirm local `HEAD` exactly equals `origin/main`.

**Step 5: Deploy only through the guarded script**

Run:

```bash
scripts/deploy-local.sh
```

Expected: the script reports the installed full main commit and `source=main`, leaves no backup or staging file, and reminds the user to reconnect Desktop Remote SSH. Do not kill the active proxy in this session.

**Step 6: Perform final evidence checks**

Query the installed executable with `--version` and `--build-info`, verify mode `0755`, confirm no temporary deployment artifact or Go build cache remains, and confirm both main worktree and feature worktree are clean.
