# Codex Provider Switcher Design

## Summary

Codex Provider Switcher is a small, provider-agnostic JSON-RPC proxy for Codex
Desktop Remote SSH connections. It lets multiple SSH entry points select
different Codex model providers while all connections use one app-server
daemon, one `CODEX_HOME`, and one task database.

The switcher wraps `codex app-server proxy`, rewrites selected client requests,
and leaves server responses untouched. It does not start the daemon, access
SQLite directly, manage credentials, or modify a user's shell and SSH
configuration.

The project is an independent community tool distributed under the MIT License.
It is not an OpenAI product.

## Problem

Using separate `CODEX_HOME` directories makes it easy to isolate provider
configuration and credentials, but it also creates separate task stores. Making
those homes share SQLite files allows both environments to see the same tasks,
but two app-server daemons then become concurrent writers to one SQLite/WAL
database. Codex app-server startup and runtime behavior is not designed around
that topology, so lock contention can prevent one daemon from starting or make
the setup unstable.

Codex app-server protocol v2 provides a better boundary. `thread/start`,
`thread/resume`, and `thread/fork` accept a `modelProvider` override, while
`thread/list` accepts a `modelProviders` filter. A connection-specific proxy can
set these fields before a single daemon receives the request.

## Goals

- Route each proxied Desktop connection through an explicitly selected model
  provider.
- Let all connections use the same app-server daemon and task store.
- Show tasks from every provider in each proxied connection.
- Preserve existing task context when a task is resumed through a different
  provider.
- Remain provider-agnostic and contain no private endpoints, credentials,
  usernames, hostnames, or machine-specific paths.
- Ship as one Go binary using only the Go standard library.
- Fail closed when routing identity is missing or a target request cannot be
  safely rewritten.
- Preserve protocol compatibility by passing unknown valid JSON-RPC messages
  through unchanged.

## Non-Goals

- Starting, stopping, bootstrapping, or supervising the app-server daemon.
- Reading, copying, repairing, or synchronizing Codex SQLite databases.
- Installing a wrapper at `~/.local/bin/codex`.
- Editing SSH, Bash, Fish, Codex, or system service configuration.
- Storing, forwarding, inspecting, or logging API keys.
- Adding providers to Codex configuration.
- Synthesizing or rewriting `model/list` responses in the first release.
- Supporting concurrent work through different providers on the same loaded
  task.
- Publishing release binaries in the first implementation milestone.

## Architecture

Each Desktop Remote SSH connection starts a separate switcher process. The
process receives its provider identity from a command-line flag or environment
variable, then starts the real Codex stdio proxy against an explicitly selected
shared Unix socket.

```text
Desktop connection A -> switcher(provider-a) --+
                                                +-> one app-server daemon
Desktop connection B -> switcher(provider-b) --+       |
                                                        +-> one CODEX_HOME
                                                        +-> one task database
```

The switcher owns two data paths:

```text
Desktop stdin -> parse and rewrite JSONL -> child stdin
child stdout  -> byte-for-byte copy       -> Desktop stdout
```

The child command is always:

```text
<codex> app-server proxy --sock <socket>
```

The switcher never opens the Unix socket itself. This keeps Codex-specific
transport behavior in the official `codex app-server proxy` implementation and
limits the project to JSON-RPC policy and process lifecycle management.

## Operating Assumptions

- One app-server daemon is already running before the switcher starts.
- Every provider ID selected by a switcher is registered in that daemon's
  effective Codex configuration.
- Provider credentials are available to the daemon through Codex auth storage,
  provider environment keys, or another Codex-supported credential source.
- Providers implement the Responses wire API behavior required by Codex.
- The selected model name is accepted by the target provider. Because the first
  release does not rewrite `model/list`, it does not add custom model names to
  the Desktop model picker.
- All switcher processes connect to the same app-server control socket.

## Provider Selection

The selected provider is resolved in this order:

1. `--provider <id>`
2. `CODEX_PROVIDER_SWITCHER_PROVIDER`
3. Startup failure when neither is present

