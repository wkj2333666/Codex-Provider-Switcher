# Wrapper Compatibility Patch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the transparent wrapper compatible with stock Codex Desktop proxy commands, preserve official Host semantics, and include the WebSocket dependency's ISC notice in every release archive.

**Architecture:** Extend `internal/wrapper` with a three-stage command parser and wrapper-only default socket resolution while leaving direct-mode validation in `internal/config`. Restore downstream Host through the existing upstream WebSocket dial options. Treat third-party licensing as a repository and release-workflow contract.

**Tech Stack:** Go 1.24, `flag`, Unix sockets, `net/http`, `github.com/coder/websocket` v1.8.15, GitHub Actions.

---

## File Structure

- `internal/wrapper/wrapper.go`: parse the effective Codex command, consume supported common options, resolve wrapper socket defaults.
- `internal/wrapper/wrapper_test.go`: command grammar, fail-closed behavior, socket precedence, and delegation tests.
- `cmd/codex-provider-switcher/main.go`: pass environment lookup into wrapper classification.
- `cmd/codex-provider-switcher/main_test.go`: stock no-socket wrapper integration and delegation assertions.
- `internal/transport/proxy.go`: preserve downstream Host on the upstream UDS handshake.
- `internal/transport/proxy_test.go`: Host and Origin compatibility test.
- `THIRD_PARTY_NOTICES`: pinned Coder WebSocket module and complete ISC license.
- `internal/repository/workflows_test.go`: notice content and release archive contract.
- `.github/workflows/release.yml`: copy and verify the notice in every archive.
- `README.md`, `docs/architecture.md`: threat model, stock default socket, accepted wrapper forms, license disclosure.

### Task 1: Three-Stage Wrapper Parser

**Files:**
- Modify: `internal/wrapper/wrapper.go`
- Modify: `internal/wrapper/wrapper_test.go`
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`

- [ ] **Step 1: Write failing legal-command tests**

Change the API to receive environment lookup and add table-driven cases:

```go
func TestClassifyInterceptsProxyWithCommonOptionsAtEveryLayer(t *testing.T) {
    tests := [][]string{
        {"app-server", "proxy"},
        {"-c", `model="x"`, "app-server", "proxy"},
        {"app-server", "-c", `model="x"`, "proxy"},
        {"app-server", "proxy", "-c", `model="x"`},
        {"--enable", "feature-a", "app-server", "proxy"},
        {"app-server", "--disable=feature-b", "proxy"},
        {"app-server", "proxy", "--config=model=\"x\""},
    }
    for _, args := range tests {
        action, proxyArgs, err := Classify(args, env(map[string]string{
            "CODEX_HOME": "/tmp/codex-home",
        }))
        if err != nil || action != Proxy {
            t.Fatalf("Classify(%q) = %v, %q, %v", args, action, proxyArgs, err)
        }
        want := "/tmp/codex-home/app-server-control/app-server-control.sock"
        if !slices.Equal(proxyArgs, []string{"--socket", want}) {
            t.Fatalf("proxy args = %q, want socket %q", proxyArgs, want)
        }
    }
}
```

Add a case with repeatable common options before, between, and after both
subcommands. Add `--strict-config` before `app-server` and before `proxy`.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/wrapper -run TestClassifyInterceptsProxyWithCommonOptionsAtEveryLayer -v`

Expected: FAIL because `Classify` has the old signature, requires `--sock`, and
delegates the first two reviewed forms.

- [ ] **Step 3: Implement the explicit staged parser**

Keep the public result shape and implement:

```go
func Classify(args []string, getenv func(string) string) (Action, []string, error)
func consumeCommonOption(args []string, index int, allowStrict bool) (next int, consumed bool, err error)
func looksLikeProxy(args []string) bool
func defaultSocket(getenv func(string) string) (string, error)
```

`consumeCommonOption` accepts split and equals forms for `-c/--config`,
`--enable`, and `--disable`; it rejects missing or empty values with static
errors. It accepts `--strict-config` only when `allowStrict` is true.

Parse top-level supported options, require the first positional command to be
`app-server`, parse app-server options, require `proxy`, then parse proxy
options and one optional `--sock`. A known other command returns `Delegate`.
Proxy help returns `Delegate`. Once proxy is effective, all errors return
`Proxy` with a sanitized error.

- [ ] **Step 4: Add failing ambiguity and delegation tests**

```go
func TestClassifyFailsClosedForAmbiguousProxyOptions(t *testing.T) {
    tests := [][]string{
        {"--secret-option", "app-server", "proxy"},
        {"app-server", "--secret-option", "proxy"},
        {"app-server", "proxy", "--secret-option"},
        {"app-server", "proxy", "-c"},
        {"app-server", "proxy", "--enable="},
    }
    for _, args := range tests {
        action, _, err := Classify(args, env(map[string]string{"CODEX_HOME": "/tmp/home"}))
        if action != Proxy || err == nil || strings.Contains(err.Error(), "secret") {
            t.Fatalf("Classify(%q) = %v, %v", args, action, err)
        }
    }
}

func TestClassifyDelegatesKnownNonProxyCommands(t *testing.T) {
    tests := [][]string{
        {"exec", "echo", "app-server", "proxy"},
        {"app-server", "daemon", "proxy"},
        {"app-server", "proxy", "--help"},
    }
    for _, args := range tests {
        action, _, err := Classify(args, os.Getenv)
        if err != nil || action != Delegate { t.Fatalf("%q: %v, %v", args, action, err) }
    }
}
```

