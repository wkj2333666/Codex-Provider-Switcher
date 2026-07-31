# Codex Provider Switcher

Codex Provider Switcher is a small, provider-agnostic JSON-RPC proxy for Codex
Desktop Remote SSH connections. Separate SSH entry points can select different
model providers while sharing one Codex app-server daemon, one `CODEX_HOME`, and
one task database.

This is an independent community project. It is not an OpenAI product.

## Why

Separate `CODEX_HOME` directories also create separate task stores. Sharing the
same SQLite files between multiple app-server daemons creates competing writers
and is not a supported way to merge those stores.

The switcher instead runs the official stdio proxy and changes only the
provider-routing fields in client requests:

```text
Desktop A -> switcher(provider-a) --+
                                      +-> one Codex app-server -> one task store
Desktop B -> switcher(provider-b) --+
```

It executes exactly:

```text
codex app-server proxy --sock <socket>
```

## Requirements

- Linux or macOS on amd64 or arm64
- Codex CLI with app-server protocol v2 provider overrides (tested against
  Codex CLI 0.146.0)
- One already-running app-server on a Unix domain socket
- Every selected provider configured in the daemon's effective Codex config

Native Windows is not supported because the switcher validates and uses a Unix
domain socket.

## Install

Download the archive for your operating system and architecture from GitHub
Releases, verify it against the adjacent `.sha256` file, then place the binary
on the remote host's `PATH`.

To build from source:

```bash
go build -trimpath -o codex-provider-switcher ./cmd/codex-provider-switcher
./codex-provider-switcher --version
```

## Usage

```text
Usage: codex-provider-switcher [options]

Options:
  --provider <id>  Provider to inject into task requests
  --socket <path>  Shared app-server Unix socket
  --codex <path>   Codex executable or command name; defaults to codex
  --version        Print the switcher version and exit
  --help           Print help and exit
```

Equivalent environment variables are:

```text
CODEX_PROVIDER_SWITCHER_PROVIDER
CODEX_PROVIDER_SWITCHER_SOCKET
CODEX_PROVIDER_SWITCHER_CODEX
```

Flags take precedence over environment variables. Provider IDs accept ASCII
letters, digits, `.`, `_`, and `-`. Both provider and socket are required; the
process fails before starting Codex if either is missing or invalid.

### Shared daemon

Use one absolute socket path for every connection. If your Codex setup does not
already manage the daemon, a foreground example is:

```bash
mkdir -p "$HOME/.local/state/codex"
codex app-server --listen "unix://$HOME/.local/state/codex/app-server.sock"
```

Codex also provides `codex app-server daemon bootstrap` and `daemon start` for
managed SSH-driven use. Daemon setup remains the user's responsibility; this
project never starts or supervises it.

### Provider entry points

Configure each Desktop Remote SSH entry point to launch the switcher as its
stdio proxy. For example, the two remote commands are:

```bash
codex-provider-switcher \
  --provider provider-a \
  --socket "$HOME/.local/state/codex/app-server.sock"

codex-provider-switcher \
  --provider provider-b \
  --socket "$HOME/.local/state/codex/app-server.sock"
```

Environment-based entries are equivalent:

```bash
CODEX_PROVIDER_SWITCHER_PROVIDER=provider-a \
CODEX_PROVIDER_SWITCHER_SOCKET="$HOME/.local/state/codex/app-server.sock" \
exec codex-provider-switcher
```

Use the same `CODEX_HOME` and socket for every entry. Provider credentials stay
in Codex auth storage, provider environment variables, or another
Codex-supported source; they are never handled by the switcher.

## Routing behavior

| Client method | Enforced field |
| --- | --- |
| `thread/start` | `params.modelProvider = <selected provider>` |
| `thread/resume` | `params.modelProvider = <selected provider>` |
| `thread/fork` | `params.modelProvider = <selected provider>` |
| `thread/list` | `params.modelProviders = []` |

An existing client value is overwritten. Missing or null `params` becomes an
object. A target request with non-object `params` terminates the connection
instead of risking fallback to the wrong provider.

Other valid client messages pass through unchanged. Server output is copied
byte-for-byte and never parsed. Messages larger than 64 KiB are supported.

## Task semantics

A task can be used sequentially through different providers:

```text
resume with provider-a -> finish the turn -> leave the task
resume with provider-b -> continue from the same persisted history
```

Different tasks can be active through different providers at the same time.
Operating the same loaded task concurrently through different providers is not
supported because provider selection belongs to the loaded thread runtime.

## Security boundaries

The switcher does not:

- read or write Codex SQLite databases;
- start a second app-server daemon;
- inspect API keys, prompts, responses, or authentication traffic;
- edit shell, SSH, Codex, or service configuration;
- invoke a shell to start Codex.

Diagnostics contain error categories, target method names, line numbers, and
process status. They do not contain request bodies or environment values.

See [docs/architecture.md](docs/architecture.md) for the component and lifecycle
details.

## Development

```bash
gofmt -w cmd internal
go test ./...
go test -race ./...
go vet ./...
```

CI also cross-builds Linux and macOS binaries for amd64 and arm64. A `v*` tag
triggers the release workflow, which publishes four versioned archives and
their SHA-256 checksum files.

## License

MIT
