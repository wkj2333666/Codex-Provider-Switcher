# Send-Time Provider Handoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the provider configured by the SSH alias that sends the next `turn/start` safely take ownership of an idle persisted thread without restarting app-server or changing its thread id.

**Architecture:** Add a short-lived distributed coordinator formed by the live switcher processes for one app-server socket. A stateful transport session multiplexes Desktop and switcher-internal JSON-RPC messages, while a two-phase peer protocol and per-thread `flock` remove subscribers before a verified different-provider resume.

**Tech Stack:** Go 1.24, Unix domain sockets, `syscall.Flock`, `encoding/json`, `github.com/coder/websocket`, Codex app-server v2 JSON-RPC.

---

## File Structure

- `internal/handoff/coordinator.go`: runtime namespace, peer lifecycle, two-phase peer broadcast, per-thread file locks.
- `internal/handoff/coordinator_test.go`: namespace, lock, stale peer, busy peer, timeout, and bounded protocol tests.
- `internal/transport/jsonrpc.go`: bounded JSON-RPC envelope parsing, ids, thread ids, response metadata, and static error encoding.
- `internal/transport/jsonrpc_test.go`: parser and encoding tests independent of WebSocket transport.
- `internal/transport/session.go`: stateful WebSocket multiplexer, active/effective state, internal requests, control handler, and handoff algorithm.
- `internal/transport/session_test.go`: session state and response-correlation tests.
- `internal/transport/proxy.go`: construct and run a session instead of the old generic bridge.
- `internal/transport/proxy_test.go`: two-connection provider handoff integration coverage.
- `internal/rewrite/rewrite.go`: keep existing routing rewrite as the first downstream transform.
- `README.md`, `docs/architecture.md`: user semantics, compatibility floor, failure modes, and runtime artifacts.
- `internal/repository/workflows_test.go`: documentation contract for the new behavior.

### Task 1: JSON-RPC Message Model

**Files:**
- Create: `internal/transport/jsonrpc.go`
- Create: `internal/transport/jsonrpc_test.go`

- [ ] **Step 1: Write failing parser tests**

Cover integer and string ids, notifications, response errors, `thread/resume`
templates, `turn/start.params.threadId`, and `turn/completed.params.threadId`:

```go
func TestParseRPCMessageExtractsRoutingMetadata(t *testing.T) {
    tests := []struct {
        input    string
        kind     rpcKind
        method   string
        id       string
        threadID string
    }{
        {`{"id":7,"method":"turn/start","params":{"threadId":"thr-a","input":[]}}`, rpcRequest, "turn/start", "7", "thr-a"},
        {`{"id":"resume-1","method":"thread/resume","params":{"threadId":"thr-b"}}`, rpcRequest, "thread/resume", `"resume-1"`, "thr-b"},
        {`{"method":"turn/completed","params":{"threadId":"thr-a","turn":{"id":"turn-1"}}}`, rpcNotification, "turn/completed", "", "thr-a"},
        {`{"id":7,"result":{"thread":{"id":"thr-a"},"modelProvider":"sub2api"}}`, rpcResponse, "", "7", ""},
    }
    for _, tt := range tests {
        got, err := parseRPCMessage([]byte(tt.input))
        if err != nil { t.Fatal(err) }
        if got.kind != tt.kind || got.method != tt.method || got.idKey != tt.id || got.threadID != tt.threadID {
            t.Fatalf("parse = %#v", got)
        }
    }
}
```

Add malformed top-level, non-object params, missing turn thread id, and
non-string thread id cases. Errors must not contain input values.

- [ ] **Step 2: Run RED**

Run: `go test ./internal/transport -run 'TestParseRPCMessage' -v`

Expected: FAIL because `parseRPCMessage` and its types do not exist.

- [ ] **Step 3: Implement the bounded message model**

Define:

```go
type rpcKind uint8
const (
    rpcUnknown rpcKind = iota
    rpcRequest
    rpcNotification
    rpcResponse
)

type rpcMessage struct {
    kind      rpcKind
    method    string
    id        json.RawMessage
    idKey     string
    params    map[string]json.RawMessage
    result    map[string]json.RawMessage
    hasError  bool
    threadID  string
}

func parseRPCMessage(payload []byte) (rpcMessage, error)
func requireThreadID(message rpcMessage) (string, error)
func responseThreadProvider(message rpcMessage) (threadID, provider string, ok bool)
func encodeRPCRequest(id, method string, params map[string]json.RawMessage) ([]byte, error)
func encodeRPCError(id json.RawMessage, code int, message string) ([]byte, error)
```

Use structured JSON parsing only. `idKey` is the trimmed raw id bytes, so `7`
and `"7"` remain distinct. Accept unrelated valid JSON-RPC messages without
requiring object params; enforce object params only in the method-specific
helpers.