- [ ] **Step 5: Run parser tests and verify GREEN**

Run: `go test ./internal/wrapper -v`

Expected: PASS.

- [ ] **Step 6: Update command wiring and tests**

Change wrapper dispatch to:

```go
action, proxyArgs, err := wrapper.Classify(args, deps.getenv)
```

Update existing command tests to provide `CODEX_HOME` when no explicit socket
is present. Verify help delegation does not require provider identity.

- [ ] **Step 7: Run command tests and commit**

Run: `go test ./cmd/codex-provider-switcher ./internal/wrapper`

Expected: PASS.

```bash
git add internal/wrapper cmd/codex-provider-switcher
git commit -m "fix: parse stock Codex proxy invocations"
```

### Task 2: Default Socket And Stock Wrapper Integration

**Files:**
- Modify: `internal/wrapper/wrapper_test.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`

- [ ] **Step 1: Write failing socket-precedence tests**

```go
func TestClassifyResolvesWrapperSocketPrecedence(t *testing.T) {
    tests := []struct {
        name string
        args []string
        env map[string]string
        want string
    }{
        {"explicit", []string{"app-server", "proxy", "--sock", "/explicit.sock"}, map[string]string{"CODEX_PROVIDER_SWITCHER_SOCKET": "/env.sock", "CODEX_HOME": "/home"}, "/explicit.sock"},
        {"switcher env", []string{"app-server", "proxy"}, map[string]string{"CODEX_PROVIDER_SWITCHER_SOCKET": "/env.sock", "CODEX_HOME": "/home"}, "/env.sock"},
        {"codex home", []string{"app-server", "proxy"}, map[string]string{"CODEX_HOME": "/home"}, "/home/app-server-control/app-server-control.sock"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            action, proxyArgs, err := Classify(tt.args, env(tt.env))
            if err != nil || action != Proxy {
                t.Fatalf("Classify() = %v, %q, %v", action, proxyArgs, err)
            }
            if !slices.Equal(proxyArgs, []string{"--socket", tt.want}) {
                t.Fatalf("proxy args = %q, want socket %q", proxyArgs, tt.want)
            }
        })
    }
}
```

Use `t.Setenv("HOME", tempHome)` with `os.Getenv` for the `.codex` fallback and
verify a missing home produces a static error.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/wrapper -run TestClassifyResolvesWrapperSocketPrecedence -v`

Expected: FAIL until all precedence branches are implemented.

- [ ] **Step 3: Complete default resolution**

Use explicit socket first, then `CODEX_PROVIDER_SWITCHER_SOCKET`, then
`CODEX_HOME`, then `os.UserHomeDir()`. Join path segments with `filepath.Join`.
Do not stat the path in wrapper; `config.ParseProxy` remains the single owner of
absolute-path and Unix-socket validation.

- [ ] **Step 4: Write the failing main-level stock invocation test**

Create a temporary `CODEX_HOME/app-server-control/app-server-control.sock`,
start a real UDS WebSocket test server, and invoke:

```go
codeDone := make(chan int, 1)
go func() {
    codeDone <- run(context.Background(), "codex", []string{"app-server", "proxy"}, dependencies{
        getenv: env(map[string]string{
            "CODEX_PROVIDER_SWITCHER_PROVIDER": "provider-a",
            "CODEX_HOME": codexHome,
        }),
        stdin: switcherSide,
        stdout: switcherSide,
        runProxy: transport.Run,
    })
}()
```

Dial the opposite `net.Pipe` side as a WebSocket client, assert HTTP 101, send a
masked `thread/start` text message, and assert the UDS server receives
`modelProvider: provider-a`.

- [ ] **Step 5: Run and verify RED, then GREEN**

Run: `go test ./cmd/codex-provider-switcher -run TestRunStockWrapperUsesDefaultSocket -v`

Expected RED: current wrapper rejects the missing `--sock`.

After default resolution is present, expected GREEN: PASS with a real 101 and
provider rewrite.

- [ ] **Step 6: Run related packages and commit**

Run: `go test ./cmd/codex-provider-switcher ./internal/wrapper ./internal/transport`

Expected: PASS.

```bash
git add internal/wrapper cmd/codex-provider-switcher
git commit -m "test: cover stock Desktop default socket"
```

### Task 3: Restore Official Host Semantics

**Files:**
- Modify: `internal/transport/proxy_test.go`
- Modify: `internal/transport/proxy.go`

- [ ] **Step 1: Change the handshake expectation and verify RED**

In `TestRunForwardsHeadersPathAndSubprotocolWithoutCompression`, require:

```go
if details.host != "desktop.test" {
    t.Fatalf("upstream Host = %q, want desktop.test", details.host)
}
```

Run: `go test ./internal/transport -run TestRunForwardsHeadersPathAndSubprotocolWithoutCompression -v`

Expected: FAIL with upstream Host `localhost`.

- [ ] **Step 2: Restore Host through the WebSocket dial options**

```go
return websocket.Dial(ctx, "ws://localhost"+path, &websocket.DialOptions{
    HTTPClient: client,
    HTTPHeader: headers,
    Host: request.Host,
    Subprotocols: headerTokens(request.Header.Values("Sec-WebSocket-Protocol")),
    CompressionMode: websocket.CompressionDisabled,
})
```

Keep `Host` deleted from the cloned header map so only one Host field is sent.

- [ ] **Step 3: Verify GREEN and commit**

Run: `go test ./internal/transport`

Expected: PASS.

```bash
git add internal/transport/proxy.go internal/transport/proxy_test.go
git commit -m "fix: preserve downstream websocket host"
```

### Task 4: Third-Party Notice And Release Archive

**Files:**
- Create: `THIRD_PARTY_NOTICES`
- Modify: `internal/repository/workflows_test.go`
- Modify: `.github/workflows/release.yml`
- Modify: `README.md`

- [ ] **Step 1: Write failing repository and workflow tests**

Add:

```go
func TestThirdPartyNoticesContainsCoderWebSocketLicense(t *testing.T) {
    content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "THIRD_PARTY_NOTICES"))
    if err != nil { t.Fatal(err) }
    for _, required := range []string{
        "github.com/coder/websocket v1.8.15",
        "Copyright (c) 2025 Coder",
        "Permission to use, copy, modify, and distribute this software",
        `THE SOFTWARE IS PROVIDED "AS IS"`,
    } {
        if !strings.Contains(string(content), required) { t.Errorf("missing %q", required) }
    }
}
```

Extend the release workflow assertion with `THIRD_PARTY_NOTICES` and a tar
listing check.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/repository -v`

