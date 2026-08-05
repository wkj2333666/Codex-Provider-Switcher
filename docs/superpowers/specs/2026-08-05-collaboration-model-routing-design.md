# Collaboration Model Routing Design

## Problem

Codex 0.146 gives `turn/start.params.collaborationMode` precedence over the
top-level `model`. Desktop sends the selected UI model in both
`params.model` and `params.collaborationMode.settings.model`. The switcher
currently rewrites only the top-level field, so a stale Desktop model can
override a verified provider/model route. Desktop also sends
`thread/settings/update` before turns, which currently bypasses task-specific
routing and can make the switcher's verified runtime cache stale.

## Decision

The selected provider route remains authoritative for model-bearing requests.
One rewrite helper will set the top-level `model` and, when a non-null
`collaborationMode` is present, its `settings.model`. The helper applies to
provider-bearing thread start/resume/fork requests, ordinary turns, and
task-selected `thread/settings/update` requests.

`thread/settings/update` remains byte-transparent when the task has no saved
provider selection. A selected update is serialized by the existing per-thread
coordinator lock and uses the immutable catalog route. It does not initiate a
provider handoff or save new state; the next ordinary turn retains responsibility
for reconciling a provider mismatch.

## Safety

- Preserve every unrelated JSON field semantically.
- Treat a present non-object `collaborationMode`, missing/non-object
  `collaborationMode.settings`, or another unsafe target shape as a routing
  policy error.
- Do not add `modelProvider` to `turn/start` or `thread/settings/update`.
- Do not rewrite settings for tasks without a saved selection.
- Keep exact provider controls local and preserve their input semantics.

## Verification

Unit tests cover nested model precedence, settings updates, null collaboration
mode, malformed nested structures, unselected transparency, and input
preservation. Existing integration and repository tests remain the regression
gate. A host build and Linux/Darwin cross-builds verify release compatibility.

