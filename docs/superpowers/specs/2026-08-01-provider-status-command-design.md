# Provider Status And Explicit Switch Command Design

## Goal

Replace the ambiguous provider control syntax with two explicit operations:

```text
/provider status
/provider switch <name>
```

Desktop skill invocations use the equivalent `$provider status` and
`$provider switch <name>` internal wire forms, which are accepted only with the
corresponding skill metadata. They are not additional user-facing commands.

## Command Grammar

Only exact control inputs are recognized. A command contains one text input,
plus exactly one `provider` skill item for the `$provider` form. Attachments,
extra text, unsafe provider identifiers, missing arguments, and the legacy
`/provider <name>` form are rejected with the existing sanitized JSON-RPC error
path. Ordinary prompts that merely mention the command remain ordinary turns.

## Status Semantics

Status is read-only. It takes the existing per-thread lock, rejects an active
turn, and prepares all cooperating peers, but it never unsubscribes, resumes,
changes provider, or writes selection state.

The response reports two independent facts:

- `Runtime provider` is the provider most recently verified from this
  connection's app-server `thread/start`, `thread/resume`, or `thread/fork`
  response. If unavailable, it is reported as `unknown` rather than inferred.
- `Selected provider` is the durable task selection (or an explicit direct-mode
  provider). Without a selection, it reports `app-server configuration`.

If runtime and selection differ, the response states that the selection will be
applied before the next model turn. This avoids presenting persisted intent as
current runtime state.

## Switch Semantics

`switch` reuses the existing coordinated handoff. It resumes with the requested
provider, verifies the app-server response, resubscribes peers, persists the
selection, and only then emits a local success turn. The original `turn/start`
never reaches app-server.

## Synthetic Turns

Both operations use the existing app-server v2 synthetic lifecycle. The user
item contains the canonical `/provider status` or `/provider switch <name>`
text. The agent item contains the status report or `Provider switched to
<name>.` No model runs, no tokens are consumed, and no control turn is written
to rollout history.

## Compatibility And Release

The skill description, examples, README, architecture documentation, and
repository assertions move to the explicit grammar. Because the pre-1.0 command
surface changes incompatibly, the initial release version is `0.5.0`. Removing
the unintended spaced alias is released as `0.5.1`.

## Verification

Tests cover every accepted transport form, rejection of legacy and malformed
forms, status with matching/different/missing runtime and selection, zero
upstream writes for status, successful switch handoff, app-server v2 synthetic
lifecycle fields, and an end-to-end skill-shaped status then switch sequence.
