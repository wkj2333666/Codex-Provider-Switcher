# Codex Provider Switcher WebSocket Transport Design

## Status

This document supersedes the original v0.1.0 design. The original design
incorrectly treated the standard input of `codex app-server proxy --sock` as
JSONL. That command is a byte-for-byte stdio-to-Unix-socket bridge, while the
Unix socket carries an HTTP Upgrade followed by RFC 6455 WebSocket frames.

The v0.1.0 implementation therefore fails on Desktop's first HTTP request line
and must not be presented as usable with Desktop Remote SSH. The corrected
design terminates and re-establishes WebSocket connections so JSON-RPC message
payloads can be rewritten at the correct protocol layer.

## Summary

Codex Provider Switcher is a provider-agnostic WebSocket proxy for Codex
Desktop Remote SSH connections. Multiple SSH entry points can select different
Codex model providers while sharing one app-server daemon, one `CODEX_HOME`,
and one task database.

For each Desktop connection, the switcher accepts the WebSocket carried over
stdin/stdout, opens another WebSocket to the existing app-server Unix socket,
and rewrites selected client-to-server JSON-RPC text messages. Server messages
and non-target client messages pass through unchanged.

The same binary can be installed as a transparent executable named `codex`.
In that mode it intercepts only `codex app-server proxy --sock ...`, which is
the command used for the Desktop-to-existing-socket path. Every other command
is delegated to the real Codex executable.

The project is an independent community tool distributed under the MIT
License. It is not an OpenAI product.

## Problem

Separate `CODEX_HOME` directories isolate provider configuration and
credentials, but they also create separate task stores. Pointing multiple
app-server daemons at shared SQLite files introduces multiple writers and can
cause startup failures or runtime lock contention.

Codex app-server protocol v2 provides a safer boundary. `thread/start`,
`thread/resume`, and `thread/fork` accept a `modelProvider` override, while
`thread/list` accepts a `modelProviders` filter. A connection-specific proxy
can set these fields before one shared daemon handles the requests.

The policy is simple, but the transport is not JSONL at the interception point:

```text
Desktop
  -> HTTP WebSocket Upgrade over SSH stdio
  -> masked WebSocket frames
  -> codex app-server proxy
  -> byte copy to Unix socket
  -> app-server WebSocket endpoint
```

Rewriting must happen after WebSocket message decoding and before encoding the
upstream WebSocket message.

## Goals

- Route each proxied Desktop connection through an explicitly selected model
  provider.
- Let all connections use the same app-server daemon and task store.
- Show tasks from every provider in each proxied connection.
- Preserve persisted task context when a task is resumed through another
  provider.
- Work with stock Desktop Remote SSH by providing a transparent remote `codex`
  executable entry point.
- Correctly handle masking, all payload-length encodings, fragmented messages,
  ping, pong, close, large messages, and partial I/O.
- Fail closed when routing identity is missing or a target request cannot be
  safely rewritten.
- Remain provider-agnostic and contain no private endpoints, credentials,
  usernames, hostnames, or machine-specific paths.
- Build one static Go binary for common Linux and macOS architectures.

## Non-Goals

- Starting, stopping, bootstrapping, or supervising the app-server daemon.
- Reading, copying, repairing, or synchronizing Codex SQLite databases.
- Editing SSH daemon configuration or silently modifying a user's shell setup.
- Storing, forwarding to logs, or inspecting API keys.
- Adding providers to Codex configuration.
- Synthesizing or rewriting `model/list` responses in the first corrected
  release.
- Supporting concurrent work through different providers on the same loaded
  task.
- Implementing RFC 6455 framing from scratch.
- Supporting native Windows Unix-socket connections.

## Architecture

Each Desktop Remote SSH connection starts one switcher process. The process
receives a provider identity, accepts exactly one downstream connection over
stdin/stdout, and connects directly to the shared app-server Unix socket.

```text
Desktop connection A -> switcher(provider-a) --+
                                                +-> one app-server daemon
Desktop connection B -> switcher(provider-b) --+       |
                                                        +-> one CODEX_HOME
                                                        +-> one task database
```

The transport path is:

```text
Desktop stdin/stdout
  -> single-connection net.Conn adapter
  -> HTTP Upgrade and downstream WebSocket accept
  -> JSON-RPC message policy
  -> upstream WebSocket dial over Unix socket
  -> app-server
```

The switcher no longer starts `codex app-server proxy`. That command cannot be
used as an upstream child because it copies encoded WebSocket bytes and offers
no message boundary at which JSON can be rewritten.

### Downstream Connection

stdin and stdout are wrapped as a single full-duplex `net.Conn`. A
single-connection `net.Listener` lets `net/http` parse the request and perform
the HTTP Upgrade. The listener accepts one connection only; additional HTTP
requests are out of scope.

The request must be a valid WebSocket upgrade. Plain HTTP requests and malformed
upgrades are rejected without opening the app-server socket.

### Upstream Connection

The switcher dials a WebSocket URL whose path and query match the downstream
request. A custom `http.Transport.DialContext` opens the configured Unix socket
instead of a TCP connection. The URL host is a fixed local placeholder and is
never used for network routing.

End-to-end request headers are forwarded after removing headers that the
WebSocket client library must generate itself, including `Connection`,
`Upgrade`, `Host`, `Sec-WebSocket-Key`, `Sec-WebSocket-Version`, and
`Sec-WebSocket-Extensions`. This preserves authentication and compatible
future headers without replaying invalid hop-by-hop handshake state.

Requested WebSocket subprotocols are offered upstream. If the app-server
selects one, the same subprotocol is selected for the downstream connection.
The upstream handshake completes before the downstream `101 Switching
Protocols` response is sent. An upstream handshake failure produces a bounded,
body-free downstream error and a non-zero process exit.

### WebSocket Library

The implementation uses `github.com/coder/websocket`, pinned in `go.mod` to a
reviewed release. It has context-aware client and server APIs, close and
ping/pong handling, and passes the Autobahn WebSocket testsuite.

Using a mature implementation is a protocol requirement, not an optional
convenience. The switcher must not maintain its own masking, fragmentation, or
payload-length parser.

### Compression

`permessage-deflate` is disabled on both downstream accept and upstream dial.
The switcher strips `Sec-WebSocket-Extensions` before the upstream handshake
and does not advertise an extension downstream. A client may request
compression, but the `101` response must not negotiate it.

This keeps each text payload available as ordinary JSON and avoids having to
translate compression context between independent WebSocket connections.

## Invocation Modes

### Direct Proxy Mode

Direct mode is intended for tests and integrations that can choose their
command explicitly:

```text
codex-provider-switcher proxy --provider <id> --socket <path>
```

Configuration environment variables are:

```text
CODEX_PROVIDER_SWITCHER_PROVIDER
CODEX_PROVIDER_SWITCHER_SOCKET
CODEX_PROVIDER_SWITCHER_CODEX
```

Flags take precedence over environment values. `--provider` and `--socket`
are required after resolution. The socket is resolved to an absolute path and
must exist as a Unix socket before the downstream handshake is accepted.

### Transparent Codex Wrapper Mode

When the executable basename is `codex`, it behaves as a transparent wrapper:

- `codex app-server proxy --sock <path>` is intercepted and served by the
  switcher WebSocket proxy.
- The `--sock=<path>` spelling is also accepted.
- Missing provider identity, missing socket, duplicate socket options, and
  unknown proxy options fail closed. They are never delegated to the real
  command because delegation would bypass routing policy.
- Every command not matching `app-server proxy` is delegated unchanged to the
  real Codex executable with `exec`, preserving arguments, environment,
  standard streams, signals, and exit status.

The real Codex executable is resolved in this order:

1. Absolute executable path in `CODEX_PROVIDER_SWITCHER_CODEX`.
2. A `codex` candidate later in `PATH` that is not the switcher itself.
3. Startup failure with an actionable diagnostic.

Candidate paths are canonicalized and compared by file identity so a symlink
or hard link cannot create recursive wrapper execution.

The supported stock Desktop installation is a deliberate, documented remote
setup: put the switcher in the login shell's effective `PATH` under the name
`codex`, keep the real Codex executable available later in `PATH` or configure
its absolute path, and provide `CODEX_PROVIDER_SWITCHER_PROVIDER` for that SSH
entry point. For per-alias environment selection with OpenSSH `SetEnv`, the
server must allow that variable with `AcceptEnv`; otherwise the provider must
be set by an equivalent server-side wrapper or account environment.

