# Codex Provider Switcher Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build, test, document, and publicly release a provider-selecting JSON-RPC proxy for Codex Desktop Remote SSH.

**Architecture:** A small Go command resolves provider, socket, and Codex executable configuration, then launches `codex app-server proxy --sock <socket>`. A rewrite package owns fail-closed JSONL transformation, while a proxy package owns bidirectional streams, signals, child cleanup, and exit status.

**Tech Stack:** Go 1.24 standard library, GitHub Actions, GitHub CLI release publishing

---

## File Structure

- `cmd/codex-provider-switcher/main.go`: CLI help/version, signal subscription, dependency wiring, process exit mapping.
- `internal/config/config.go`: environment/flag precedence and validated executable/socket resolution.
- `internal/rewrite/rewrite.go`: JSON-RPC line and stream rewriting without a scanner size limit.
- `internal/proxy/proxy.go`: child process startup, stream forwarding, signal forwarding, cleanup, and exit errors.
- `internal/*/*_test.go`: focused unit and integration coverage for each boundary.
- `.github/workflows/ci.yml`: formatting, vet, tests, race detector, and four cross-builds.
- `.github/workflows/release.yml`: tag validation, quality gate, four archives/checksums, and GitHub Release publication.
- `README.md`, `docs/architecture.md`, `LICENSE`: public usage, design limitations, and licensing.

### Task 1: Module And Rewrite Contract

**Files:**
- Create: `go.mod`
- Create: `internal/rewrite/rewrite_test.go`
- Create: `internal/rewrite/rewrite.go`

- [ ] **Step 1: Initialize the Go module**

Use module path `github.com/wkj2333666/Codex-Provider-Switcher` and Go 1.24.

- [ ] **Step 2: Write failing table tests for target methods**

Cover `thread/start`, `thread/resume`, and `thread/fork` injecting the selected `modelProvider`; cover `thread/list` forcing `modelProviders: []`; cover missing, null, and existing params.

- [ ] **Step 3: Run the focused tests and confirm the missing package fails**

Run: `go test ./internal/rewrite`
Expected: FAIL because the implementation does not exist.

- [ ] **Step 4: Implement semantic JSON rewriting**

Expose:

```go
func Line(line []byte, provider string) ([]byte, error)
func Stream(dst io.Writer, src io.Reader, provider string) error
```

Decode only client objects, overwrite policy-controlled fields with
`json.RawMessage`, preserve all unrelated fields semantically, and pass valid
non-target lines through byte-for-byte.

- [ ] **Step 5: Add adversarial and large-message tests**

Cover malformed JSON, non-object target params, unknown methods, unrelated
fields/IDs, no trailing newline, and a prompt payload larger than 64 KiB.

- [ ] **Step 6: Run rewrite tests**

Run: `go test ./internal/rewrite`
Expected: PASS.

### Task 2: Configuration Resolution

**Files:**
- Create: `internal/config/config_test.go`
- Create: `internal/config/config.go`

- [ ] **Step 1: Write failing precedence and validation tests**

Use temporary Unix sockets and executable files to cover flags over environment,
default `codex`, missing socket/provider, the provider grammar, non-socket paths,
PATH lookup, explicit executable paths, help, and version mode.

- [ ] **Step 2: Run the focused tests and confirm failure**

Run: `go test ./internal/config`
Expected: FAIL because the package is absent.

- [ ] **Step 3: Implement parsing and validation**

Expose:

```go
type Config struct { Provider, Socket, Codex string }
type Result struct { Config Config; ShowVersion bool }
func Parse(args []string, getenv func(string) string) (Result, error)
```

Use `flag.FlagSet` with `ContinueOnError`, validate `[A-Za-z0-9._-]+`, require
and absolutize a Unix socket, and resolve the executable without invoking a
shell.

- [ ] **Step 4: Run configuration tests**

Run: `go test ./internal/config`
Expected: PASS.

### Task 3: Child Proxy Lifecycle

**Files:**
- Create: `internal/proxy/proxy_test.go`
- Create: `internal/proxy/proxy.go`

- [ ] **Step 1: Write a failing helper-process integration harness**

Re-exec the test binary behind an environment guard and verify exact child
arguments, bidirectional forwarding, inherited stderr, stdin EOF, and clean exit.

- [ ] **Step 2: Run the focused test and confirm failure**

Run: `go test ./internal/proxy`
Expected: FAIL because `Run` is missing.

- [ ] **Step 3: Implement the process runner**

Expose:

```go
type Options struct { Config config.Config; Stdin io.Reader; Stdout, Stderr io.Writer; Signals <-chan os.Signal }
func Run(options Options) error
func ExitCode(err error) int
```

