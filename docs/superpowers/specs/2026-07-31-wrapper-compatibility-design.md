# Wrapper Compatibility And Licensing Patch Design

## Status

This document defines the v0.2.1 compatibility patch for Codex Provider
Switcher. It supplements the WebSocket transport design and supersedes its
wrapper argument, default-socket, upstream Host, and release-license rules where
they conflict.

Codex Provider Switcher v0.2.0 correctly handles the WebSocket protocol but
does not work with the stock Desktop Remote SSH command because it requires an
explicit `--sock`. It also delegates valid proxy commands when supported Codex
configuration options precede either subcommand.

## Verified Behavior

Codex CLI 0.146.0 accepts all of these forms:

```text
codex app-server proxy
codex -c 'model="example"' app-server proxy
codex app-server -c 'model="example"' proxy
codex app-server proxy -c 'model="example"'
```

`-c/--config`, `--enable`, and `--disable` are accepted at the top-level,
`app-server`, and `app-server proxy` argument layers. `--sock` is optional.

When `--sock` is absent, the official proxy connects to:

```text
${CODEX_HOME:-$HOME/.codex}/app-server-control/app-server-control.sock
```

A real Codex 0.146.0 invocation without `--sock` has been verified to complete
the WebSocket Upgrade against the current control socket.

## Threat Model

The SSH account and processes running as that Unix user are trusted at the
account-authority level. A user who can log in as that account can already run
the real Codex executable, connect directly to the app-server socket, change
their environment and PATH, and replace their own wrapper. The switcher is not
an authorization boundary against a malicious user with the same account.

Inputs are still treated as potentially invalid. Desktop version drift,
damaged frames, unexpected shell output, and malformed arguments must produce
bounded, sanitized errors. Message limits, JSON shape checks, hop-by-hop header
filtering, and no-body logging remain required for robustness and privacy.

The supported deployment does not include setuid installation, a privileged
socket that the SSH user cannot otherwise access, or a restricted `ForceCommand`
environment intended to contain a hostile user. Such deployments require a
different security design.

Provider enforcement is therefore a routing-correctness guarantee: compatible
Desktop proxy commands must not accidentally delegate to the real proxy or
silently use a default provider.

## Wrapper Command Grammar

The wrapper uses a small explicit parser rather than copying the full Codex CLI
grammar. It recognizes the operational command while consuming only options
that are valid and transport-neutral at the relevant layers.

### Common Configuration Options

The following repeatable options are accepted before `app-server`, between
`app-server` and `proxy`, and after `proxy`:

```text
-c <key=value>
--config <key=value>
--config=<key=value>
--enable <feature>
--enable=<feature>
--disable <feature>
--disable=<feature>
```

The wrapper consumes these values but does not apply them. The official proxy
uses them for Codex configuration parsing, while the switcher's intercepted
path only selects a Unix socket and forwards WebSocket messages. They cannot
alter the connection's selected provider.

Missing option values, empty feature names, and malformed supported options
return a sanitized wrapper error and are never delegated.

`--strict-config` is accepted before `app-server` and between `app-server` and
`proxy`. It is transport-neutral and has no value. It is not accepted after
`proxy`, matching the Codex 0.146.0 proxy help.

### Command Identification

Parsing proceeds in three stages:

1. Consume supported top-level configuration options. The first positional
   token must be inspected as the top-level command.
2. If it is not `app-server`, return `Delegate` and preserve the entire original
   argument vector.
3. After `app-server`, consume supported app-server options and inspect its
   subcommand. If it is not `proxy`, return `Delegate`.
4. After `proxy`, consume supported common configuration options and one
   optional `--sock <path>` or `--sock=<path>`.

`codex app-server proxy -h` and `--help` are delegated because they do not open
a socket or route work. Every operational command whose effective subcommands
are `app-server proxy` returns `Proxy`, including parse failures. This preserves
fail-closed routing behavior.

Once `app-server` has been identified, an unsupported option before a later
`proxy` token returns a wrapper error. Before the top-level command is known,
an unsupported option followed by an `app-server ... proxy` shape also returns
a wrapper error instead of silently delegating. A known non-app-server command,
such as `exec`, `review`, or `daemon` under app-server, delegates normally even
if later argument values contain the words `app-server` or `proxy`.

The parser never includes rejected option values in diagnostics.

## Socket Resolution

Direct mode remains unchanged: `codex-provider-switcher proxy` requires a
socket from `--socket` or `CODEX_PROVIDER_SWITCHER_SOCKET`.

Transparent wrapper mode resolves the socket in this order:

1. Explicit proxy `--sock` or `--sock=<path>`.
2. `CODEX_PROVIDER_SWITCHER_SOCKET` when non-empty.
3. `$CODEX_HOME/app-server-control/app-server-control.sock` when `CODEX_HOME`
   is non-empty.
4. `<user-home>/.codex/app-server-control/app-server-control.sock`, where the
   user home comes from `os.UserHomeDir()`; failure to determine it is a
   sanitized configuration error.

The selected path is passed through the existing configuration validation: it
is made absolute and must exist as a Unix socket before the HTTP Upgrade is
accepted. An unavailable default fails with a sanitized socket-inspection
error.

## Upstream Host Semantics

The official `codex app-server proxy` copies the downstream HTTP Upgrade bytes
to the Unix socket, including the Host value. The WebSocket-aware switcher must
preserve the same semantic behavior by setting the upstream request Host to the
downstream `request.Host`.

This is compatible with app-server Origin/Host validation and does not create a
meaningful privilege boundary under the trusted same-user threat model. Host is
still excluded from the cloned `http.Header` map so the WebSocket client library
emits exactly one Host field. Standard hop-by-hop and generated WebSocket
headers remain stripped.

## Third-Party License Notice

The repository adds `THIRD_PARTY_NOTICES` containing:

- Module and pinned version: `github.com/coder/websocket v1.8.15`.
- The complete upstream ISC license text.
- The upstream copyright line exactly as shipped:
  `Copyright (c) 2025 Coder`.

Every release archive contains `README.md`, the project's MIT `LICENSE`, and
`THIRD_PARTY_NOTICES`. Repository tests assert that the notice contains the
pinned dependency and required ISC text and that the release workflow copies
the file.

## Test Strategy

### Wrapper Unit Tests

Table-driven tests cover:

- `app-server proxy` with no options.
- Explicit `--sock` in split and equals forms.
- `CODEX_PROVIDER_SWITCHER_SOCKET` override.
- `CODEX_HOME` default socket.
- `$HOME/.codex` fallback.
- `-c/--config`, `--enable`, and `--disable` at all three grammar layers.
- Multiple repeatable common options around both subcommands.
- Missing values and duplicate socket options.
- Unsupported ambiguous options fail closed without leaking their values.
- Known non-proxy commands delegate with their original arguments.
- Proxy help delegates without requiring a provider or socket.

### Integration Tests

A main-level test creates the default socket below a temporary `CODEX_HOME`,
invokes wrapper mode as `codex app-server proxy` with no `--sock`, performs a
real raw HTTP/WebSocket Upgrade, and verifies a masked JSON-RPC text frame is
rewritten before reaching the UDS test server.

The upstream handshake test expects the original downstream Host and Origin to
arrive at the Unix-socket server while generated and hop-by-hop headers remain
absent.

### Repository And Release Tests

Tests verify the complete third-party notice and require
`THIRD_PARTY_NOTICES` in the release packaging command. Existing module-tidy,
formatting, test, race, vet, and four-platform build gates remain unchanged.
Each package job lists the completed tar archive and fails unless
`THIRD_PARTY_NOTICES` is present at the archive root beside README and LICENSE.

## Documentation And Release

README installation instructions must state that stock Desktop invokes
`codex app-server proxy` without `--sock` and that the wrapper mirrors Codex's
default socket resolution. Explicit socket configuration remains documented as
an override, not a stock Desktop requirement.

The v0.2.0 GitHub Release is marked as incompatible with stock Desktop Remote
SSH because its wrapper requires `--sock`. After CI passes, the patch is merged
and released as v0.2.1 with four archives and four checksum files. The release
workflow must prove that each archive contains `THIRD_PARTY_NOTICES`.

## Acceptance Criteria

- Stock `codex app-server proxy` reaches the default control socket and returns
  a valid WebSocket `101` response through the switcher.
- All three reviewed `-c` placements are intercepted, never delegated.
- `--enable` and `--disable` placements at all supported layers are intercepted.
- Invalid effective proxy forms fail closed with sanitized diagnostics.
- Non-proxy Codex commands still delegate unchanged.
- The upstream Host matches the downstream Host.
- Release archives include the complete Coder ISC notice.
- Local verification, independent review, GitHub race, macOS tests, four target
  builds, checksums, and v0.2.1 publication all pass.

## References

- OpenAI Codex app-server transport documentation and default proxy socket:
  <https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>
- Coder WebSocket repository and ISC license:
  <https://github.com/coder/websocket/blob/v1.8.15/LICENSE.txt>
