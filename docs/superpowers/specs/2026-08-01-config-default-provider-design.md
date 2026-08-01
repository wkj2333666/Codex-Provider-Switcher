# Codex Config Default Provider Design

## Goal

Use Codex's own `config.toml` model-provider default for tasks without a saved
selection, eliminating `CODEX_PROVIDER_SWITCHER_PROVIDER` from stock Desktop
configuration.

## Resolution

The switcher resolves the Codex home from `CODEX_HOME`, then a stock
`app-server-control.sock` path, then `$HOME/.codex`. It reads that home's
`config.toml` with a TOML parser. A top-level `model_provider` string selects the
default provider. If the file or field is absent, the built-in Codex default is
`openai`.

The direct `proxy --provider <id>` flag remains an explicit override for tests
and custom integrations. `CODEX_PROVIDER_SWITCHER_PROVIDER` is not read. Saved
per-task selections continue to override the connection default.

## Failure Policy

Unreadable files other than absence, oversized files, invalid TOML, non-string
`model_provider`, and provider identifiers outside the existing grammar fail
closed with sanitized errors. The parser reads at most 1 MiB and does not log
paths or values.

## Dependencies And Release

Use `github.com/pelletier/go-toml/v2` instead of line-oriented parsing. Include
its MIT license in `THIRD_PARTY_NOTICES`. Update CLI help, repository checks,
README, architecture, plugin version, CI, release, and the installed wrapper.

## Verification

Tests cover explicit `model_provider`, implicit `openai`, ignored legacy
environment values, explicit flag precedence, invalid TOML and values, Codex
home resolution, and unchanged saved-selection precedence. Full tests, vet,
race CI, four cross-builds, real WebSocket handshake, and unchanged daemon PID
remain release gates.
