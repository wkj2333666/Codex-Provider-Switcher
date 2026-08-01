# User-Friendly Bilingual README Design

## Goal

Replace the current implementation-heavy README with two concise, user-facing
guides that make installation and day-to-day use straightforward:

- `README.md` is the canonical English guide.
- `README.zh-CN.md` is a complete Simplified Chinese counterpart.
- Each file links to the other at the top.

The primary reader is a Codex Desktop Remote SSH user who wants one SSH entry
and per-task provider switching. Internal protocol and handoff implementation
details are not part of the main reading path.

## Information Architecture

Both README files use the same section order:

1. Short description and project disclaimer.
2. What the switcher provides.
3. Requirements and compatibility warning.
4. Install from a GitHub Release.
5. Configure the remote login shell.
6. Connect Desktop with one SSH alias.
7. Use `/provider status` and `/provider switch <name>`.
8. Upgrade.
9. Troubleshooting.
10. Uninstall.
11. Development and advanced documentation links.
12. License.

The opening explanation is limited to the user-visible model: the wrapper
intercepts `codex app-server proxy`, delegates other commands to the real Codex,
and keeps the existing daemon and task store. A small text flow may be retained
when it makes this relationship clearer.

## Deployment Experience

The primary installation path is a manual GitHub Release installation. Commands
must be copyable and use a version variable plus detected OS and architecture.
The guide must:

- preserve the real `codex` path before placing the wrapper first in `PATH`;
- download both the archive and adjacent SHA-256 file;
- verify the checksum before extraction;
- install the wrapper under `$HOME/.local/lib/codex-provider-switcher`;
- install the explicit provider skill under `$HOME/.agents/skills/provider`;
- show the two required login-shell exports;
- verify wrapper discovery, delegated Codex version, and switcher version;
- tell the user to reconnect the Desktop SSH host after installation or upgrade.

No one-line remote shell installer is added. Source builds remain available in
the Development section but are not presented as the normal deployment path.

The upgrade section repeats only the operations that matter: download and
verify the new release, replace the binary and skill, and reconnect Desktop.
It does not preserve an old-version backup by default.

## Content Moved Out Of The README

The following subjects belong in `docs/architecture.md` and are linked from the
README instead of being explained inline:

- direct proxy mode and every CLI/environment precedence rule;
- JSON-RPC method-by-method rewriting;
- peer coordination and handoff phases;
- dirty markers, restore behavior, runtime sockets, and locking;
- WebSocket framing, compression, message limits, and header forwarding;
- detailed threat model and diagnostic sanitization rules.

Existing accuracy requirements are preserved. If a fact required by repository
tests currently exists only in the README, it must be retained in the concise
user guide when user-relevant or moved to `docs/architecture.md` when advanced.

## Style

- Lead with tasks and outcomes, not protocol history.
- Prefer short paragraphs and numbered deployment steps.
- Keep commands complete and directly runnable.
- Explain placeholders immediately beside the command that uses them.
- Avoid repeating the same behavior in multiple sections.
- Keep the English and Chinese versions structurally equivalent, while using
  natural wording rather than sentence-by-sentence literal translation.

## Verification

The documentation change is complete when:

- both README files contain matching sections and commands;
- all relative links resolve to repository files;
- shell snippets pass syntax checks where practical;
- repository documentation tests pass;
- the release-install path includes archive checksum verification;
- obsolete command forms are not reintroduced;
- `go test ./...` and `go vet ./...` pass;
- a diff review confirms that advanced content removed from the README remains
  available in `docs/architecture.md`.
