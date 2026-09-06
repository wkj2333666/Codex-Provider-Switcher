# Codex Provider Switcher

[简体中文](README.zh-CN.md)

Codex Provider Switcher lets Codex Desktop Remote SSH use different model
providers in different tasks while keeping one SSH host, one sidebar, and one
Codex app-server.

This is an independent community project. It is not an OpenAI product.

> Use v0.2.1 or newer. v0.1.0 is incompatible with Desktop's WebSocket
> transport, and **v0.2.0 is also incompatible with stock Desktop Remote SSH**.

## What It Does

The project installs a small transparent wrapper named `codex`. It intercepts
Desktop's app-server proxy connection and delegates every other command to the
real Codex executable.

```text
Codex Desktop (one SSH alias)
    -> Codex Provider Switcher
    -> existing Codex app-server
    -> existing tasks and configuration
```

The switcher does not start another daemon or modify the task database. Tasks
without a saved provider continue to use app-server configuration. Provider
commands are handled locally and do not invoke a model.

## Requirements

- Linux or macOS on amd64 or arm64
- Codex CLI 0.146.0 or newer
- Codex Desktop Remote SSH using an app-server Unix socket
- Each provider already configured for the app-server
- `curl`, `tar`, and either `sha256sum` or `shasum`

Native Windows is not supported.

## Install

### 1. Download and verify a release

Set `VERSION` to the release you want to install. The commands detect the
current operating system and CPU architecture.

```bash
VERSION="0.5.7"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

PACKAGE="codex-provider-switcher-${VERSION}-${OS}-${ARCH}"
RELEASE_URL="https://github.com/wkj2333666/Codex-Provider-Switcher/releases/download/v${VERSION}"
WORK_DIR="$(mktemp -d)"
cd "$WORK_DIR"

curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz"
curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "$PACKAGE.tar.gz.sha256"
else
  shasum -a 256 -c "$PACKAGE.tar.gz.sha256"
fi
tar -xzf "$PACKAGE.tar.gz"
cd "$PACKAGE"
```

