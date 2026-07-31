# WebSocket Transport Correction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the broken JSONL child-process proxy with a WebSocket-aware Unix-socket proxy and a transparent `codex` wrapper that works with stock Desktop Remote SSH.

**Architecture:** The command accepts one downstream HTTP Upgrade over stdin/stdout, opens a second WebSocket through a Unix socket, and applies the existing JSON-RPC rewrite policy only to downstream text messages. A wrapper mode selected by executable basename intercepts `app-server proxy` and delegates every other invocation to the real Codex executable without spawning a shell.

**Tech Stack:** Go 1.24, `net/http`, Unix domain sockets, `github.com/coder/websocket` v1.8.15, GitHub Actions.

---

## File Structure

- `internal/config/config.go`: direct proxy flags, environment precedence, provider/socket validation.
- `internal/config/config_test.go`: configuration behavior and sanitized failures.
- `internal/wrapper/wrapper.go`: wrapper command classification, official proxy argument parsing, real Codex resolution, recursion protection.
- `internal/wrapper/wrapper_test.go`: wrapper interception and executable-resolution tests.
- `internal/transport/conn.go`: stdin/stdout `net.Conn` and one-shot listener adapters.
- `internal/transport/proxy.go`: HTTP Upgrade coordination, UDS WebSocket dial, message pumps, close handling.
- `internal/transport/proxy_test.go`: real WebSocket-over-UDS integration and raw RFC 6455 client tests.
- `internal/rewrite/rewrite.go`: JSON-RPC message rewrite only; remove obsolete JSONL streaming.
- `internal/rewrite/rewrite_test.go`: retain message-policy tests and remove JSONL-stream tests.
- `cmd/codex-provider-switcher/main.go`: direct/wrapper dispatch, signal context, diagnostics, delegation.
- `cmd/codex-provider-switcher/main_test.go`: process-mode wiring and fail-closed behavior.
- `README.md`: stock Desktop wrapper installation, migration warning, usage, uninstall.
- `docs/architecture.md`: corrected dual-WebSocket architecture and lifecycle.
- `go.mod`, `go.sum`: pin the WebSocket implementation.

### Task 1: Direct Proxy Configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write failing direct-mode tests**

Replace Codex-executable expectations with a proxy-only configuration and add
an explicit parser name:

```go
func TestParseProxyFlagsOverrideEnvironment(t *testing.T) {
    flagSocket := unixSocket(t, "flag.sock")
    envSocket := unixSocket(t, "env.sock")
    got, err := ParseProxy([]string{
        "--provider", "flag-provider",
        "--socket", flagSocket,
    }, environment(map[string]string{
        "CODEX_PROVIDER_SWITCHER_PROVIDER": "env-provider",
        "CODEX_PROVIDER_SWITCHER_SOCKET": envSocket,
    }))
    if err != nil { t.Fatal(err) }
    if got.Config != (Config{Provider: "flag-provider", Socket: flagSocket}) {
        t.Fatalf("Config = %#v", got.Config)
    }
}
```

Keep table tests for missing/invalid provider, missing/non-socket path,
unexpected positional arguments, `--help`, and `--version`. Assert rejected
provider text is absent from errors.

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test ./internal/config`

Expected: FAIL because `ParseProxy` does not exist and `Config` still requires
the obsolete `Codex` field.

- [ ] **Step 3: Implement the proxy-only parser**

Implement:

```go
type Config struct {
    Provider string
    Socket   string
}

type Result struct {
    Config      Config
    ShowVersion bool
}

func ParseProxy(args []string, getenv func(string) string) (Result, error)
```

Resolve flag values over `CODEX_PROVIDER_SWITCHER_PROVIDER` and
`CODEX_PROVIDER_SWITCHER_SOCKET`; validate provider with
`^[A-Za-z0-9._-]+$`; resolve the socket to an absolute existing Unix socket.
Remove Codex executable lookup from this package.

- [ ] **Step 4: Verify GREEN**

Run: `go test ./internal/config`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "refactor: make configuration proxy-only"
```

### Task 2: Transparent Codex Wrapper

**Files:**
- Create: `internal/wrapper/wrapper.go`
- Create: `internal/wrapper/wrapper_test.go`

- [ ] **Step 1: Write failing wrapper tests**

Tests define the desired API:

