# Codex Provider Switcher

> **v0.1.0 is broken for Desktop Remote SSH.** It parsed the stdin of
> `codex app-server proxy` as JSONL, but that stream contains an HTTP Upgrade
> and WebSocket frames. **v0.2.0 is also incompatible with stock Desktop Remote SSH**
> because its wrapper requires an explicit `--sock`. Do not install either
> version. Use v0.2.1 or newer.

Codex Provider Switcher is a provider-agnostic WebSocket proxy for Codex
Desktop Remote SSH. One SSH alias can switch each task between configured model
providers while retaining one Desktop host identity, one sidebar, one Codex
app-server daemon, one `CODEX_HOME`, and one task database.

This is an independent community project. It is not an OpenAI product.

## How It Works

Desktop sends an HTTP WebSocket Upgrade and masked WebSocket frames through the
remote `codex app-server proxy` process. Stock Desktop invokes
`codex app-server proxy` without `--sock`; Codex normally resolves its control
socket from `CODEX_HOME` or the user's `.codex` directory. The switcher installs
as a transparent executable named `codex`, intercepts that effective command,
and opens a new WebSocket directly to the same app-server Unix socket:

```text
Desktop (one SSH alias)
    -> switcher(no default provider override)
    -> one Codex app-server
    -> one task store
```

All other `codex` commands are delegated unchanged to the real Codex
executable. The switcher does not start a second daemon or access SQLite.

Live wrapper processes connected to the same app-server form a local provider
handoff group. `/provider status` reports the current task's verified runtime
and saved selection. `/provider switch <name>` switches an idle task, persists
its selection, and returns a local synthetic confirmation without invoking a
model.

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

install -d -m 0755 "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider/. \
  "$HOME/.agents/skills/provider/"
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

### Use One SSH Alias

Keep one Desktop Remote SSH entry for the machine. For example:

```sshconfig
Host pi
    HostName server.example.com
    User developer
```

Desktop assigns a different host ID and sidebar to every SSH alias, even when
the aliases reach the same machine. Multiple aliases therefore cannot provide
one shared Desktop task list. The single-alias setup keeps the host identity
stable. A task without a saved switcher selection is sent without a provider
override, so app-server configuration selects the provider using Codex's normal
configuration precedence and built-in defaults.

### Inspect Or Switch The Current Task

After reconnecting the `pi` entry so Desktop discovers the installed skill,
invoke it from the slash menu. Query the current routing state or explicitly
switch to a configured provider. For example, use `/provider switch openai`
for the native provider or `/provider switch sub2api` for the configured
alternative:

```text
/provider status
/provider switch openai
/provider switch sub2api
```

Desktop transmits explicit skill invocations as `$provider status` and
`$provider switch <name>`. Plain clients may send `/provider ...` or the literal
spaced form `/ provider ...`. The legacy `/provider <name>` form is rejected,
and mentions inside ordinary prompts are not commands.

Status is read-only and does not resume a task or change its provider. A loaded
task can report:

```text
Runtime provider: openai (verified).
Selected provider: app-server configuration.
```

When the connection has not observed a start or resume response, it reports
`Runtime provider: unknown.` rather than guessing. If a saved selection differs
from the verified runtime, the second line says it `will be applied before the
next model turn`.

For a successful switch, the switcher performs the provider handoff and emits a
synthetic local turn containing, for example:

```text
Provider switched to sub2api.
```

The control turn does not invoke a model, consume model tokens, or enter Codex
rollout history. Its confirmation is UI-only and disappears after reopening
the task. The provider selection does persist and applies to later messages and
resumes for that task.

Selections are private mode-0600 files in a mode-0700 directory. The location
is selected by `--state-dir`, then `CODEX_PROVIDER_SWITCHER_STATE_DIR`, then
`$CODEX_HOME/codex-provider-switcher`; stock socket paths infer the same
directory. A custom socket uses a socket-specific directory beside that socket.

The wrapper fails closed when the effective socket is unavailable. It never
delegates a malformed `app-server proxy` invocation to the real CLI.

### Stock Proxy Compatibility

In transparent wrapper mode, the socket is selected in this order:

1. Explicit `--sock <path>` or `--sock=<path>` on `app-server proxy`.
2. `CODEX_PROVIDER_SWITCHER_SOCKET`.
3. `$CODEX_HOME/app-server-control/app-server-control.sock`.
4. `$HOME/.codex/app-server-control/app-server-control.sock`.