The README must state these prerequisites explicitly. It must not claim that
Desktop has a custom proxy-command setting.

## Provider Selection

The selected provider is resolved in this order:

1. `--provider <id>` in direct proxy mode.
2. `CODEX_PROVIDER_SWITCHER_PROVIDER`.
3. Startup failure when neither is present.

Provider identifiers must be non-empty and match `[A-Za-z0-9._-]+`. The
switcher has no provider allowlist and does not special-case provider names.

Failing when the provider is absent prevents an environment propagation error
from silently routing work through an unintended account or billing path.

## Message Bridge

After both handshakes succeed, two concurrent pumps run until one direction
closes or fails.

### Client To Server

- Text messages are read as complete WebSocket messages. The library
  reassembles fragmented frames before the rewrite function sees the payload.
- Target JSON-RPC messages are rewritten and written upstream as one text
  message. Frame boundaries are not preserved because they have no JSON-RPC
  semantics.
- Valid non-target text messages are forwarded without semantic modification.
- Binary messages are passed through as binary messages without inspection.
- Malformed JSON text and unsafe target requests terminate the connection
  rather than being forwarded.

### Server To Client

- Text and binary messages are forwarded without parsing or rewriting.
- Message type and payload bytes are preserved; frame boundaries may differ.

### Control Frames And Close

The WebSocket library handles masking, ping, pong, and fragmented control-flow
details. A received close status and reason are propagated to the opposite
connection when valid. Cancellation or a non-close transport error closes both
connections with an internal-error or going-away status as appropriate.

The first pump to finish cancels the other. Shutdown is bounded; the process
must not remain blocked forever waiting for a peer that stopped reading.

### Size Limits And Partial I/O

Both connections use an explicit 64 MiB message read limit. This is large
enough for normal app-server traffic while bounding memory consumed by one
message. Exceeding the limit closes the connection with `StatusMessageTooBig`
and exits non-zero.

The stdio adapter and Unix dialer must support short reads and writes. No code
may assume that one operating-system read corresponds to one HTTP line, frame,
or WebSocket message.

## JSON-RPC Rewrite Policy

Only downstream WebSocket text-message payloads are candidates for parsing.
Each text message must contain one JSON-RPC object, matching app-server's
WebSocket transport contract.

For the selected provider `provider-a`, requests are rewritten as follows.

### Start, Resume, And Fork

For `thread/start`, `thread/resume`, and `thread/fork`, the switcher creates a
missing `params` object and unconditionally sets:

```json
{"modelProvider":"provider-a"}
```

An existing `modelProvider` value is overwritten. The SSH entry point is the
routing-policy authority, so a conflicting client value must not bypass it.

### List

For `thread/list`, the switcher creates a missing `params` object and
unconditionally sets:

```json
{"modelProviders":[]}
```

Protocol v2 defines an empty list as including all providers. Every proxied
connection therefore receives the same task inventory.

### Other Messages

- `turn/start` is not rewritten. It inherits the provider selected when the
  thread was started or resumed.
- `model/list` is not rewritten in the first corrected release.
- Valid requests and notifications with other method names pass through.
- Upstream responses and notifications pass through without JSON parsing.

Re-encoding a rewritten JSON object may change whitespace or object key order.
JSON semantics, request IDs, and all unrelated fields must be preserved.

## Task And Concurrency Semantics

An existing task may be used sequentially through different providers:

```text
resume with provider-a -> complete turn -> leave task
resume with provider-b -> continue from the same persisted history
```

The switcher chooses the provider for new work. Context still comes from the
single app-server's persisted thread history.

Different tasks may be active through different switcher connections at the
same time. The same task must not be actively operated through different
providers at the same time. Provider selection belongs to the loaded thread
runtime, not permanently to one client connection, so competing overrides on
one loaded task are ambiguous and unsupported.

## Lifecycle And Errors

Startup proceeds in this order:

1. Detect direct or transparent-wrapper mode.
2. Parse and validate configuration.
3. Validate the provider and Unix socket.
4. Parse the downstream HTTP Upgrade request.
5. Establish the upstream WebSocket over the Unix socket.
6. Accept the downstream WebSocket using the upstream subprotocol result.
7. Run both message pumps until close, cancellation, or error.

`SIGINT`, `SIGTERM`, and `SIGHUP` cancel the proxy context and initiate bounded
WebSocket shutdown. There is no child process in intercepted proxy mode and no
daemon to reap. Delegated commands replace the wrapper process through `exec`.

Diagnostics use stderr and contain only error categories, method names,
connection phases, and process status. They never include full JSON payloads,
prompts, header values, environment values, or credentials.

The switcher fails closed for:

- Missing or invalid provider identity.
- Missing, invalid, or unreachable Unix socket.
- Invalid HTTP Upgrade or failed upstream handshake.
- Invalid JSON in a downstream text message.
- A target method with a non-object, non-null `params` value.
- A message larger than the configured hard limit.
- Read, write, close, or cancellation failures that prevent clean forwarding.
- Wrapper recursion or inability to resolve the real Codex executable.

A target method with missing or null `params` receives a new object. A valid
unknown method passes through unchanged.

## Security And Privacy

- Provider values are JSON-encoded and never interpolated into a shell.
- The wrapper delegates with an argument vector and no shell evaluation.
- Unix socket paths are passed to the dialer as data, not command text.
- API keys remain in Codex configuration or the daemon environment.
- Hop-by-hop and generated WebSocket headers are not replayed upstream.
- `permessage-deflate` is not negotiated on either connection.
- Logs never retain or format request bodies, prompts, authorization headers,
  cookies, or environment contents.
- The repository contains only generic example names and `example.com`
  endpoints.

## Compatibility

The corrected implementation targets Go 1.24 and Linux and macOS on amd64 and
arm64. Runtime proxying requires Unix-domain sockets, so native Windows is out
of scope.

The protocol behavior is based on Codex app-server protocol v2. The required
fields are present in the schema generated by Codex CLI 0.146.0:

- `ThreadStartParams.modelProvider`
- `ThreadResumeParams.modelProvider`
- `ThreadForkParams.modelProvider`
- `ThreadListParams.modelProviders`

The implementation must not depend on undocumented response fields. The
transport contract is one JSON-RPC message per WebSocket text message.

Provider and model compatibility still belongs to Codex and the configured
upstream. The switcher cannot make an incompatible model implement Codex tool
calls or Responses semantics.

## References

- OpenAI Codex `stdio-to-uds` implementation:
  <https://github.com/openai/codex/blob/main/codex-rs/stdio-to-uds/src/lib.rs>
- OpenAI app-server protocol and transport documentation:
  <https://learn.chatgpt.com/docs/app-server#protocol>
- OpenAI Desktop Remote SSH connection documentation:
  <https://learn.chatgpt.com/docs/remote-connections#connect-to-an-ssh-host>
- Coder WebSocket implementation and protocol-test status:
  <https://github.com/coder/websocket>

## Test Strategy

### Unit Tests

Unit tests cover:

- Flag and environment precedence.
- Direct-mode and wrapper-mode dispatch.
- Missing and invalid provider rejection.
- Socket validation.
- Real-Codex resolution and recursion prevention.
- Provider injection for start, resume, and fork.
- All-provider list rewriting.
- Creation of missing or null `params` objects.
- Rejection of non-object `params` on target methods.
- Semantic preservation of IDs and unrelated fields.
- Pass-through of valid unknown methods.
- Rejection of malformed JSON.

### Required End-To-End Transport Test

The primary integration test uses a Unix-socket WebSocket test server and a
raw simulated Desktop client:

```text
masked Desktop WebSocket client
  -> HTTP Upgrade over switcher stdin/stdout
  -> switcher
  -> Unix socket WebSocket test server
```

It must prove all of the following:

- The downstream client receives `101 Switching Protocols`.
- A masked text message reaches the Unix-socket server as a valid unmasked
  application payload with the expected provider rewrite.
