# Single-Alias Provider Command Design

## Goal

Let one Codex Desktop SSH host keep one shared sidebar while each task can switch
between configured model providers with `/provider <name>`. The switch is a local
control operation: it must not call a model, consume tokens, or persist the
control message in the Codex rollout.

## User Interaction

The project ships a skill named `provider`. Codex Desktop exposes enabled skills
from the remote host in its command/skill picker. Selecting it produces the
documented app-server skill input shape: a text input beginning with `$provider`
and a `{ "type": "skill", "name": "provider" }` item. The switcher also accepts
an exact plain-text `/provider <name>` input for clients that transmit the slash
form directly.

Only one provider name is accepted. Names use the existing provider identifier
grammar: ASCII letters, digits, dot, underscore, and hyphen. A command with
attachments, extra input items, missing arguments, or trailing prose is rejected
without reaching the model. Ordinary prompts that merely mention `/provider` or
`$provider` are forwarded unchanged.

On success Desktop renders a short synthetic reply:

```text
Provider switched to sub2api.
```

The synthetic turn is intentionally absent from `thread/read` after the task is
reopened. It is UI feedback for a local routing control, not conversation
history.

## Routing State

The connection's configured provider remains the default for new and previously
unselected tasks. A successful command writes a per-task selection to a private
state directory. `CODEX_PROVIDER_SWITCHER_STATE_DIR` or `--state-dir` can set the
directory explicitly. Otherwise the default is:

1. `$CODEX_HOME/codex-provider-switcher`, when `CODEX_HOME` is set.
2. A sibling `codex-provider-switcher` directory inferred from the stock
   `app-server-control/app-server-control.sock` path.
3. A socket-specific hidden directory beside a custom socket.

The directory is mode `0700`. Each selection is a mode `0600` file named from a
SHA-256 digest of the task ID; task IDs never appear in filenames or errors.
Writes use a temporary file and atomic rename. The existing per-task cross-
process lock serializes selection changes with handoff and send operations.

`thread/resume` uses the stored provider override so reopening a task preserves
its selection. A normal `turn/start` also resolves the stored selection before
the existing handoff logic runs. A missing selection falls back to the
connection default. A corrupt or unreadable selection fails closed.

## Control Flow

For a recognized provider command, the session:

1. Acquires the existing per-task cross-process lock.
2. Rejects the command if this session or any cooperating peer reports an active
   turn.
3. Runs the existing dirty-state-aware handoff to the requested provider.
4. Verifies `thread/resume` returned the requested provider and resubscribes all
   previously attached peers.
5. Atomically persists the task selection.
6. Does not forward the original `turn/start` to app-server.
7. Writes a synthetic `TurnStartResponse`, `turn/started`, user and agent
   `item/started`/`item/completed`, `item/agentMessage/delta`, and
   `turn/completed` sequence to the requesting Desktop connection.

The synthetic payload follows the Codex 0.146.0 v2 schema, including
`itemsView`, timestamps, nullable turn fields, `clientId`, `text_elements`,
`phase`, and `memoryCitation`. The completed turn contains the final agent
message as a summary item.

If handoff fails, the existing global dirty marker and restore behavior remain
authoritative and the command receives the existing sanitized handoff error. If
selection persistence fails after a verified handoff, the command returns a
sanitized error and no success turn is emitted. The next normal send reads the
last durable selection and reconciles the runtime through the normal handoff
path.

## Packaging

A skills-only plugin lives under `plugins/codex-provider-switcher`. Release
archives include this directory. Remote-host installation copies or symlinks
`plugins/codex-provider-switcher/skills/provider` to
`$HOME/.agents/skills/provider`. The skill disables implicit invocation so
ordinary provider-related prompts do not activate it.

The binary remains usable without installing the skill; exact plain-text
`/provider <name>` control messages still work when a client transmits them.

## Verification

Unit tests cover strict command recognition, malformed input, schema-complete
synthetic events, atomic state persistence, permissions, corrupt state, and
state-directory resolution. Session tests prove the command is not forwarded,
handoff completes before persistence and feedback, normal turns use the stored
provider, resume uses the stored provider, and failures emit no fake success.

The WebSocket integration test sends a real skill-shaped `turn/start` through
the proxy and asserts that the fake lifecycle reaches Desktop while the test
app-server sees no model turn. Existing handoff, peer recovery, race, macOS, and
four-target build checks remain required.
