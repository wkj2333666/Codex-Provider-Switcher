# Architecture

Codex Provider Switcher is a policy layer in front of the official
`codex app-server proxy` command. It intentionally does not implement Codex's
socket transport or app-server protocol beyond four request rewrites.

## Components

`internal/config` resolves flags over environment variables, validates the
provider identifier, verifies the Unix socket, and resolves the Codex
executable to an absolute executable path.

`internal/rewrite` reads newline-delimited JSON without `bufio.Scanner`, so it
does not impose Scanner's 64 KiB token limit. It validates every client line,
rewrites `thread/start`, `thread/resume`, `thread/fork`, and `thread/list`, and
passes all other valid messages through byte-for-byte.

`internal/proxy` starts Codex with an argument array, never a shell. One
goroutine rewrites client input into child stdin while another copies child
stdout untouched. Child stderr is inherited. The package forwards `SIGINT`,
`SIGTERM`, and `SIGHUP`, closes child stdin after Desktop EOF, and kills and
reaps the child after either forwarding direction fails.

`cmd/codex-provider-switcher` owns help, version output, signal subscription,
dependency wiring, diagnostics, and process exit codes.

## Data flow

```text
Desktop stdin
    |
    v
validate JSONL -> enforce provider/list policy -> Codex proxy stdin
                                                  |
                                                  v
                                           app-server socket

Desktop stdout <------------------------- Codex proxy stdout
                   byte-for-byte copy
```

Only client input is parsed. This prevents response-schema assumptions and
keeps authentication traffic and model output outside the switcher's logic.

## Fail-closed behavior

The provider identity is the SSH entry point's routing policy. A missing
identity, invalid identity, missing socket, or unsafe target request fails
before unmodified work can reach the daemon. Unknown valid methods remain
forward-compatible and pass through unchanged.

Error messages never format the input line. A malformed message is identified
only by its line number; a routing shape error also identifies the method.

## Process lifecycle

1. Resolve and validate configuration.
2. Start `<codex> app-server proxy --sock <absolute socket>`.
3. Rewrite client input and copy server output concurrently.
4. On client EOF, close child stdin and continue draining output.
5. Forward supported Unix signals to the child.
6. On forwarding failure, kill and wait for the child.
7. Return a normal child status code unchanged; map other failures to status 1.

The switcher neither opens the app-server socket nor the task database. All
connections must use one separately managed daemon and its effective
`CODEX_HOME`.

## Release model

CI runs formatting checks, unit/integration tests, `go vet`, the race detector,
and cross-builds. Tag pushes matching `v*` repeat the quality gate, inject the
tag into `--version`, and package these targets:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`

Each archive includes the binary, README, and MIT license. The publication job
is isolated from build jobs and is the only job granted `contents: write`.