Expected: FAIL because the notice and packaging references do not exist.

- [ ] **Step 3: Add the complete notice**

Create `THIRD_PARTY_NOTICES` with the module/version heading followed by the
complete unmodified `github.com/coder/websocket@v1.8.15/LICENSE.txt` text,
including `Copyright (c) 2025 Coder`.

- [ ] **Step 4: Package and verify the notice**

Change the release copy command to:

```text
cp README.md LICENSE THIRD_PARTY_NOTICES "$root/"
```

After creating each archive, require:

```text
tar -tzf "dist/${name}.tar.gz" | grep -Fx "${name}/THIRD_PARTY_NOTICES"
```

Update README's license section to identify the project MIT license and link
the bundled third-party notices.

- [ ] **Step 5: Verify GREEN and commit**

Run: `go test ./internal/repository`

Expected: PASS.

```bash
git add THIRD_PARTY_NOTICES .github/workflows/release.yml internal/repository/workflows_test.go README.md
git commit -m "build: include third-party license notices"
```

### Task 5: Documentation, Verification, And v0.2.1

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify only implementation files required by verified findings.

- [ ] **Step 1: Update behavior documentation**

Document the trusted same-user threat model, stock no-socket invocation,
socket precedence, supported three-layer options, fail-closed parse errors, and
preserved Host. Mark v0.2.0 as incompatible with stock Desktop and direct users
to v0.2.1 or newer.

- [ ] **Step 2: Run complete local gates**

```text
go mod tidy
git diff --exit-code -- go.mod go.sum
test -z "$(gofmt -l cmd internal)"
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/cps-linux-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/cps-linux-arm64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o /tmp/cps-darwin-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/cps-darwin-arm64 ./cmd/codex-provider-switcher
```

Expected: every command exits zero. Record the local ARM ThreadSanitizer VMA
limitation and use GitHub x86 CI as the race gate.

- [ ] **Step 3: Run real stock-command smoke test**

Build the wrapper, place a temporary symlink named `codex` before the real CLI,
set provider and real-Codex environment values, then send a raw WebSocket
Upgrade through `codex app-server proxy` without `--sock`. Expect HTTP 101 from
the current default control socket.

- [ ] **Step 4: Request independent code review**

Review the patch against
`docs/superpowers/specs/2026-07-31-wrapper-compatibility-design.md`. Fix every
Critical and Important issue with a failing regression test.

- [ ] **Step 5: Integrate and publish**

Push the feature branch, create a PR, wait for Ubuntu race, macOS tests, and all
four cross-builds, then merge. Add a stock-incompatibility warning to v0.2.0,
tag merged main as `v0.2.1`, wait for the release workflow, and verify all four
archives contain `THIRD_PARTY_NOTICES` and have matching checksum assets.