The checksum command must report `OK` before you continue. Releases are
available on [GitHub Releases](https://github.com/wkj2333666/Codex-Provider-Switcher/releases).

### 2. Install the wrapper and provider skill

Record the real Codex path before changing `PATH`:

```bash
REAL_CODEX="$(command -v codex)"
case "$REAL_CODEX" in /*) ;; *) echo "Codex path must be absolute" >&2; exit 1 ;; esac
printf 'Real Codex: %s\n' "$REAL_CODEX"
```

Install the switcher separately from the real Codex executable:

```bash
INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
mkdir -p "$INSTALL_ROOT/bin"
install -m 0755 codex-provider-switcher "$INSTALL_ROOT/codex-provider-switcher"
ln -sfn ../codex-provider-switcher "$INSTALL_ROOT/bin/codex"

mkdir -p "$HOME/.agents/skills"
rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

### 3. Configure provider-to-model routes

Create the strict `models.json` catalog in the switcher state directory. Give
each provider a default model. To select multiple models for one provider, use
a `default` plus a `models` allowlist; a string still means “only this model”:

```bash
STATE_ROOT="${CODEX_HOME:-$HOME/.codex}/codex-provider-switcher"
install -d -m 0700 "$STATE_ROOT"
install -m 0600 /dev/null "$STATE_ROOT/models.json"
printf '%s\n' '{"openai":{"default":"gpt-6-astra","models":["gpt-6-astra","gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","gpt-5.5","gpt-5.4","gpt-5.4-mini","gpt-5.3-codex-spark"]},"sub2api":"gpt-5.6-sol","glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.3-flash","glm-5.2"]},"kimi":"k3","deepseek":{"default":"deepseek-v4-flash","models":["deepseek-v4-flash","deepseek-v4-pro"]},"openrouter":{"default":"stealth/ox-alpha","models":["stealth/ox-alpha"]}}' \
  > "$STATE_ROOT/models.json"
```

Provider switches use the default model. Later Desktop picker changes persist
only when the model is in that provider's allowlist. A model change within the
same provider applies on the next turn without a provider handoff. Cross-provider
or unlisted values do not change the route. Invalid JSON or values, symlinks, and
non-regular files make the switcher fail closed before it proxies Desktop.

To show third-party models in Desktop, also load the Codex model catalog from
top-level `~/.codex/config.toml`, then reconnect Desktop SSH:

```toml
model_catalog_json = "/home/your-user/.codex/models-override.json"

[model_providers.openrouter]
name = "openrouter"
base_url = "https://openrouter.ai/api/v1"
wire_api = "responses"
env_key = "OPENROUTER_API_KEY"
```

OpenRouter uses the `OPENROUTER_API_KEY` environment variable. The configured
model is `stealth/ox-alpha`, exposed as both the task model and its automatic
reviewer, with image input enabled and a 10,000-token Desktop context cap.

### 4. Configure the login shell

Add these exports to the remote login-shell profile used by Desktop. Replace
the first value with the absolute path printed in the previous step.

```bash
export CODEX_PROVIDER_SWITCHER_CODEX="/absolute/path/to/real/codex"
export PATH="$HOME/.local/lib/codex-provider-switcher/bin:$PATH"
```

For a normal Bash login this is usually `~/.profile`.

Recovery from an exact idle provider mismatch or terminal `systemError` is
automatic. The switcher briefly archives and restores the affected thread to
unload its stale runtime. It keeps the thread ID, history, and descendants and
never resends the user's message. Every client that subscribes to the same
app-server threads must use the switcher wrapper; direct app-server subscribers
are unsupported.

Short-lived Desktop or Android SSH proxy connections can leave an older proxy
with a stale local `active` notification after app-server has already made the
task idle. Before returning handoff error `-32090`, the switcher now checks the
authoritative task status across all providers. It clears peer-local stale
state only for an exact `idle` or `systemError`; a locally active turn, active
server status, malformed response, or failed peer reconciliation still fails
closed before the user's message is sent. The per-task fence remains held until
app-server acknowledges a newly forwarded turn, so a genuine in-flight request
cannot be mistaken for stale state during that acknowledgement window.

Reconnects and explicit task unsubscription can also clear a proxy's in-memory
route while app-server still has the task loaded. Before deciding that the next
turn needs a provider handoff, the switcher restores the authoritative runtime
provider and model from `thread/list`. A matching provider uses the lightweight
same-provider path; missing, malformed, or genuinely different runtime state
still fails closed or performs the full handoff as appropriate.

When a task is handed to another provider, the switcher also checks its rollout
for stale provider-bound reasoning IDs. Invalid reasoning records are removed
and stale optional item IDs are stripped before app-server loads the history.
Compaction replacement history is checked too. OpenAI resumes also inspect history
to repair contamination left by older switchers; other same-provider resumes pass through.
Paginated rewrites preserve each record's byte length and ordinal, replacing invalid
reasoning with model-ignored placeholders so UI history and SQLite offsets remain valid.
A missing rollout for an existing task fails closed instead of being treated as clean.
Cross-provider rewrites are atomic and keep visible messages and
tool-call pairs. Malformed JSONL, or an active Codex writer when a rewrite is
required, makes the operation fail closed; clean rollouts remain a no-op and
the switcher never invents an `rs_` ID.

### 5. Verify and reconnect Desktop

Start a fresh login shell and run:

```bash
command -v codex
codex --version
"$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher" --version
```

The first command should show
`$HOME/.local/lib/codex-provider-switcher/bin/codex`. The second should show the
real Codex version, and the third should show the switcher version.

Reconnect the Desktop SSH host after installation so new proxy processes load
the installed version and provider skill.

## Use One SSH Host

Keep one SSH alias for the machine. Desktop gives each alias a separate host
identity and sidebar, even if multiple aliases reach the same server.

```sshconfig
Host pi
    HostName server.example.com
    User developer
```

Connect Desktop to that one alias. Different tasks under it can keep different
provider selections.

## Use The Provider Command

Select the provider skill from Desktop's command menu, then use one of these
commands:

```text
/provider status
/provider switch openai
/provider switch sub2api
/provider switch glm
/provider switch openrouter
```

For example, use `/provider switch openai` for the native provider or
`/provider switch sub2api`, `/provider switch glm`, or
`/provider switch openrouter` for configured alternatives.

The user-facing commands are:

- `/provider status` — show the task's verified runtime and saved selection.
- `/provider switch <name>` — switch the current idle task and save its
  selection.

The confirmation is generated locally, so it does not invoke a model or consume
model tokens. A successful alternative-provider switch reports
`Provider switched to sub2api using model gpt-5.6-sol.` Switching keeps the
same task ID and history.
An active turn is never interrupted; wait for it to finish before switching.

`models.json` controls the provider/model combinations allowed for the current
task; `model_catalog_json` controls what appears in Desktop's picker. Once a
task has a saved provider, an allowlisted same-provider picker choice is saved
with it. Other choices are rewritten to the task's saved safe route.

## Deploy the current main checkout

For this machine, use the repository's guarded deployment command instead of
copying a binary from whichever worktree happens to be open. It only accepts a
clean checkout on `main` whose `HEAD` exactly matches `origin/main`, then runs
tests, builds Linux arm64, verifies embedded provenance, and atomically replaces
the installed executable without a backup:

```bash
cd /home/wkj/projects/codex-provider-switcher
git switch main
git pull --ff-only origin main
scripts/deploy-local.sh
"$HOME/.local/lib/codex-provider-switcher/codex-provider-switcher" --build-info
```

The output must report `source` as `main` and the pushed commit you intended to
deploy. The script never stops an existing proxy; reconnect the Desktop Remote
SSH host so new proxy processes load the replacement.

## Upgrade from a release archive

Repeat the download and checksum steps with the new `VERSION`, enter the
extracted package directory, stage the binary and model catalog, activate the
catalog before the binary, and replace the provider skill:

```bash
INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
BINARY_STAGE="$(mktemp "$INSTALL_ROOT/.codex-provider-switcher.XXXXXX")"
install -m 0755 codex-provider-switcher "$BINARY_STAGE"

STATE_ROOT="${CODEX_HOME:-$HOME/.codex}/codex-provider-switcher"
install -d -m 0700 "$STATE_ROOT"
MODELS_STAGE="$(mktemp "$STATE_ROOT/.models.json.XXXXXX")"
install -m 0600 /dev/null "$MODELS_STAGE"
printf '%s\n' '{"openai":{"default":"gpt-6-astra","models":["gpt-6-astra","gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","gpt-5.5","gpt-5.4","gpt-5.4-mini","gpt-5.3-codex-spark"]},"sub2api":"gpt-5.6-sol","glm":{"default":"glm-5.3","models":["glm-5.3","glm-5.3-flash","glm-5.2"]},"kimi":"k3","deepseek":{"default":"deepseek-v4-flash","models":["deepseek-v4-flash","deepseek-v4-pro"]},"openrouter":{"default":"stealth/ox-alpha","models":["stealth/ox-alpha"]}}' \
  > "$MODELS_STAGE"
mv -f "$MODELS_STAGE" "$STATE_ROOT/models.json"
mv -f "$BINARY_STAGE" "$INSTALL_ROOT/codex-provider-switcher"

rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

These commands replace the current installation and do not keep an old-version
backup. Keep the existing `CODEX_PROVIDER_SWITCHER_CODEX` value—it must point to
the real Codex executable, not the wrapper. Reconnect the Desktop SSH host after
every upgrade. The staged catalog replacement installs the exact supported
routes at mode 0600 before the new binary becomes active.

## Troubleshooting

**Desktop does not show the provider command:** confirm
`$HOME/.agents/skills/provider/SKILL.md` exists, then reconnect the SSH host.

**`command -v codex` does not show the wrapper:** check that the wrapper's `bin`
directory appears before the real Codex directory in the remote login-shell
`PATH`.

**Codex delegation loops or fails:** check that `CODEX_PROVIDER_SWITCHER_CODEX`
is an absolute path to the real Codex executable.

**A switch is rejected:** finish any active turn first. Also confirm the
provider is configured in the app-server and Codex CLI is 0.146.0 or newer.
After upgrading the switcher, reconnect the Desktop SSH host so every live
proxy uses the same version.

**A later message returns JSON-RPC `-32090`:** upgrade to v0.5.9 or newer and
reconnect every Desktop/Android SSH client once. This version reconciles stale
peer activity only after app-server verifies that the task is idle and no
longer mistakes a model-only change inside one provider for a full handoff. If
Codex keeps a failed task's rollout writer lock after unsubscribe, the switcher
now performs its journaled soft unload before sanitation and resumes only after
the rollout is safe for the target provider. Active turns and cross-provider
history remain protected.

**The app-server socket is unavailable:** connect or restart Codex Desktop's
remote host so its normal app-server is running. The switcher intentionally
fails closed instead of starting a second daemon.

For transport and handoff details, see
[docs/architecture.md](docs/architecture.md).

## Uninstall

Remove the switcher exports from the remote login-shell profile, then remove the
wrapper and skill:

```bash
rm -rf "$HOME/.local/lib/codex-provider-switcher"
rm -rf "$HOME/.agents/skills/provider"
unset CODEX_PROVIDER_SWITCHER_CODEX
hash -r 2>/dev/null || true
command -v codex
codex --version
```

The real Codex installation was never overwritten and should become the first
`codex` in `PATH` again.

## Development

```bash
go build -trimpath -o codex-provider-switcher ./cmd/codex-provider-switcher
go test ./...
go test -race ./...
go vet ./...
```

Advanced proxy options, routing rules, WebSocket limits, provider handoff, and
security boundaries are documented in
[docs/architecture.md](docs/architecture.md).

See [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) for dependency licenses.

## License

MIT. See [LICENSE](LICENSE).
