# Codex Provider Switcher

> **v0.1.0 is broken for Desktop Remote SSH.** It parsed the stdin of
> `codex app-server proxy` as JSONL, but that stream contains an HTTP Upgrade
> and WebSocket frames. **v0.2.0 is also incompatible with stock Desktop Remote SSH**
> because its wrapper requires an explicit `--sock`. Do not install either
> version. Use v0.2.1 or newer.

Codex Provider Switcher is a provider-agnostic WebSocket proxy for Codex
Desktop Remote SSH. Separate SSH entry points can select different model
providers while sharing one Codex app-server daemon, one `CODEX_HOME`, and one
task database.

This is an independent community project. It is not an OpenAI product.

## How It Works

Desktop sends an HTTP WebSocket Upgrade and masked WebSocket frames through the
remote `codex app-server proxy` process. Stock Desktop invokes
`codex app-server proxy` without `--sock`; Codex normally resolves its control
socket from `CODEX_HOME` or the user's `.codex` directory. The switcher installs
as a transparent executable named `codex`, intercepts that effective command,
and opens a new WebSocket directly to the same app-server Unix socket:

```text
Desktop A -> switcher(provider-a) --+
                                      +-> one Codex app-server -> one task store
Desktop B -> switcher(provider-b) --+
```

All other `codex` commands are delegated unchanged to the real Codex
executable. The switcher does not start a second daemon or access SQLite.

Live wrapper processes connected to the same app-server also form a local
send-time provider handoff group. Opening a task only loads it for viewing. The
SSH alias that sends the next `turn/start` becomes the requested provider for
that task before the turn reaches app-server.

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

### Stock Proxy Compatibility

In transparent wrapper mode, the socket is selected in this order:

1. Explicit `--sock <path>` or `--sock=<path>` on `app-server proxy`.
2. `CODEX_PROVIDER_SWITCHER_SOCKET`.
3. `$CODEX_HOME/app-server-control/app-server-control.sock`.
4. `$HOME/.codex/app-server-control/app-server-control.sock`.

The wrapper recognizes repeatable `-c`/`--config`, `--enable`, and `--disable`
options before `app-server`, between `app-server` and `proxy`, and after
`proxy`. It also accepts `--strict-config` before either subcommand. Supported
configuration options are consumed for command classification but cannot
change the provider selected by the SSH entry point. Proxy help is delegated;
an invalid effective proxy command returns a sanitized wrapper error.

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
For one task, opening or viewing it through another SSH alias does not switch
the loaded runtime. Provider handoff occurs when that alias sends the next
message:

```text
open in sub2api alias -> view only, no provider change
send next message     -> unsubscribe idle peers -> resume with sub2api
                      -> resubscribe detached peers -> start turn
```

The handoff keeps the same thread id and persisted history. It does not restart
the shared daemon, create another daemon, edit SQLite, fork the task, or change
either daemon PID.

Peers that had the task open are detached only long enough to replace the idle
runtime. The switcher internally resumes those peers with the newly verified
provider before forwarding the original turn. Their internal resume responses
remain hidden, but previously open Desktop views receive the new turn's normal
item and lifecycle notifications and stay synchronized. A Desktop connection
that explicitly unsubscribed is not reattached.

An active turn is never interrupted or stolen. If another alias is still
running a turn, a client bypassed the switcher, or app-server cannot confirm the
requested provider, the new `turn/start` is not forwarded. Desktop receives a
JSON-RPC `-32090` error and can retry after the active turn finishes.
The same peer activity check also runs on the same-provider path, closing the
window where two aliases could submit before the second received
`turn/started`.

Automatic handoff requires Codex CLI 0.146.0 or newer. Older app-server
versions do not provide the idle zero-subscriber replacement behavior needed
for an immediate provider change; the switcher detects the unchanged provider
and fails closed.

### Handoff Runtime Files

Each live proxy exposes a mode-0700 local control socket below a short runtime
namespace derived from the Unix account and app-server socket:

```text
/tmp/cps-<uid>-<socket-hash>/
```

These sockets coordinate only wrapper processes for the same daemon. Per-task
file locks serialize resume/send transitions and are released automatically if
a process exits. Stale control sockets are removed after a failed local connect.

## Security Boundaries

- The SSH account and processes running as the same Unix user are trusted at
  the account-authority level. They can already invoke the real Codex binary,
  connect to their app-server socket, and change their own PATH or environment;
  this switcher is a routing-correctness layer, not an authorization boundary.
- Setuid installation, a socket privileged beyond the SSH user, and restricted
  `ForceCommand` containment are outside the supported deployment model.
- API keys and provider credentials remain in Codex configuration or the
  daemon environment.
- End-to-end handshake headers, including the downstream `request.Host`, are
  forwarded but never logged. Hop-by-hop and generated WebSocket headers are
  removed before the upstream handshake.
- Diagnostics do not include JSON bodies, prompts, header values, environment
  values, or candidate executable paths.
- Delegation uses an argument vector and `exec`; no shell evaluates arguments.
- Symlink and hard-link identity checks prevent recursive wrapper execution.
- Handoff control sockets are local, mode-protected, length-bounded, and never
  carry prompts or credentials. Thread ids are not included in diagnostics.

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