```go
func TestClassifyInterceptsOnlyAppServerProxy(t *testing.T) {
    action, proxyArgs, err := Classify([]string{"app-server", "proxy", "--sock", "/tmp/a.sock"})
    if err != nil || action != Proxy || !slices.Equal(proxyArgs, []string{"--socket", "/tmp/a.sock"}) {
        t.Fatalf("Classify = %v, %q, %v", action, proxyArgs, err)
    }
    action, _, err = Classify([]string{"app-server", "--listen", "stdio://"})
    if err != nil || action != Delegate { t.Fatalf("action = %v, err = %v", action, err) }
}

func TestClassifyRejectsUnsafeProxyArguments(t *testing.T) {
    for _, args := range [][]string{
        {"app-server", "proxy"},
        {"app-server", "proxy", "--unknown"},
        {"app-server", "proxy", "--sock", "a", "--sock", "b"},
    } {
        if action, _, err := Classify(args); action != Proxy || err == nil {
            t.Fatalf("Classify(%q) = %v, %v", args, action, err)
        }
    }
}
```

Add real-filesystem tests for `ResolveRealCodex(current, getenv, pathEnv)`:
absolute environment override wins; PATH search skips a symlink or hard link to
the switcher; a later distinct executable is selected; relative override and
recursive-only PATH fail without leaking candidate paths.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/wrapper`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement classification and resolution**

Define:

```go
type Action uint8
const (
    Delegate Action = iota
    Proxy
)

func Classify(args []string) (Action, []string, error)
func ResolveRealCodex(current string, getenv func(string) string) (string, error)
```

`Classify` returns `Proxy` for every invocation beginning with
`app-server proxy`, even when arguments are invalid, so malformed invocations
cannot bypass provider enforcement. Accept only one `--sock value` or
`--sock=value` and translate it to `--socket value` for `config.ParseProxy`.

`ResolveRealCodex` requires an absolute override when
`CODEX_PROVIDER_SWITCHER_CODEX` is set. Otherwise inspect each PATH directory
for `codex`, verify regular/executable files, and use `os.SameFile` to skip the
current executable through symlinks or hard links. Return sanitized errors.

- [ ] **Step 4: Verify GREEN**

Run: `go test ./internal/wrapper`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper
git commit -m "feat: add transparent codex wrapper routing"
```

### Task 3: Basic WebSocket-Over-UDS Regression

**Files:**
- Modify: `go.mod`
- Create: `go.sum`
- Create: `internal/transport/conn.go`
- Create: `internal/transport/proxy.go`
- Create: `internal/transport/proxy_test.go`

- [ ] **Step 1: Pin the protocol implementation**

Run: `go get github.com/coder/websocket@v1.8.15`

Expected: `go.mod` and `go.sum` pin v1.8.15.

- [ ] **Step 2: Write the failing P0 regression test**

Create a UDS HTTP server that accepts WebSocket connections with compression
disabled. Connect the switcher's stdin/stdout to one side of `net.Pipe` and
configure a WebSocket client to dial the other side. Send:

```json
{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"keep":true}}
```

Assert the client receives HTTP status 101, the UDS server receives a text
message containing `modelProvider: provider-a` and `keep: true`, and a server
response is returned byte-for-byte.

The public API exercised by the test is:

```go
type Options struct {
    Config         config.Config
    Stdin          io.Reader
    Stdout         io.Writer
    MaxMessageSize int64
}

func Run(ctx context.Context, options Options) error
```

- [ ] **Step 3: Run and verify RED**

Run: `go test ./internal/transport -run TestRunUpgradesAndRewritesOverUnixSocket -v`

Expected: FAIL because `internal/transport` does not exist.

- [ ] **Step 4: Implement the minimum dual-WebSocket proxy**

`conn.go` implements a full-duplex `net.Conn` around separate reader/writer
streams, loops over short writes, and exposes a listener that returns exactly
one connection.

`proxy.go` must:

```go
const maxMessageSize = 64 << 20

func Run(ctx context.Context, options Options) error
func serveConnection(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg config.Config) error
func dialUpstream(ctx context.Context, r *http.Request, socket string) (*websocket.Conn, *http.Response, error)
func bridge(ctx context.Context, downstream, upstream *websocket.Conn, provider string) error
func pump(ctx context.Context, dst, src *websocket.Conn, transform func([]byte) ([]byte, error)) error
```

Validate the downstream Upgrade before dialing. Use an `http.Client` with a
custom transport whose `DialContext` always dials the Unix socket. Remove
generated and hop-by-hop WebSocket headers, forward subprotocol offers, disable
compression on dial and accept, and set both read limits to 64 MiB. Apply
`rewrite.Line` only to client text messages; copy upstream messages and binary
messages without parsing.

