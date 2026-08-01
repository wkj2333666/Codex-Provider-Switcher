# Desktop Markdown Provider Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make v0.5.2 handle Codex Desktop's single-text-item `[$provider](absolute-path/provider/SKILL.md) ...` encoding locally without intercepting ordinary mentions.

**Architecture:** Extend the existing strict provider-command parser with one anchored Markdown skill-link marker. Reuse the current status, verified handoff, and synthetic lifecycle paths unchanged, with exact wire-shape coverage at parser, session, and WebSocket boundaries.

**Tech Stack:** Go 1.24, standard-library `path` and `strings`, coder/websocket, JSON-RPC v2, GitHub Actions.

## Global Constraints

- Release version is `0.5.2`.
- The Markdown label is exactly `$provider`.
- The target is a non-empty absolute POSIX path whose cleaned form ends in `/provider/SKILL.md`.
- The Markdown form requires exactly one valid text input item.
- Only `status` and `switch <name>` are controls; provider IDs retain the existing ASCII grammar.
- Embedded mentions, prose prefixes, wrong labels, relative or unrelated paths, and code examples remain ordinary model input.
- A valid provider link with malformed arguments fails closed with the existing sanitized error.
- Status performs no resume, handoff, selection write, or upstream model turn.
- Switch retains the existing idle-task handoff and returned-provider verification.
- Existing slash and separate-skill-item forms remain supported.
- Parsing performs no filesystem reads and diagnostics do not include the path or user input.
- Deployment does not restart app-server; live proxies reconnect before using v0.5.2.

---

### Task 1: Strict Markdown Skill-Link Transport Compatibility

**Files:**
- Modify: `internal/transport/provider_command.go`
- Modify: `internal/transport/provider_command_test.go`
- Modify: `internal/transport/session_test.go`
- Modify: `internal/transport/handoff_integration_test.go`

**Interfaces:**
- Consumes: `parseProviderCommand(message rpcMessage) (providerCommand, bool, error)` and existing status/switch session handling.
- Produces: `parseProviderMarkdownArguments(text string) ([]string, bool)`. True means the text begins with an exact provider label and a valid provider-skill path; the returned fields go through the existing action grammar.

- [ ] **Step 1: Add valid parser cases**

Add these entries to `TestParseProviderCommandRecognizesStrictControlInputs`:

~~~go
{
	name: "Desktop Markdown status",
	input: `{"id":5,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"  [$provider](/home/user/.agents/skills/provider/SKILL.md) status  ","text_elements":[]}]}}`,
	action: providerCommandStatus,
},
{
	name: "Desktop Markdown switch",
	input: `{"id":6,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/Users/test/.agents/skills/provider/SKILL.md) switch provider_1.test"}]}}`,
	action: providerCommandSwitch,
	provider: "provider_1.test",
},
~~~

- [ ] **Step 2: Add forwarding and rejection cases**

Add these to `TestParseProviderCommandForwardsOrdinaryMessages`:

~~~go
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"Explain [$provider](/home/user/.agents/skills/provider/SKILL.md) status"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$other](/home/user/.agents/skills/provider/SKILL.md) status"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](relative/provider/SKILL.md) status"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/other/SKILL.md) status"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider)/SKILL.md) status"}]}}`,
~~~

Add these to `TestParseProviderCommandRejectsMalformedControlsWithoutLeaking`:

~~~go
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md)"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md) switch secret/bad"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md) status trailing"}]}}`,
`{"id":1,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md) status"},{"type":"image","url":"secret-url"}]}}`,
~~~

Keep the existing assertion that sanitized errors never contain `secret`. Add a fenced-code example containing the Markdown link to the forwarding table.

- [ ] **Step 3: Add session and end-to-end coverage**

Convert `TestSessionProviderCommandRejectsMalformedInputWithoutForwarding` to a table containing its existing slash request and:

~~~go
`{"id":22,"method":"turn/start","params":{"threadId":"thr-a","input":[{"type":"text","text":"[$provider](/home/user/.agents/skills/provider/SKILL.md) status trailing"}]}}`
~~~

