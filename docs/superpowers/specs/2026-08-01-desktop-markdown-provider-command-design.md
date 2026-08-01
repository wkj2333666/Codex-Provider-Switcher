# Desktop Markdown Provider Command Compatibility

## Goal

Accept the provider skill invocation format emitted by the current Codex
Desktop client without broadening provider controls to ordinary mentions. The
new compatibility form is released as `v0.5.2`.

The observed Desktop input is a single text item such as:

```text
[$provider](/home/user/.agents/skills/provider/SKILL.md) status
```

Codex Provider Switcher must handle that input locally, just like
`/provider status`, without starting a model turn.

## Accepted Inputs

The existing public controls remain unchanged:

```text
/provider status
/provider switch <name>
```

The existing internal form, consisting of `$provider ...` text plus one
separate `provider` skill item, remains supported.

One additional internal form is accepted when the entire `turn/start` input
contains exactly one text item:

```text
[$provider](<skill-path>) status
[$provider](<skill-path>) switch <name>
```

The Markdown label must be exactly `$provider`. The link target must be a
non-empty absolute POSIX path whose cleaned form ends in
`/provider/SKILL.md`. The path is not tied to a username or home directory and
is not required to exist at parse time. Link targets containing a closing
parenthesis or a line break are outside this compatibility form.

Leading and trailing whitespace around the complete invocation is ignored.
Whitespace between the link and command arguments follows the existing
field-based command parsing. Provider identifiers retain the existing ASCII
letters, digits, `.`, `_`, and `-` grammar.

## Recognition Boundary

Recognition is anchored to the complete text item. Merely containing
`[$provider]` does not make ordinary text a control. These examples remain
ordinary model input:

```text
Explain [$provider](/home/user/.agents/skills/provider/SKILL.md) status.
Here is an example: [$provider](/path/provider/SKILL.md) switch openai
[$other](/path/provider/SKILL.md) status
[$provider](relative/provider/SKILL.md) status
```

If an input starts with a structurally valid provider skill link but has a
missing, malformed, or extra command argument, the switcher recognizes it as
an attempted control and returns the existing sanitized invalid-command error.
It must not forward that attempted control to a model. A different label,
relative or unrelated path, embedded mention, or prose before the link is not
a provider control and passes through unchanged.

The Markdown form is rejected when the input array contains any additional
text, image, attachment, skill, or malformed item. This preserves the rule that
provider controls carry no unrelated user content.

## Parser Design

`parseProviderCommand` gains a small parser for the anchored Markdown link
form. It runs alongside, rather than replacing, the existing compact slash and
skill-item forms.

The parser performs these steps:

1. Trim outer whitespace and require the exact `[$provider](` prefix.
2. Split the link target at its one allowed closing `)` and validate the path.
3. Require that the request contains exactly one valid text input item.
4. Parse the remaining fields with the existing `status` and `switch` action
   rules.
5. Return the same canonical command text and local status or switch lifecycle
   used by the existing forms.

No filesystem reads, skill-file parsing, Codex configuration reads, or path
hard-coding are introduced. The path is only a strict marker matching the
Desktop encoding observed at the WebSocket boundary.

## Error And Security Behavior

The compatibility change preserves fail-closed routing:

- A recognized malformed control receives the sanitized
  `invalid provider command` response and causes no upstream model turn.
- A valid status command performs no resume, handoff, or provider-state write.
- A valid switch command retains the existing coordinated idle-task handoff
  and verified-provider requirements.
- Diagnostics never include the supplied skill path, provider input, prompts,
  or credentials.
- Ordinary mentions and code examples are not intercepted.

## Verification

Parser tests cover valid status and switch commands with different absolute
home paths, surrounding whitespace, and valid provider identifiers. Negative
tests cover relative paths, wrong suffixes, wrong labels, empty targets,
closing-parenthesis ambiguity, trailing arguments, embedded mentions, prose
prefixes, and additional input items.

An end-to-end transport test sends the exact observed single-text-item Desktop
shape and proves that status and switch controls produce local synthetic turns
with zero upstream model turns. Existing strict-command, handoff, WebSocket,
repository, race, vet, and four-platform build checks remain release gates.

Deployment replaces the Linux ARM64 wrapper and provider skill without
restarting app-server. Existing live proxy processes must reconnect before
they can use `v0.5.2`. Final Desktop acceptance invokes the provider skill from
the slash menu and verifies both status and two-direction switching.