Start the exact argument array, stream rewritten input, copy output untouched,
forward Unix signals, close child stdin on EOF, kill and reap on forwarding
errors, and preserve child exit codes.

- [ ] **Step 4: Add failure lifecycle tests**

Cover invalid JSON cleanup, child non-zero status, signal forwarding, child
startup failure, and output/write failures without asserting sensitive bodies.

- [ ] **Step 5: Run proxy tests under the race detector**

Run: `go test -race ./internal/proxy`
Expected: PASS.

### Task 4: Public Command

**Files:**
- Create: `cmd/codex-provider-switcher/main_test.go`
- Create: `cmd/codex-provider-switcher/main.go`

- [ ] **Step 1: Write failing command tests**

Test deterministic usage, version output from an injectable `version` variable,
configuration failures before child startup, and exit-code mapping.

- [ ] **Step 2: Run focused command tests and confirm failure**

Run: `go test ./cmd/codex-provider-switcher`
Expected: FAIL because the command is absent.

- [ ] **Step 3: Wire configuration, signals, and proxy lifecycle**

Keep `main` minimal, send diagnostics to stderr, never print request bodies or
environment values, and use `signal.Notify` for `SIGINT`, `SIGTERM`, and `SIGHUP`.

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS.

### Task 5: Public Documentation And License

**Files:**
- Create: `README.md`
- Create: `docs/architecture.md`
- Create: `LICENSE`
- Create: `.gitignore`

- [ ] **Step 1: Document installation and operation**

Include source/release installation, complete CLI/environment reference, generic
provider examples, shared-daemon topology, Remote SSH invocation, sequential
cross-provider task use, and unsupported same-task concurrency.

- [ ] **Step 2: Document security and architecture boundaries**

State that the tool does not handle keys, edit Codex/SSH configuration, open
SQLite, or start a daemon; explain fail-closed routing and output pass-through.

- [ ] **Step 3: Add MIT licensing and ignore build artifacts**

Use copyright `2026 wkj2333666` and ignore local binaries, archives, coverage,
and temporary build directories.

- [ ] **Step 4: Scan for private data and placeholders**

Run: `rg -n 'TBD|TODO|FIXME|api[_-]?key|https?://' . --glob '!docs/superpowers/**'`
Expected: only intentional public documentation URLs, with no secrets or
placeholders.

### Task 6: CI And Release Automation

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Add least-privilege CI**

Trigger on pushes and pull requests to `main`, plus manual dispatch. Pin
third-party actions to full SHAs; run `gofmt -l`, `go vet ./...`, `go test ./...`,
`go test -race ./...`, and build `linux/amd64`, `linux/arm64`, `darwin/amd64`,
and `darwin/arm64` with `CGO_ENABLED=0`.

- [ ] **Step 2: Add tag-driven release builds**

Trigger on `v*`, validate semantic tag shape, rerun the quality gate, inject the
tag via `-ldflags -X=main.version=...`, create one `.tar.gz` plus `.sha256` per
target, and upload each as a workflow artifact.

- [ ] **Step 3: Add isolated release publishing**

Give only the final job `contents: write`, download all target artifacts, verify
four archives and four checksums, then call `gh release create` with generated
notes. Follow the reviewed separation used by Codex-SSH-Bridge.

- [ ] **Step 4: Validate workflow syntax and release scripts locally**

Parse workflow YAML where a parser is available and execute the shell packaging
logic against locally built binaries.

### Task 7: Full Verification And Public Repository

**Files:**
- Modify only files needed to fix discovered defects.

- [ ] **Step 1: Format and inspect the diff**

Run: `gofmt -w cmd internal` and `git diff --check`.

- [ ] **Step 2: Run fresh quality gates**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Expected: every command exits 0.

- [ ] **Step 3: Build every release target**

Run all four `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build` commands with
release ldflags and inspect the resulting executable formats.

- [ ] **Step 4: Create focused commits**

Commit the specification/plan, implementation/tests, documentation, and
automation in reviewable units without staging unrelated changes.

- [ ] **Step 5: Authenticate GitHub and create the repository**

Ensure `gh auth status` succeeds, then create public repository
`wkj2333666/Codex-Provider-Switcher`, add it as `origin`, and push the intended
default branch.

- [ ] **Step 6: Verify remote state and CI**

Confirm repository visibility is `PUBLIC`, inspect the remote default branch,
and wait for the pushed CI workflow to finish successfully.

- [ ] **Step 7: Exercise automatic release**

Create and push an initial semantic tag only after CI is green, wait for the
Release workflow, and verify the published GitHub Release contains exactly four
archives and four matching checksum assets.
