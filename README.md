# Codex Provider Switcher

> **v0.1.0 is broken for Desktop Remote SSH.** It parsed the stdin of
> `codex app-server proxy` as JSONL, but that stream contains an HTTP Upgrade
> and WebSocket frames. Do not install or use v0.1.0. Use v0.2.0 or newer.

Codex Provider Switcher is a provider-agnostic WebSocket proxy for Codex
Desktop Remote SSH. Separate SSH entry points can select different model
providers while sharing one Codex app-server daemon, one `CODEX_HOME`, and one
task database.

This is an independent community project. It is not an OpenAI product.

## How It Works

Desktop sends an HTTP WebSocket Upgrade and masked WebSocket frames through the
remote `codex app-server proxy --sock ...` process. The switcher installs as a
transparent executable named `codex`, intercepts only that command, and opens a
new WebSocket directly to the app-server Unix socket:

```text
Desktop A -> switcher(provider-a) --+
                                      +-> one Codex app-server -> one task store
Desktop B -> switcher(provider-b) --+
```

All other `codex` commands are delegated unchanged to the real Codex
executable. The switcher does not start a second daemon or access SQLite.

## Requirements

- Linux or macOS on amd64 or arm64
- Codex CLI with app-server protocol v2 provider overrides (tested against
  Codex CLI 0.146.0)
- One app-server Unix socket managed by Codex Desktop or an existing daemon
- Every selected provider configured in the daemon's effective Codex config
- A remote login shell whose PATH can put the wrapper before the real Codex

Native Windows is not supported because the runtime transport uses Unix domain
sockets.

## Install For Stock Desktop

Download the archive for the remote operating system and architecture from
GitHub Releases and verify the adjacent `.sha256` file. The following setup
keeps the real Codex installation untouched.

First record the absolute real Codex path before changing PATH:

```bash
REAL_CODEX="$(command -v codex)"
test -n "$REAL_CODEX"
case "$REAL_CODEX" in /*) ;; *) echo "Codex path must be absolute" >&2; exit 1;; esac
```

Install the switcher and create a separate transparent wrapper directory:

```bash
mkdir -p "$HOME/.local/lib/codex-provider-switcher/bin"
install -m 0755 codex-provider-switcher \
  "$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher"
ln -s ../codex-provider-switcher \
  "$HOME/.local/lib/codex-provider-switcher/bin/codex"
```

Add these values to the remote login-shell profile used by Desktop, replacing
the example real path with the value printed by `command -v codex` above:

```bash
export CODEX_PROVIDER_SWITCHER_CODEX="/absolute/path/to/real/codex"
export PATH="$HOME/.local/lib/codex-provider-switcher/bin:$PATH"
```

Verify both paths in a fresh login shell:

```bash
command -v codex
codex --version
"$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher" --version
```

`command -v codex` must show the wrapper symlink. `codex --version` must still
show the real Codex version because non-proxy commands are delegated.

### Select A Provider Per SSH Alias

OpenSSH can attach the provider identity to each local alias:

```sshconfig
Host codex-provider-a
    HostName server.example.com
    User developer
    SetEnv CODEX_PROVIDER_SWITCHER_PROVIDER=provider-a

Host codex-provider-b
    HostName server.example.com
    User developer
    SetEnv CODEX_PROVIDER_SWITCHER_PROVIDER=provider-b
```

The remote SSH server must permit that variable, for example in
`sshd_config`:

```text
AcceptEnv CODEX_PROVIDER_SWITCHER_PROVIDER
```

Reload the SSH daemon after validating its configuration. If server policy
does not allow `AcceptEnv`, use separate remote accounts or an equivalent
server-side environment wrapper. Desktop does not expose a custom proxy-command
setting, so simply documenting a different command is not sufficient.

The wrapper fails closed when the provider is missing. It never delegates a
malformed `app-server proxy` invocation to the real CLI.

## Direct Proxy Mode

Direct mode is available for tests and integrations that can choose the
command themselves:

```bash
codex-provider-switcher proxy \
  --provider provider-a \
  --socket "$HOME/.local/state/codex/app-server.sock"
```

Direct proxy environment equivalents are:

```text
CODEX_PROVIDER_SWITCHER_PROVIDER
CODEX_PROVIDER_SWITCHER_SOCKET
```

Flags take precedence over environment values. Provider IDs accept ASCII
letters, digits, `.`, `_`, and `-`.

`CODEX_PROVIDER_SWITCHER_CODEX` is wrapper-only. It identifies the absolute
real Codex executable used when delegating non-proxy commands.

## Routing Behavior

| Client method | Enforced field |
| --- | --- |
| `thread/start` | `params.modelProvider = <selected provider>` |
| `thread/resume` | `params.modelProvider = <selected provider>` |
| `thread/fork` | `params.modelProvider = <selected provider>` |
| `thread/list` | `params.modelProviders = []` |

An existing client value is overwritten. Missing or null `params` becomes an
object. A target request with non-object `params` closes the connection instead
of risking fallback to the wrong provider.

Only downstream WebSocket text messages are candidates for JSON-RPC parsing.
Unknown valid text messages pass through byte-for-byte. Binary messages and all
server messages pass through without parsing. The WebSocket implementation
handles masking, all payload length encodings, fragmentation, ping/pong, close,
and partial I/O.

`permessage-deflate` is disabled on both WebSocket connections. The message
limit is 64 MiB.

## Task Semantics

A task can be used sequentially through different providers:

```text
resume with provider-a -> finish the turn -> leave the task
resume with provider-b -> continue from the same persisted history
```

Different tasks can be active through different providers at the same time.
Operating the same loaded task concurrently through different providers is not
supported because provider selection belongs to the loaded thread runtime.

## Security Boundaries

- API keys and provider credentials remain in Codex configuration or the
  daemon environment.
- End-to-end handshake headers are forwarded but never logged. Hop-by-hop and
  generated WebSocket headers are removed before the upstream handshake.
- Diagnostics do not include JSON bodies, prompts, header values, environment
  values, or candidate executable paths.
- Delegation uses an argument vector and `exec`; no shell evaluates arguments.
- Symlink and hard-link identity checks prevent recursive wrapper execution.

## Uninstall

Remove the two profile exports added during installation, start a fresh login
shell, and remove the isolated wrapper directory:

```bash
rm -rf "$HOME/.local/lib/codex-provider-switcher"
unset CODEX_PROVIDER_SWITCHER_CODEX CODEX_PROVIDER_SWITCHER_PROVIDER
hash -r 2>/dev/null || true
command -v codex
codex --version
```

The real Codex binary was never overwritten, so it should become the first
`codex` in PATH again.

## Build And Test

```bash
go build -trimpath -o codex-provider-switcher ./cmd/codex-provider-switcher
go test ./...
go test -race ./...
go vet ./...
```

CI cross-builds Linux and macOS binaries for amd64 and arm64. A `v*` tag runs
the release workflow and publishes four archives plus SHA-256 files.

See [docs/architecture.md](docs/architecture.md) for transport and lifecycle
details.

## License

The project is licensed under MIT. Statically linked dependency licenses are
included in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