- [ ] **Step 4: Add response and error encoding tests**

Assert internal requests preserve raw params, generated errors preserve the
original numeric/string id, and no helper includes thread ids or payloads in
errors.

- [ ] **Step 5: Run GREEN and commit**

Run: `go test ./internal/transport -run 'Test(ParseRPCMessage|EncodeRPC)' -v`

Expected: PASS.

```bash
git add internal/transport/jsonrpc.go internal/transport/jsonrpc_test.go
git commit -m "feat: model app-server routing messages"
```

### Task 2: Distributed Peer Coordinator

**Files:**
- Create: `internal/handoff/coordinator.go`
- Create: `internal/handoff/coordinator_test.go`

- [ ] **Step 1: Write failing namespace and lock tests**

```go
func TestRuntimeDirectoryIsShortAndSocketSpecific(t *testing.T) {
    first := runtimeDirectory(1000, "/home/user/.codex/app-server-control/app-server-control.sock")
    second := runtimeDirectory(1000, "/tmp/other.sock")
    if first == second || !strings.HasPrefix(first, "/tmp/cps-1000-") || len(first) > 64 {
        t.Fatalf("runtime directories = %q, %q", first, second)
    }
}

func TestThreadLockSerializesAndHonorsCancellation(t *testing.T) {
    coordinator := testCoordinator(t)
    release, err := coordinator.LockThread(context.Background(), "thr-a")
    if err != nil { t.Fatal(err) }
    ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
    defer cancel()
    if _, err := coordinator.LockThread(ctx, "thr-a"); !errors.Is(err, context.DeadlineExceeded) {
        t.Fatalf("second lock error = %v", err)
    }
    release()
}
```

- [ ] **Step 2: Run RED**

Run: `go test ./internal/handoff -run 'Test(RuntimeDirectory|ThreadLock)' -v`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement namespace and `flock`**

Define:

```go
type PeerStatus string
const (
    StatusReady PeerStatus = "ready"
    StatusBusy PeerStatus = "busy"
    StatusUnsubscribed PeerStatus = "unsubscribed"
    StatusNotSubscribed PeerStatus = "notSubscribed"
    StatusNotLoaded PeerStatus = "notLoaded"
)

type Handler interface {
    Prepare(threadID string) PeerStatus
    Unsubscribe(context.Context, string) (PeerStatus, error)
}

type Coordinator struct {
    directory string
    socketPath string
    listener net.Listener
    handler Handler
    closeOnce sync.Once
    done chan struct{}
}

func Open(appServerSocket string, handler Handler) (*Coordinator, error)
func (c *Coordinator) LockThread(context.Context, string) (release func(), err error)
func (c *Coordinator) PrepareAll(context.Context, string) error
func (c *Coordinator) UnsubscribeAll(context.Context, string) error
func (c *Coordinator) Close() error
```

Derive names with SHA-256. Create directories 0700 and files 0600. Use
`syscall.Flock(LOCK_EX|LOCK_NB)` with a short timer loop so context cancellation
is observed. Never include thread ids in paths or returned errors.

- [ ] **Step 4: Write failing two-phase peer tests**

Open three coordinators in one namespace. Assert `PrepareAll` is side-effect
free, any busy peer aborts before all `Unsubscribe` counters remain zero, and a
ready set receives exactly one unsubscribe per live peer. Create a stale socket
entry and assert it is ignored and removed after connection refusal.

- [ ] **Step 5: Implement bounded peer RPC**

Use newline-delimited JSON with a 4 KiB `io.LimitReader`, one request per Unix
connection, and two-second dial/read/write deadlines. Request and response
structs contain only method, thread id, status, and a static error flag.
`PrepareAll` completes before `UnsubscribeAll` begins.

- [ ] **Step 6: Run GREEN, cross-build, and commit**

Run:

```text
go test ./internal/handoff -v
GOOS=linux GOARCH=amd64 go test ./internal/handoff
GOOS=darwin GOARCH=arm64 go test ./internal/handoff
```

Expected: all compile/tests pass; Darwin command compiles the test binary even
when it cannot execute on Linux by using `go test -c` if needed.

```bash
git add internal/handoff
git commit -m "feat: coordinate provider handoffs across proxies"
```

### Task 3: Stateful WebSocket Session

**Files:**
- Create: `internal/transport/session.go`
- Create: `internal/transport/session_test.go`
- Modify: `internal/transport/proxy.go`

- [ ] **Step 1: Write failing internal-response correlation tests**

Construct a session with fake write functions. Register an internal request,
deliver its response through `handleUpstreamText`, and assert the waiter receives
it while the downstream writer is not called. Deliver an ordinary Desktop
response and assert it is forwarded byte-for-byte.

- [ ] **Step 2: Run RED**

