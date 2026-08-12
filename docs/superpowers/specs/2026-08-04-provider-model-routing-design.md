# Provider Model Routing Design

## Problem

Provider selection and model selection are independent app-server fields. The
switcher currently changes only `modelProvider`, so a task switched to the
`glm` provider can still send the Desktop-selected `gpt-5.6-sol` model. The GLM
gateway rejects that request. Desktop does not treat `/model` as a command, and
its model picker is populated by app-server `model/list`, which does not include
arbitrary configured model ids.

## Configuration

The switcher owns an optional JSON file next to its task state:

```text
<state-directory>/models.json
```

The default installed location is:

```text
$CODEX_HOME/codex-provider-switcher/models.json
```

The file is a JSON object mapping provider ids to model ids:

```json
{
  "openai": "gpt-5.6-sol",
  "sub2api": "gpt-5.6-sol",
  "glm": "glm-5.2"
}
```

A missing file preserves existing provider-only behavior. An empty object is
valid. Invalid JSON, duplicate keys, invalid provider ids, invalid model ids,
symlinks, non-regular files, or an oversized file make the proxy fail closed
before accepting Desktop traffic. Model ids must be non-empty UTF-8 strings of
at most 256 bytes with no whitespace or control characters. The file is loaded
once per proxy process, so reconnecting Desktop applies changes consistently to
all requests on that connection.

Once a deployment maps one provider to a different model family, it should map
every provider that users switch between. This prevents a task switched back
from GLM from retaining the GLM model under another provider.

## Routing

The effective route is a provider plus an optional configured model.

- `thread/start`, `thread/resume`, and `thread/fork` receive the selected
  `modelProvider`; they also receive `model` when the selected provider has a
  mapping.
- Ordinary `turn/start` keeps the user's input and all unrelated parameters,
  but its `model` field is overwritten when the selected provider has a model
  mapping.
- Internal handoff, resubscription, fresh-thread materialization, and recovery
  resumes apply the same provider-model route.
- A mapped handoff succeeds only if app-server returns both the requested
  `modelProvider` and requested `model`. Unmapped routes retain the existing
  provider-only verification behavior.
- Task selection state remains the provider id. The model is resolved from the
  connection's immutable model map, avoiding a second independently mutable
  per-task selection.
- Recovery journals persist the optional model so crash recovery completes the
  exact route that began the handoff.

Provider control messages remain local and are never replayed. A successful
mapped switch reports both values, for example:

```text
Provider switched to glm using model glm-5.2.
```

`/provider status` reports the verified runtime model and selected mapped model
when available. Its existing provider-only output remains unchanged for
unmapped providers.

## Desktop Display

App-server responses for `thread/start` and `thread/resume` include the active
`model`. Rewriting the route therefore gives Desktop the `glm-5.2` value for
the current thread. Desktop may display that raw id in the current conversation
control, but the switcher does not rewrite `model/list`, so GLM is not added as
a selectable model-picker entry. This avoids coupling the switcher to Codex's
versioned model catalog and capability metadata.

## Compatibility And Safety

- No Android, SSH alias, shell PATH, or provider credential changes are
  required.
- Existing installations without `models.json` retain byte-for-byte behavior
  outside the existing provider rewrites.
- Unknown methods, binary messages, upstream messages, and `model/list` remain
  transparent.
- User input is never replayed or reconstructed.
- The model map contains identifiers only, never credentials.
- Codex CLI remains delegated unchanged by the wrapper.

## Verification

Tests cover strict model-map loading, provider-only compatibility, model
rewrites for every routing method, normal turn enforcement, mapped handoff
verification, peer resubscription, recovery persistence, status/confirmation
text, and an end-to-end GLM route through the fake app-server. Full Go tests,
vet, build, and the existing repository workflow checks must pass before
deployment.