- [ ] **Step 5: Verify GREEN**

Run: `go test ./internal/transport -run TestRunUpgradesAndRewritesOverUnixSocket -v`

Expected: PASS and the old P0 handshake failure is covered by a real HTTP 101.

- [ ] **Step 6: Run package tests and commit**

Run: `go test ./internal/transport ./internal/rewrite`

Expected: PASS.

```bash
git add go.mod go.sum internal/transport
git commit -m "fix: proxy websocket messages over unix sockets"
```

### Task 4: RFC 6455 Boundaries And Lifecycle

**Files:**
- Modify: `internal/transport/proxy_test.go`
- Modify: `internal/transport/proxy.go`
- Modify: `internal/transport/conn.go`

- [ ] **Step 1: Add failing frame-boundary tests**

Use a raw downstream client helper that writes a valid HTTP Upgrade and masked
frames. Add separate tests for:

```go
func TestRunSupportsSevenSixteenAndSixtyFourBitPayloadLengths(t *testing.T)
func TestRunReassemblesFragmentedClientText(t *testing.T)
func TestRunHandlesPingPongAndClose(t *testing.T)
func TestRunDoesNotNegotiatePerMessageDeflate(t *testing.T)
func TestRunForwardsEndToEndHeadersAndSubprotocol(t *testing.T)
func TestRunPassesBinaryMessagesWithoutParsing(t *testing.T)
func TestRunPreservesUpstreamBinaryMessages(t *testing.T)
func TestRunHandlesShortReadsAndWrites(t *testing.T)
func TestRunRejectsMessageAboveLimit(t *testing.T)
func TestRunRejectsPlainHTTPWithoutDialingUpstream(t *testing.T)
func TestRunReturnsBadGatewayBeforeUpgradeWhenUpstreamFails(t *testing.T)
```

The raw frame writer must generate client masking keys and lengths directly,
because using an echoing JSONL child or relying only on a high-level client
would not reproduce the reviewed defect.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/transport -run 'TestRun(Supports|Reassembles|Handles|DoesNot|Passes|Rejects)' -v`

Expected: at least the fragmentation, close propagation, partial-I/O, or
message-limit assertion fails against the minimal bridge.

- [ ] **Step 3: Complete control-flow behavior**

Make the first pump result cancel the other direction, translate valid close
status/reason to the peer, treat normal closure as success, use bounded close
contexts, and return sanitized phase errors for abnormal failures. Ensure
`stdioConn.Close` unblocks closable readers without closing stdout twice.
`Options.MaxMessageSize` is an internal test seam: zero selects the fixed 64 MiB
production default, while tests use a small positive limit to exercise the
message-too-big close path without excessive memory use.

- [ ] **Step 4: Verify GREEN and race behavior**

Run:

```text
go test ./internal/transport -v
go test -race ./internal/transport
```

Expected: PASS with no races or leaked goroutines.

- [ ] **Step 5: Commit**

```bash
git add internal/transport
git commit -m "test: cover websocket framing and lifecycle"
```

### Task 5: Command Wiring And Legacy Removal

**Files:**
- Modify: `cmd/codex-provider-switcher/main.go`
- Modify: `cmd/codex-provider-switcher/main_test.go`
- Delete: `internal/proxy/proxy.go`
- Delete: `internal/proxy/proxy_test.go`
- Modify: `internal/rewrite/rewrite.go`
- Modify: `internal/rewrite/rewrite_test.go`

- [ ] **Step 1: Write failing dispatch tests**

Tests call a dependency-injected command runner and verify:

```go
func TestRunDirectProxyMode(t *testing.T)
func TestRunWrapperInterceptsAppServerProxy(t *testing.T)
func TestRunWrapperFailsClosedWithoutProvider(t *testing.T)
func TestRunWrapperDelegatesOtherCommands(t *testing.T)
func TestRunWrapperDoesNotExposeSwitcherVersion(t *testing.T)
```

Direct syntax is `codex-provider-switcher proxy --provider ... --socket ...`.
Wrapper syntax is selected by `filepath.Base(argv0) == "codex"`. Delegation
receives the resolved real executable, an argv slice beginning with that path,
and the unchanged environment.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./cmd/codex-provider-switcher -v`

Expected: FAIL because the command still exposes the old option-only syntax and
uses `internal/proxy`.

- [ ] **Step 3: Wire the corrected command**