Run: `go test ./internal/transport -run 'TestSession(ConsumesInternalResponse|ForwardsDesktopResponse)' -v`

Expected: FAIL because `session` does not exist.

- [ ] **Step 3: Implement session state and serialized I/O**

Define:

```go
type session struct {
    provider string
    upstream, downstream *websocket.Conn
    coordinator *handoff.Coordinator
    upstreamWriteMu, downstreamWriteMu sync.Mutex
    stateMu sync.Mutex
    internal map[string]chan rpcMessage
    desktop map[string]desktopRequest
    resumeTemplates map[string]map[string]json.RawMessage
    effectiveProviders map[string]string
    activeThreads map[string]bool
    internalPrefix string
    sequence atomic.Uint64
}

type desktopRequest struct {
    method string
    threadID string
    responseSeen chan struct{}
}
```

Implement `callUpstream`, `writeUpstream`, `writeDownstream`,
`handleUpstreamText`, and state helpers. Internal calls use string ids and a
five-second timeout. Close all waiters when the session stops.

- [ ] **Step 4: Add active-state and resume-response tests**

Assert a forwarded `turn/start` marks active before write, a response error and
`turn/completed` clear it, `thread/resume` responses record returned provider,
and a peer unsubscribe clears effective-provider state.

- [ ] **Step 5: Implement downstream request flow and control Handler**

Implement:

```go
func (s *session) handleDownstreamText(context.Context, []byte) error
func (s *session) Prepare(threadID string) handoff.PeerStatus
func (s *session) Unsubscribe(context.Context, string) (handoff.PeerStatus, error)
func (s *session) handoff(context.Context, threadID string) error
func (s *session) internalResume(context.Context, threadID string) error
```

Run `rewrite.Line` first for existing provider/list policy. Ordinary
`thread/resume` holds the thread lock through its response. `turn/start` holds
the lock, checks effective provider, performs prepare/unsubscribe/internal
resume when needed, verifies the response provider, marks active, then writes
the original payload. Handoff failures write JSON-RPC error code `-32090` with
message `provider handoff unavailable; retry after the active turn finishes`
and do not terminate the WebSocket.

- [ ] **Step 6: Refactor `bridge` to run the session**

After both WebSockets are established, construct the session, open its
coordinator, and run concurrent downstream/upstream pumps. Preserve existing
close propagation, message limits, binary pass-through, ping/pong behavior,
and sanitized routing-policy closes.

- [ ] **Step 7: Run transport tests and commit**

Run: `go test ./internal/transport -v`

Expected: existing transport tests and new session tests pass.

```bash
git add internal/transport internal/handoff
git commit -m "feat: switch provider before the next turn"
```

### Task 4: Multi-Connection App-Server Integration

**Files:**
- Modify: `internal/transport/proxy_test.go`

- [ ] **Step 1: Build a fake loaded-thread server**

Add a test server that accepts multiple WebSocket connections and implements
`initialize`, `thread/resume`, `thread/unsubscribe`, and `turn/start`. Its
shared state tracks effective provider, subscribers, and active status. A
different-provider resume switches only when subscriber count is zero and the
thread is idle, matching 0.146.0.

- [ ] **Step 2: Write the A-to-B-to-A end-to-end test**

Connect two real `transport.Run` proxies with providers `openai` and `sub2api`.
Initialize both, resume the same `thr-shared`, and assert B initially receives
`modelProvider: openai`. Send B's `turn/start`; assert the fake server records
`sub2api` and the same thread id. Emit `turn/completed`, then send A's next turn
and assert `openai`.

- [ ] **Step 3: Run the end-to-end contract**

Run: `go test ./internal/transport -run TestRunSwitchesProviderOnNextTurn -v`

Expected: PASS. The session and coordinator behavior reached RED then GREEN in
Tasks 2 and 3; this test proves those units compose across real WebSockets and
multiple Unix connections.

- [ ] **Step 4: Add active and non-cooperating subscriber cases**

Keep A active and assert B receives JSON-RPC error `-32090`, no B turn reaches
the fake server, and A is not unsubscribed. Add a raw app-server subscriber not
represented by a switcher peer; assert B's internal resume reports `openai` and
B's turn remains blocked.

- [ ] **Step 5: Verify no internal protocol leakage**

Collect all Desktop-visible messages and assert no id starts with the internal
prefix and no `thread/unsubscribe` request/response appears. Retain 101,
fragmentation, binary, ping/pong, close, short-I/O, and 64 MiB coverage.

- [ ] **Step 6: Run related suites and commit**

Run:

```text
go test ./internal/transport ./internal/handoff ./internal/rewrite
go test -race ./internal/transport ./internal/handoff
```

Expected: PASS on x86 race runners; document the local ARM VMA limitation.

