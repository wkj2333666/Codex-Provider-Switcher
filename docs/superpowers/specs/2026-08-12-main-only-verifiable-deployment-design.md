# Main-Only Verifiable Deployment Design

## Summary

The installed switcher currently comes from commit `745d78b` on the
`system-error-provider-recovery` branch, while the primary checkout is on
`main` at `48bca07`. The binary itself is correct, but the split source of truth
makes a valid deployment look stale and makes later upgrades difficult to
audit.

This change makes `main` the only supported deployment source, records the
exact source identity inside every binary, and provides one guarded local
deployment command. A deployed executable becomes self-describing without
depending on the checkout that happens to be open later.

## Goals

- Merge the complete recovery and model-routing implementation into `main`
  without rewriting either branch's history.
- Make an installed binary report its release version, commit, source branch,
  and UTC build time.
- Keep the existing concise `--version` interface useful for humans.
- Add a machine-readable `--build-info` interface for deployment checks.
- Refuse local deployment from a dirty worktree, a non-`main` branch, or a
  commit that is not the current pushed `origin/main`.
- Build and install atomically without retaining the previous executable.
- Clean Go build and test caches after every Go test or build command.
- Verify the installed executable against the source commit before reporting
  deployment success.

## Non-Goals

- Supervising or restarting the shared Codex app-server daemon.
- Changing Codex, Android SSH Codex, shell PATH policy, credentials, provider
  selections, or task databases.
- Preserving old deployment binaries or rollback copies.
- Turning the local deployment script into a general release manager.

## Source Reconciliation

`system-error-provider-recovery` is merged into `main` with a normal merge
commit. The existing `main` design commit and all feature commits remain in
history. Conflicts in the recovery design document are resolved in favor of
the implemented feature branch, while retaining any newer non-conflicting
clarifications from `main`.

After verification, `main` is pushed. The feature branch may remain as a
historical ref, but it is not a supported deployment source.

## Embedded Build Information

The command package exposes four link-time variables:

- `version`: semantic release tag or local label;
- `commit`: full Git commit SHA;
- `source`: source branch, which must be `main` for local deployment;
- `builtAt`: RFC 3339 UTC timestamp.

Developer builds retain explicit defaults (`dev`, `unknown`, `unknown`, and
`unknown`) so ordinary `go test` and `go build` remain usable.

`--version` prints one stable human-readable line containing the version and
short commit. `--build-info` prints one JSON object with the four complete
fields. JSON output is preferred for scripts so no deployment check parses
human text.

Release builds populate the same fields from the tag, `GITHUB_SHA`, the
release source name, and a UTC timestamp. Local builds use the label
`main-<short-sha>`.

## Guarded Local Deployment

`scripts/deploy-local.sh` is the single documented local deployment path. It
performs these checks before creating an install candidate:

1. The checkout is exactly on branch `main`.
2. The worktree and index are clean.
3. `origin/main` exists locally.
4. `HEAD` equals `origin/main`; an unpushed or stale main is rejected.
5. The requested target is the host's `linux/arm64` platform.

The script runs the repository tests, cleans Go caches, builds a static arm64
binary with embedded build information, cleans Go caches again, and validates
the candidate's `--build-info` output. It then stages the candidate inside the
installation directory with mode `0755` and renames it over the installed
binary. It never creates a backup.

The installed executable is queried again. The script fails unless its commit,
source, and build time equal the candidate metadata. Temporary build and
staging files are removed on both success and failure.

The script does not kill active proxy processes. Its final message states that
Desktop Remote SSH must reconnect before existing processes use the new
executable.

## Release Workflow

The GitHub release workflow passes all four link-time values. Its existing
cross-platform archive layout and checksums remain unchanged. Release
verification invokes `--build-info` for each native testable artifact where
the runner architecture permits it; repository tests verify the workflow text
contains every required linker variable.

## Failure Handling

- Any source guard failure occurs before build or installation.
- Test or build failure leaves the installed executable untouched.
- Candidate metadata mismatch leaves the installed executable untouched.
- Atomic rename is the only operation that replaces the executable.
- Post-install metadata mismatch is reported as a hard failure; no old binary
  is retained or automatically restored.
- Cache cleanup runs through shell traps even when testing or building fails.

## Testing

Tests cover:

- default and fully populated human version output;
- valid JSON `--build-info` output;
- correct delegation when the executable is invoked as `codex`;
- deployment guard rejection for non-main, dirty, unpushed, and stale states;
- candidate metadata validation without modifying a real installation;
- release workflow inclusion of all linker variables;
- the full Go test and vet suites;
- linux arm64 and amd64 plus Darwin amd64 and arm64 builds;
- cache cleanup and absence of temporary or backup artifacts.

Every Go test, vet, or build command used during implementation and deployment
is followed by `go clean -cache -testcache`.

## Acceptance Criteria

- `main` contains the feature implementation and is pushed to `origin/main`.
- A clean checkout of `origin/main` can reproduce the installed binary's
  reported commit and source.
- The installed executable reports `source: main` and the exact pushed main
  commit through `--build-info`.
- Local deployment from any other source fails before installation.
- No deployment backup or temporary artifact remains.
- Reconnecting Desktop Remote SSH starts proxy processes from the newly
  installed executable.