The wrapper recognizes repeatable `-c`/`--config`, `--enable`, and `--disable`
options before `app-server`, between `app-server` and `proxy`, and after
`proxy`. It also accepts `--strict-config` before either subcommand. Supported
configuration options are consumed for command classification and remain part
of app-server's own effective configuration. Proxy help is delegated; an
invalid effective proxy command returns a sanitized wrapper error.

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
CODEX_PROVIDER_SWITCHER_SOCKET
CODEX_PROVIDER_SWITCHER_STATE_DIR
```

Direct mode accepts an optional `--provider` override and `--state-dir`. When
`--provider` is omitted, the request is left for app-server configuration to
resolve. Provider IDs accept ASCII letters, digits, `.`, `_`, and `-`.

`CODEX_PROVIDER_SWITCHER_CODEX` is wrapper-only. It identifies the absolute
real Codex executable used when delegating non-proxy commands.

## Routing Behavior

| Client method | Enforced field |
| --- | --- |
| `thread/start` | Explicit direct-mode selection overrides `params.modelProvider`; otherwise unchanged |
| `thread/resume` | Saved or explicit selection overrides `params.modelProvider`; otherwise unchanged |
| `thread/fork` | Explicit direct-mode selection overrides `params.modelProvider`; otherwise unchanged |
| `thread/list` | `params.modelProviders = []` |

When an override is active, an existing client value is overwritten. Missing or
null `params` becomes an object. A target request with non-object `params`
closes the connection instead of risking fallback to the wrong provider. With
no override, provider-bearing methods pass through byte-for-byte. The switcher
does not read or parse `config.toml`; app-server remains the single owner of
Codex configuration resolution.

Only downstream WebSocket text messages are candidates for JSON-RPC parsing.
Unknown valid text messages pass through byte-for-byte. Binary messages and all
server messages pass through without parsing. The WebSocket implementation
handles masking, all payload length encodings, fragmentation, ping/pong, close,
and partial I/O.

`permessage-deflate` is disabled on both WebSocket connections. The message
limit is 64 MiB.

## Task Semantics

A task can be used sequentially through different providers on the same
Desktop connection:

```text
/provider status          -> local read-only synthetic status
/provider switch openai   -> local handoff -> later messages use openai
/provider switch sub2api  -> local handoff -> later messages use sub2api
```

Different tasks can be active through different providers at the same time.
Opening or viewing a task does not change its saved selection. A provider
command on an idle task performs the handoff before returning its fake turn:

```text
/provider switch sub2api -> unsubscribe idle peers -> resume with sub2api
                         -> resubscribe detached peers -> persist selection
                         -> synthesize local completed turn
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

An active turn is never interrupted or stolen. If another live connection is
still running a turn, a client bypassed the switcher, or app-server cannot
confirm the requested provider, the control is rejected. Desktop receives a
JSON-RPC `-32090` error and can retry after the active turn finishes. The same
peer activity check also runs on the same-provider path, closing the window
where two connections could submit before the second received `turn/started`.

Once a handoff starts changing subscriptions, a per-task dirty marker remains
until every phase succeeds. A failed unsubscribe, sender resume, or peer
resubscribe triggers best-effort `restore` for every live peer without a
provider override. This reattaches previously open views to the actual runtime.
The dirty marker forces the next sender to run the complete handoff even when a
different session already reports the requested provider.

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
a process exits. Hashed dirty markers persist incomplete transitions across
proxy exits. Stale control sockets are removed after a failed local connect.
Cross-provider mutation requires the `prepareHandoffV2` capability, so older
live proxies fail before any subscription changes; reconnect the Desktop entry
after an upgrade.

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

Remove the profile exports added during installation, start a fresh login
shell, and remove the isolated wrapper directory and provider skill:

```bash
rm -rf "$HOME/.local/lib/codex-provider-switcher"
rm -rf "$HOME/.agents/skills/provider"
unset CODEX_PROVIDER_SWITCHER_CODEX
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
the release workflow and publishes four archives plus SHA-256 files. Every
archive includes the explicit-only provider skill plugin.

See [docs/architecture.md](docs/architecture.md) for transport and lifecycle
details.

## License

The project is licensed under MIT. Statically linked dependency licenses are
included in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