- A server text message reaches the client unchanged.
- Payload sizes using RFC 6455 7-bit, 16-bit, and 64-bit length encodings work.
- A fragmented client text message is reassembled and rewritten.
- Ping receives pong and does not enter the JSON rewrite path.
- Close status and reason propagate without hanging either pump.
- A requested `permessage-deflate` extension is absent from both negotiated
  connections.
- Binary messages pass through without parsing.
- Messages above 64 KiB and below the 64 MiB limit work.
- Deliberately short reads and writes do not corrupt the handshake or messages.

The test must not replace the Unix side with an echoing JSONL child. CI need
not install Codex because the UDS WebSocket server exercises the actual
transport boundary. An opt-in local smoke test may additionally target a real
running Codex app-server socket.

### Wrapper Integration Tests

Wrapper tests verify that the exact app-server proxy command is intercepted,
unknown proxy flags fail closed, other commands are delegated with unchanged
arguments and exit status, and symlink-based recursion is rejected.

### Verification Commands

```text
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./cmd/codex-provider-switcher
GOOS=linux GOARCH=arm64 go build ./cmd/codex-provider-switcher
GOOS=darwin GOARCH=amd64 go build ./cmd/codex-provider-switcher
GOOS=darwin GOARCH=arm64 go build ./cmd/codex-provider-switcher
```

GitHub Actions runs formatting checks, tests, the race detector, vet, and all
four cross-builds.

## Release And Migration

The existing v0.1.0 release is protocol-incompatible with Desktop and must be
clearly marked as broken in its GitHub release notes. Its artifacts remain for
auditability; users must be directed to the first corrected release.

The corrected implementation is released as v0.2.0 after the end-to-end
WebSocket test and a real-socket smoke test pass. Pushing a `v*` tag runs the
release workflow, which validates the tag and builds static binaries for:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`

Each target is packaged as a versioned `.tar.gz` containing the binary,
README, and license, plus a matching SHA-256 checksum. The publish job alone
receives `contents: write`; third-party actions remain pinned to commit SHAs.

README migration guidance must remove the obsolete JSONL architecture, explain
why v0.1.0 cannot work, document transparent-wrapper installation for stock
Desktop, and include a reversible uninstall path.

## Repository Layout

```text
codex-provider-switcher/
├── cmd/codex-provider-switcher/main.go
├── internal/config/config.go
├── internal/config/config_test.go
├── internal/rewrite/rewrite.go
├── internal/rewrite/rewrite_test.go
├── internal/transport/proxy.go
├── internal/transport/proxy_test.go
├── internal/wrapper/wrapper.go
├── internal/wrapper/wrapper_test.go
├── docs/
│   ├── architecture.md
│   └── superpowers/specs/2026-07-31-codex-provider-switcher-design.md
├── .github/workflows/
│   ├── ci.yml
│   └── release.yml
├── .gitignore
├── go.mod
├── LICENSE
└── README.md
```

The public command owns mode selection and exit behavior. Internal packages
separately own configuration, JSON-RPC policy, WebSocket transport, and real
Codex delegation.

## Acceptance Criteria

- A stock Desktop Remote SSH connection can reach the switcher through a remote
  executable named `codex` without a Desktop custom-command feature.
- Desktop's HTTP Upgrade receives a valid `101` only after the upstream Unix
  socket WebSocket is ready.
- Two switcher processes with different provider values can connect to the same
  app-server control socket.
- Each process injects only its configured provider into thread start, resume,
  and fork requests.
- Both processes request an unfiltered task list.
- Server messages and binary messages are not inspected or rewritten.
- Masking, all payload lengths, fragmentation, ping/pong, close, large
  messages, and partial I/O pass the end-to-end tests.
- `permessage-deflate` is not negotiated.
- No second app-server daemon or SQLite writer is created by the switcher.
- Missing routing identity cannot fall back silently to the official proxy.
- Non-proxy Codex commands delegate without wrapper recursion.
- Tests, the race detector, vet, and Linux/macOS amd64/arm64 builds pass.
- A v0.2.0 tag creates a public GitHub Release containing four archives and
  matching SHA-256 files, and v0.1.0 is visibly marked broken.
- Documentation contains no private infrastructure or credentials and clearly
  states the stock Desktop setup and unsupported same-task concurrency case.