Provider identifiers must be non-empty and match `[A-Za-z0-9._-]+`. The
switcher does not maintain an allowlist and does not special-case provider
names.

Failing when the provider is absent prevents a missing SSH environment variable
from silently routing a task through an unintended account or billing path.

## Command-Line Interface

```text
codex-provider-switcher [options]

  --provider <id>  Provider to inject into task requests
  --socket <path>  Shared app-server Unix socket
  --codex <path>   Codex executable or command name; defaults to codex
  --version        Print the switcher version and exit
  --help           Print usage and exit
```

Configuration environment variables are:

```text
CODEX_PROVIDER_SWITCHER_PROVIDER
CODEX_PROVIDER_SWITCHER_SOCKET
CODEX_PROVIDER_SWITCHER_CODEX
```

Command-line values take precedence over environment values. The socket is
required. Before starting the child, the switcher resolves the socket to an
absolute path and verifies that it exists and is a Unix socket. An explicit
Codex path is resolved to an absolute executable path; a command name is
resolved with the current `PATH`.

## JSON-RPC Rewrite Policy

Input from Desktop is newline-delimited JSON. The switcher reads complete lines
without `bufio.Scanner` so messages larger than 64 KiB remain supported.

For the selected provider `provider-a`, requests are rewritten as follows.

### Start, Resume, And Fork

For `thread/start`, `thread/resume`, and `thread/fork`, the switcher creates a
missing `params` object and unconditionally sets:

```json
{"modelProvider":"provider-a"}
```

An existing `modelProvider` value is overwritten. The SSH entry point is the
routing policy authority, so a conflicting client value must not bypass it.

### List

For `thread/list`, the switcher creates a missing `params` object and
unconditionally sets:

```json
{"modelProviders":[]}
```

Protocol v2 defines an empty list as including all providers. This gives every
proxied connection the same task inventory.

### Other Messages

- `turn/start` is not rewritten. It inherits the provider selected when the
  thread was started or resumed.
- `model/list` is not rewritten in the first release.
- Valid requests and notifications with other method names are forwarded
  unchanged.
- Child responses and notifications are copied without parsing or rewriting.

Re-encoding a rewritten JSON object may change whitespace or object key order.
JSON semantics, request IDs, and all fields not named above must be preserved.

## Task And Concurrency Semantics

An existing task may be used sequentially through different providers:

```text
resume with provider-a -> complete turn -> leave task
resume with provider-b -> continue from the same persisted history
```

The switcher only chooses the provider for new work. Context still comes from
the single app-server's persisted thread history.

Different tasks may be active through different switcher connections at the
same time. The same task must not be actively operated through different
providers at the same time. Provider selection belongs to the loaded thread
runtime, not permanently to one client connection, so competing overrides on
one loaded task are ambiguous and unsupported.

## Process Lifecycle

Startup proceeds in this order:

1. Parse flags and environment variables.
2. Validate the provider.
3. Resolve the Codex executable and shared socket.
4. Start `codex app-server proxy --sock <socket>`.
5. Start client-to-child rewriting and child-to-client copying.

The child inherits stderr so Codex diagnostics remain visible. Switcher
diagnostics also use stderr and contain only error categories, method names,
line numbers, and process status. They never include the full JSON line, user
prompts, environment values, or credentials.

When Desktop stdin reaches EOF, the switcher closes child stdin, continues
copying remaining child output, and waits for the child to exit. `SIGINT`,
`SIGTERM`, and `SIGHUP` are forwarded to the child process. The switcher returns
the child's exit code when the child exits normally or with a status code.

If either forwarding direction fails, the switcher terminates the child,
waits for it, and exits non-zero. It must not leave an orphaned Codex proxy.

## Error Handling

The switcher fails before child startup when configuration is missing or
invalid. During forwarding it uses these rules:

- Invalid JSON input: report the input line number and terminate.
- A target method with a non-object, non-null `params` value: report the method
  and terminate rather than forwarding an unmodified routing request.