For each request, require zero upstream messages and one downstream JSON-RPC error with `providerCommandErrorCode` and `invalidProviderCommandMessage`.

Rename `TestProviderSkillCommandSwitchesWithoutModelTurn` to `TestProviderDesktopMarkdownSkillCommandSwitchesWithoutModelTurn`. Replace both two-item skill inputs with the observed one-item shape:

~~~go
"input": []any{
	map[string]any{
		"type": "text",
		"text": "[$provider](/home/user/.agents/skills/provider/SKILL.md) status",
		"text_elements": []any{},
	},
},
~~~

Use the same shape with `switch sub2api` for the switch. Retain the eight-message lifecycle, zero app-server `turn/start` calls, and next ordinary turn provider assertions.

- [ ] **Step 4: Run focused tests and confirm RED**

~~~bash
go test ./internal/transport -run 'TestParseProviderCommand|TestSessionProviderCommandRejectsMalformedInputWithoutForwarding|TestProviderDesktopMarkdownSkillCommandSwitchesWithoutModelTurn' -count=1
~~~

Expected: the valid Markdown cases and integration test fail because v0.5.1 forwards the link text.

- [ ] **Step 5: Implement the minimal parser**

Add `path` to `provider_command.go` imports and add:

~~~go
const providerMarkdownPrefix = "[$provider]("

func parseProviderMarkdownArguments(text string) ([]string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, providerMarkdownPrefix) {
		return nil, false
	}
	remainder := trimmed[len(providerMarkdownPrefix):]
	closing := strings.IndexByte(remainder, ')')
	if closing < 0 {
		return nil, false
	}
	skillPath := remainder[:closing]
	cleaned := path.Clean(skillPath)
	if skillPath == "" || !path.IsAbs(skillPath) ||
		!strings.HasSuffix(cleaned, "/provider/SKILL.md") {
		return nil, false
	}
	return strings.Fields(remainder[closing+1:]), true
}
~~~

In the existing text-item loop, keep direct `/provider` and `$provider` handling. When neither matches, call `parseProviderMarkdownArguments`; on success set `marker = providerMarkdownPrefix` and use the returned arguments.

Add this validation beside the existing skill-item branch:

~~~go
} else if commandMarker == providerMarkdownPrefix {
	if len(items) != 1 {
		return providerCommand{}, true, errors.New(invalidProviderCommandMessage)
	}
} else if len(items) != 1 {
~~~

Do not change status, handoff, selection, or lifecycle code.

- [ ] **Step 6: Run GREEN verification**

~~~bash
gofmt -w internal/transport/provider_command.go internal/transport/provider_command_test.go internal/transport/session_test.go internal/transport/handoff_integration_test.go
go test ./internal/transport -run 'TestParseProviderCommand|TestSessionProviderCommandRejectsMalformedInputWithoutForwarding|TestProviderDesktopMarkdownSkillCommandSwitchesWithoutModelTurn' -count=1
go test ./internal/transport -count=1
go test ./internal/transport -run TestProviderDesktopMarkdownSkillCommandSwitchesWithoutModelTurn -count=20
~~~

Expected: every command exits zero and the repeated test completes twenty times without an upstream provider-control model turn.

- [ ] **Step 7: Commit**

~~~bash
git add internal/transport/provider_command.go internal/transport/provider_command_test.go internal/transport/session_test.go internal/transport/handoff_integration_test.go
git commit -m "fix: accept Desktop Markdown provider commands"
~~~

---

### Task 2: Public Contract And v0.5.2 Metadata

**Files:**
- Modify: `internal/repository/workflows_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `plugins/codex-provider-switcher/.codex-plugin/plugin.json`
- Modify: `plugins/codex-provider-switcher/skills/provider/SKILL.md`

**Interfaces:**
- Consumes: the strict Markdown grammar from Task 1.
- Produces: documentation and plugin metadata describing v0.5.2 and the current Desktop wire form.

- [ ] **Step 1: Add failing repository assertions**

In `TestDocumentationDescribesStockDesktopWrapper` add:

~~~go
"`[$provider](<absolute-path>/provider/SKILL.md) status`",
"whole input contains exactly one text item",
"ordinary mentions are not controls",
~~~

Add `"any occurrence of `[$provider]`"` to the obsolete claims. In `TestProviderPluginExposesExplicitStatusAndSwitchCommands` change the expected version to `0.5.2`.

- [ ] **Step 2: Run repository tests and confirm RED**

~~~bash
go test ./internal/repository -run 'TestDocumentationDescribesStockDesktopWrapper|TestProviderPluginExposesExplicitStatusAndSwitchCommands' -count=1
~~~

Expected: missing documentation strings and version 0.5.1 failures.

- [ ] **Step 3: Update documentation and plugin files**

After the command examples in `README.md`, add:

~~~text
When selected from the current Desktop skill menu, the command arrives as a
single text item such as
`[$provider](<absolute-path>/provider/SKILL.md) status`. The switcher
accepts that internal form only when the whole input contains exactly one text
item and the linked absolute path ends in `/provider/SKILL.md`;
ordinary mentions are not controls.
~~~

In `docs/architecture.md`, replace the claim that Desktop always emits `$provider ...` plus a skill item. Document the current one-text-item Markdown form, retained legacy form, whole-input anchoring, absolute suffix validation, and absence of filesystem reads.

Set the plugin manifest version to `0.5.2`. Replace the skill's Desktop paragraph with:

~~~text
Desktop may encode the selected skill as a Markdown link whose label is
`$provider`. Submit only the requested status or switch control,
without adding prose, attachments, or other input items. The transport
switcher recognizes the exact Desktop encoding and handles it locally; do not
answer the control through a model or tool.
~~~

- [ ] **Step 4: Verify repository contract and official validators**

~~~bash
gofmt -w internal/repository/workflows_test.go
go test ./internal/repository -count=1
python3 /home/wkj/.codex/skills/.system/skill-creator/scripts/quick_validate.py plugins/codex-provider-switcher/skills/provider
python3 /home/wkj/.codex/skills/.system/plugin-creator/scripts/validate_plugin.py plugins/codex-provider-switcher
~~~

Expected: tests pass, the skill validator reports `Skill is valid!`, and the plugin validator reports `Plugin validation passed`.

- [ ] **Step 5: Commit**

~~~bash
git add README.md docs/architecture.md internal/repository/workflows_test.go plugins/codex-provider-switcher/.codex-plugin/plugin.json plugins/codex-provider-switcher/skills/provider/SKILL.md
git commit -m "docs: document Desktop provider skill encoding"
~~~

---

### Task 3: Verification, Review, Release, And Deployment

**Files:**
- No source changes unless review identifies a defect.

**Interfaces:**
- Consumes: reviewed Task 1 transport behavior and Task 2 public contract.
- Produces: merged v0.5.2, eight verified release assets, installed ARM64 wrapper and skill, and Desktop acceptance evidence.

- [ ] **Step 1: Run the full local gate**

~~~bash
test -z "$(gofmt -l cmd internal)"
go mod tidy
git diff --exit-code -- go.mod go.sum
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/cps-v0.5.2-linux-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/cps-v0.5.2-linux-arm64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o /tmp/cps-v0.5.2-darwin-amd64 ./cmd/codex-provider-switcher
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/cps-v0.5.2-darwin-arm64 ./cmd/codex-provider-switcher
git diff --check main...HEAD
git status --short
~~~

Expected: zero exits and no uncommitted files.

- [ ] **Step 2: Review against the spec**

Use `superpowers:requesting-code-review` on `main...HEAD`. Require findings for whole-input anchoring, exact-one-item enforcement, wrong labels and paths, ordinary mentions, malformed fail-closed behavior, zero upstream model turns, unchanged status/switch semantics, sanitized diagnostics, and backward compatibility. Fix each finding with a focused RED/GREEN test, rerun Step 1, and commit it.

- [ ] **Step 3: Push a ready PR and wait for CI**

Use `github:yeet` to push `codex/desktop-markdown-provider-command` and open a ready PR titled `fix: accept Desktop Markdown provider commands`. The body describes the observed Desktop shape, strict boundary, zero-model-turn integration proof, and v0.5.2 metadata. Wait for Go quality, macOS, and four cross-build checks. Use `github:gh-fix-ci` before changing code if a check fails.

- [ ] **Step 4: Merge and publish v0.5.2**

After approval and green CI, merge without rewriting unrelated history:

~~~bash
git switch main
git pull --ff-only origin main
git tag -a v0.5.2 -m "v0.5.2"
git push origin v0.5.2
gh release view v0.5.2 --repo wkj2333666/Codex-Provider-Switcher --json tagName,isDraft,isPrerelease,publishedAt,url,assets
~~~

Expected: a non-draft, non-prerelease release with four archives and four SHA-256 files.

- [ ] **Step 5: Verify the Linux ARM64 artifact**

~~~bash
release_dir="$(mktemp -d /tmp/cps-v0.5.2-release.XXXXXX)"
gh release download v0.5.2 --repo wkj2333666/Codex-Provider-Switcher --pattern 'codex-provider-switcher-0.5.2-linux-arm64.tar.gz*' --dir "$release_dir"
(cd "$release_dir" && sha256sum -c codex-provider-switcher-0.5.2-linux-arm64.tar.gz.sha256)
tar -C "$release_dir" -xzf "$release_dir/codex-provider-switcher-0.5.2-linux-arm64.tar.gz"
"$release_dir/codex-provider-switcher-0.5.2-linux-arm64/codex-provider-switcher" --version
~~~

Expected: checksum OK and `codex-provider-switcher v0.5.2`.

- [ ] **Step 6: Deploy without restarting app-server**

Record the socket owner PID and start time:

~~~bash
socket_path="$HOME/.codex/app-server-control/app-server-control.sock"
daemon_pid="$(fuser "$socket_path" 2>/dev/null)"
test -n "$daemon_pid"
fuser -v "$socket_path"
ps -p "$daemon_pid" -o pid=,lstart=,etimes=,args=
~~~

Install the binary and skill with recoverable backups:

~~~bash
archive_root="$release_dir/codex-provider-switcher-0.5.2-linux-arm64"
install_root="$HOME/.local/lib/codex-provider-switcher"
binary_stage="$install_root/.codex-provider-switcher.v0.5.2.$$"
test ! -e "$install_root/codex-provider-switcher.v0.5.1"
cp -p "$install_root/codex-provider-switcher" "$install_root/codex-provider-switcher.v0.5.1"
install -m 0755 "$archive_root/codex-provider-switcher" "$binary_stage"
mv -f "$binary_stage" "$install_root/codex-provider-switcher"

skill_root="$HOME/.agents/skills"
skill_stage="$(mktemp -d "$skill_root/.provider.v0.5.2.XXXXXX")"
cp -R "$archive_root/plugins/codex-provider-switcher/skills/provider/." "$skill_stage/"
test ! -e "$skill_root/provider.v0.5.1"
mv "$skill_root/provider" "$skill_root/provider.v0.5.1"
mv "$skill_stage" "$skill_root/provider"
~~~

Do not signal app-server or live proxies. If any install command fails, restore
the preserved binary or skill before continuing.

Verify with:

~~~bash
command -v codex
codex --version
"$install_root/codex-provider-switcher" --version
cmp -s "$archive_root/codex-provider-switcher" "$install_root/codex-provider-switcher"
fuser -v "$socket_path"
ps -p "$daemon_pid" -o pid=,lstart=,etimes=,args=
~~~

Expected: delegated Codex is 0.146.0, wrapper is v0.5.2, `cmp` exits zero,
and the daemon PID/start time is unchanged.

- [ ] **Step 7: Run host transport and Desktop acceptance**

Reconnect the single `pi` Desktop entry so a new proxy loads v0.5.2. Confirm stock and Android-shaped Upgrade requests each return `HTTP/1.1 101 Switching Protocols`. Invoke:

~~~text
/provider status
/provider switch sub2api
/provider status
/provider switch openai
/provider status
~~~

Expected: local synthetic responses, no model-generated `Provider status` reply, verified runtime after each switch, and unchanged task ID/history. Retain or remove the v0.5.1 skill backup according to the user's rollback preference.
