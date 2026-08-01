# App-Server Default Provider Design

## Goal

Let Codex app-server select the provider for tasks without a saved switcher
selection. Stock Desktop requires no provider environment variable.

## Resolution

An empty connection provider means no override. `thread/start`,
`thread/resume`, and `thread/fork` pass through byte-for-byte until a task has a
saved selection. App-server resolves its effective provider through its own
configuration layers and returns that provider in the thread response; the
session records the response for subsequent turn coordination.

The direct `proxy --provider <id>` flag remains an explicit override for tests
and custom integrations. Saved per-task selections override app-server defaults
on resume and send. Exact `/provider <id>` controls persist a selection and use
the existing coordinated handoff.

## Failure Policy

An ordinary turn without a saved selection requires a previously observed
effective provider from app-server. Missing response state, corrupt selections,
and handoff failures fail closed. No TOML parser or duplicated Codex default is
introduced.

## Verification

Tests cover empty provider configuration, ignored legacy environment values,
byte-for-byte default requests, effective-provider tracking, ordinary sends
without handoff, saved-selection precedence, CLI help, and stock WebSocket flow.
Full tests, vet, race CI, four cross-builds, real WebSocket handshake, and
unchanged daemon PID remain release gates.