```bash
git add internal/transport internal/handoff
git commit -m "test: cover cross-provider thread handoff"
```

### Task 5: Documentation, Verification, And Release

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `internal/repository/workflows_test.go`

- [ ] **Step 1: Write failing documentation contract**

Require the combined README/architecture text to contain `send-time provider
handoff`, `turn/start`, `Codex CLI 0.146.0`, `active turn`, `-32090`, and
`/tmp/cps-`, then run `go test ./internal/repository -run TestDocumentation -v`
and verify RED.

- [ ] **Step 2: Document exact semantics**

Explain that opening a task does not switch it; the next send does. Document
idle-only switching, active/non-switcher failure, one-daemon/no-database-write
properties, runtime socket cleanup, and the 0.146.0 compatibility floor.

- [ ] **Step 3: Run complete local gates**

```text
go mod tidy
git diff --exit-code -- go.mod go.sum
test -z "$(gofmt -l cmd internal)"
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/cps-handoff-linux-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/cps-handoff-linux-arm64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o /tmp/cps-handoff-darwin-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/cps-handoff-darwin-arm64 ./cmd/codex-provider-switcher
```

- [ ] **Step 4: Request independent review**

Review against
`docs/superpowers/specs/2026-08-01-send-time-provider-handoff-design.md`.
Fix every Critical and Important issue with a failing regression test.

- [ ] **Step 5: Publish the patch**

Push the feature branch, create a PR, wait for Ubuntu race, macOS tests, and all
four cross-builds, then merge. Tag the merged main with the next patch version,
wait for Release, and verify all four archives and checksums.

### Task 6: Resynchronize Peers And Close Same-Provider Race

**Files:**
- Modify: `internal/handoff/coordinator.go`
- Modify: `internal/handoff/coordinator_test.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/handoff_integration_test.go`

- [ ] **Step 1: Reproduce stale peer subscriptions**

Make the fake app-server broadcast turn notifications to all subscribed
connections. After B switches a shared thread, assert both A and B remain
subscribed and A receives B's `turn/started` and `turn/completed`. Verify RED
because v0.3.0 leaves only B subscribed.

- [ ] **Step 2: Reproduce same-provider cross-session concurrency**

Delay the fake server's first `turn/started` broadcast. Start a turn through
openai connection A, then immediately start through openai connection B.
Assert B receives switcher error `-32090` and only one request reaches the fake
app-server. Verify RED because v0.3.0 skips peer prepare on the same-provider
path.

- [ ] **Step 3: Add the resubscribe control phase**

Extend the bounded peer protocol with
`{method:"resubscribe",threadId:"...",provider:"..."}`. Track coordinator
detach state per session, internally resume only detached peers with the new
effective provider, verify each response, and clear detach state only after a
successful reattach.

- [ ] **Step 4: Prepare every turn**

Call `PrepareAll` for every `turn/start` while holding the thread lock. Keep
unsubscribe, cold resume, and peer resubscribe conditional on provider
mismatch. Re-run the focused integration tests and verify GREEN.

- [ ] **Step 5: Verify and release**

Run formatting, module consistency, `go test ./...`, `go vet ./...`, repeated
handoff tests, and four CGO-disabled cross-builds. Push a PR, require GitHub x86
race and macOS checks, merge, publish the next patch release, validate all
archives/checksums/notices, then atomically update the installed wrapper.

### Task 7: Recover Every Failed Handoff Globally

**Files:**
- Modify: `internal/handoff/coordinator.go`
- Modify: `internal/handoff/coordinator_test.go`
- Modify: `internal/transport/session.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/handoff_integration_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`

- [ ] **Step 1: Reproduce resume-failure detachment**

Add two switcher peers plus one non-cooperating subscriber. Force the requested
provider verification to fail and assert every original subscriber is restored
before the rejected turn returns.

- [ ] **Step 2: Reproduce cross-session partial recovery**

Make one peer rejoin and a later peer fail. Send next through the successful
same-provider peer and assert it performs a complete handoff rather than the
fast path.

- [ ] **Step 3: Add global dirty state**

Create, inspect, and clear a mode-0600 per-thread dirty marker under the existing
runtime directory. Mark before unsubscribe and clear only after complete
success. Dirty state must be visible to every coordinator for the same daemon.

- [ ] **Step 4: Add best-effort restore**

Extend the control protocol with `restore`, visit every peer even after an
error, and have detached sessions resume with only `threadId`, accepting the
actual returned provider. Run restore on unsubscribe, sender resume, resubscribe,
or dirty-clear failures.

- [ ] **Step 5: Bump capability and verify**

Use `prepareHandoffV2` so older live peers reject before mutation. Run all tests,
vet, 20 repeated recovery tests, four cross-builds, GitHub race/macOS CI, and
publish the next patch release.