Use `signal.NotifyContext` for `SIGINT`, `SIGTERM`, and `SIGHUP`. Direct mode
handles `proxy`, `--help`, and `--version`. Wrapper mode calls
`wrapper.Classify`; intercepted calls are parsed through `config.ParseProxy`
and passed to `transport.Run`; delegated calls resolve the real Codex and use
an injected `syscall.Exec` adapter.

Use exit code 2 for configuration/wrapper usage failures and 1 for transport or
delegation failures. Never format argument values or environment contents in
diagnostics.

- [ ] **Step 4: Remove obsolete JSONL paths**

Delete `internal/proxy`. Remove `rewrite.Stream`, line-ending helpers, and
stream-specific tests. Keep `rewrite.Line` and all JSON-RPC policy tests.

- [ ] **Step 5: Verify GREEN**

Run: `go test ./cmd/codex-provider-switcher ./internal/...`

Expected: PASS and `rg 'rewrite\.Stream|internal/proxy|app-server proxy child'`
returns no production references.

- [ ] **Step 6: Commit**

```bash
git add -A cmd internal
git commit -m "feat: wire direct and transparent wrapper modes"
```

### Task 6: Documentation And Repository Contract

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `internal/repository/workflows_test.go`

- [ ] **Step 1: Write failing repository documentation assertions**

Add a test that reads README and architecture documentation and requires these
terms while rejecting the obsolete claims:

```go
required := []string{"WebSocket", "ln -s", "CODEX_PROVIDER_SWITCHER_PROVIDER", "AcceptEnv", "v0.1.0", "uninstall"}
rejected := []string{"runs the official stdio proxy", "Server output is copied byte-for-byte"}
```

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/repository -run TestDocumentationDescribesStockDesktopWrapper -v`

Expected: FAIL on missing wrapper and WebSocket migration content.

- [ ] **Step 3: Rewrite user and architecture documentation**

README must start with a visible warning that v0.1.0 is broken, explain that
Desktop finds remote `codex` through login-shell PATH, provide reversible
commands to preserve the real Codex and install a `codex` symlink, describe
provider propagation with `SetEnv`/`AcceptEnv`, retain direct proxy syntax for
testing, and show uninstall restoration. State that headers are forwarded but
not logged, text messages are selectively parsed, binary/server messages pass
through, compression is disabled, and the limit is 64 MiB.

Architecture documentation must show:

```text
Desktop stdio WebSocket -> switcher -> UDS WebSocket -> app-server
```

and describe wrapper delegation, two message pumps, close/cancel behavior, and
the absence of a child proxy process.

- [ ] **Step 4: Verify GREEN**

Run: `go test ./internal/repository`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/architecture.md internal/repository/workflows_test.go
git commit -m "docs: document stock Desktop websocket setup"
```

### Task 7: Complete Verification And Release Preparation

**Files:**
- Modify only files required by discovered test failures.

- [ ] **Step 1: Format and run all quality gates**

Run:

```text
gofmt -w cmd internal
test -z "$(gofmt -l cmd internal)"
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/codex-provider-switcher
```

Expected: every command exits zero.

- [ ] **Step 2: Run a real Codex socket smoke test**

Build the switcher, connect it to the current real app-server Unix socket, send
a standards-compliant WebSocket Upgrade followed by a harmless valid request
whose routing behavior can be observed without exposing prompt or credential
data, and confirm the response is `101 Switching Protocols` rather than the
v0.1.0 `invalid JSON object` failure.

- [ ] **Step 3: Review the complete diff against the design**

Run:

```text
git diff --check origin/main...HEAD
git status --short
rg -n 'TODO|TBD|FIXME|parse and rewrite JSONL|runs the official stdio proxy' .
```

Expected: no whitespace errors, no uncommitted production changes after the
final commit, and no obsolete implementation claims outside historical design
context.

- [ ] **Step 4: Request independent code review**

Review `origin/main..HEAD` against this plan and the corrected design. Fix all
Critical and Important findings with failing regression tests before changing
the branch status.

- [ ] **Step 5: Commit verification fixes**

```bash
git add -A
git commit -m "test: finalize websocket transport correction"
```

Skip this commit only when there are no post-review changes.

- [ ] **Step 6: Publish after branch integration**

After the corrected branch is integrated into `main`, edit the existing
v0.1.0 GitHub release notes to display a protocol-incompatibility warning,
push tag `v0.2.0`, wait for CI and release workflows, and verify eight assets
(four archives and four checksums) are attached to the public release.