- A target method with missing or null `params`: create an object and rewrite.
- A valid unknown method: forward it unchanged.
- Child startup failure: report the executable and error category without
  dumping the environment.
- Child write, read, or wait failure: terminate remaining work and exit
  non-zero.

This policy favors an obvious disconnected client over a connection that might
silently use the wrong provider.

## Security And Privacy

- Provider values are JSON-encoded, never interpolated into a shell command.
- The child is started with `os/exec` argument arrays; no shell is involved.
- The socket path and executable are supplied as distinct arguments.
- API keys remain in Codex provider configuration or the daemon environment.
- The switcher does not inspect child responses, request prompt content, or
  authentication traffic.
- Logs redact message bodies by never retaining or formatting them into
  diagnostic strings.
- The repository contains only generic example names and `example.com`
  endpoints.

## Compatibility

The first implementation targets Go 1.24 and Linux on amd64 and arm64. The
process and stream design is also compatible with macOS, and tests should run
there, but Unix-domain socket validation makes native Windows out of scope.

The protocol behavior is based on Codex app-server protocol v2. The required
fields are present in the schema generated by Codex CLI 0.146.0:

- `ThreadStartParams.modelProvider`
- `ThreadResumeParams.modelProvider`
- `ThreadForkParams.modelProvider`
- `ThreadListParams.modelProviders`

The implementation must not depend on undocumented response fields. CI tests
the project's rewrite contract; maintainers should regenerate the schema when
upgrading the supported Codex version.

The protocol permits provider overrides, but provider and model compatibility
still belongs to Codex and the configured upstream. The switcher cannot make an
incompatible model implement Codex tool calls or Responses semantics.

## Test Strategy

Unit tests cover:

- Flag and environment precedence.
- Missing and invalid provider rejection.
- Executable and socket validation.
- Provider injection for start, resume, and fork.
- All-provider list rewriting.
- Creation of missing or null `params` objects.
- Rejection of non-object `params` on target methods.
- Semantic preservation of IDs and unrelated fields.
- Pass-through of valid unknown methods.
- Rejection of malformed JSON.
- Rewriting of messages larger than 64 KiB.

Integration tests run the proxy against a test-helper child process and cover:

- Bidirectional stream forwarding.
- Child stderr behavior.
- Desktop stdin EOF and child stdin closure.
- Child exit-code propagation.
- Signal forwarding on Unix.
- Child cleanup after a forwarding error.

Required verification commands are:

```text
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./cmd/codex-provider-switcher
GOOS=linux GOARCH=arm64 go build ./cmd/codex-provider-switcher
```

GitHub Actions runs tests, the race detector, vet, and Linux cross-builds. The
first milestone does not publish binaries or modify a user's system.

## Repository Layout

```text
codex-provider-switcher/
├── cmd/codex-provider-switcher/main.go
├── internal/config/config.go
├── internal/config/config_test.go
├── internal/rewrite/rewrite.go
├── internal/rewrite/rewrite_test.go
├── internal/proxy/proxy.go
├── internal/proxy/proxy_test.go
├── docs/
│   ├── architecture.md
│   └── superpowers/specs/2026-07-31-codex-provider-switcher-design.md
├── .github/workflows/ci.yml
├── .gitignore
├── go.mod
├── LICENSE
└── README.md
```

The implementation uses only the Go standard library. The public command owns
argument parsing and exit behavior; internal packages separately own
configuration resolution, JSON-RPC rewriting, and child-process streaming.

## Acceptance Criteria

- Two switcher processes with different provider values can connect to the same
  running app-server control socket.
- Each process injects only its configured provider into task start, resume, and
  fork requests.
- Both processes request an unfiltered task list.
- Server output is returned without modification.
- No second app-server daemon or SQLite writer is created by the switcher.
- Missing routing identity cannot fall back silently.
- Tests, the race detector, vet, and Linux amd64/arm64 builds pass.
- Documentation contains no private infrastructure or credentials and clearly
  states the unsupported same-task concurrency case.
