package repository

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestWorkflowActionsArePinnedToFullSHAs(t *testing.T) {
	for _, name := range []string{"ci.yml", "release.yml"} {
		content := readWorkflow(t, name)
		uses := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*[^@\s]+@([^\s#]+)`).FindAllStringSubmatch(content, -1)
		if len(uses) == 0 {
			t.Fatalf("%s has no third-party actions", name)
		}
		for _, match := range uses {
			if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(match[1]) {
				t.Errorf("%s contains unpinned action reference @%s", name, match[1])
			}
		}
	}
}

func TestCIContainsRequiredQualityAndBuildGates(t *testing.T) {
	content := readWorkflow(t, "ci.yml")
	for _, required := range []string{
		"gofmt -l", "go vet ./...", "go test ./...", "go test -race ./...",
		"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64",
		"contents: read", "pull_request:", "workflow_dispatch:", "go mod tidy",
		"git diff --exit-code -- go.mod go.sum",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("ci.yml missing %q", required)
		}
	}
}

func TestReleaseContainsRequiredTargetsChecksumsAndPermissions(t *testing.T) {
	content := readWorkflow(t, "release.yml")
	for _, required := range []string{
		`- "v*"`, "go test -race ./...", "linux-amd64", "linux-arm64",
		"darwin-amd64", "darwin-arm64", "sha256sum", "gh release create",
		"--generate-notes", "contents: read", "contents: write",
		"go mod tidy", "git diff --exit-code -- go.mod go.sum",
		`bash scripts/validate-release-tag.sh "$GITHUB_REF_NAME"`,
		`cp README.md LICENSE THIRD_PARTY_NOTICES "$root/"`,
		`tar -tzf "dist/${name}.tar.gz" | grep -Fx "${name}/THIRD_PARTY_NOTICES"`,
		"Validate provider plugin",
		"python3 -m json.tool plugins/codex-provider-switcher/.codex-plugin/plugin.json",
		"allow_implicit_invocation: false",
		`cp -R plugins/codex-provider-switcher "$root/plugins/"`,
		`tar -tzf "dist/${name}.tar.gz" | grep -Fx "${name}/plugins/codex-provider-switcher/skills/provider/SKILL.md"`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("release.yml missing %q", required)
		}
	}
	if count := strings.Count(content, "contents: write"); count != 1 {
		t.Errorf("release.yml has %d contents: write grants, want 1", count)
	}
}

func TestThirdPartyNoticesContainsCoderWebSocketLicense(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "THIRD_PARTY_NOTICES"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "github.com/coder/websocket v1.8.15") {
		t.Error("THIRD_PARTY_NOTICES missing pinned coder/websocket version")
	}
	const license = `Copyright (c) 2025 Coder

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.`
	if !strings.Contains(string(content), license) {
		t.Error("THIRD_PARTY_NOTICES does not contain the complete coder/websocket ISC license")
	}
}

func TestReleaseTagValidation(t *testing.T) {
	root := repositoryRoot(t)
	script := filepath.Join(root, "scripts", "validate-release-tag.sh")
	tests := []struct {
		tag   string
		valid bool
	}{
		{tag: "v0.1.0", valid: true},
		{tag: "v10.20.30", valid: true},
		{tag: "v1.2.3-alpha.1", valid: true},
		{tag: "v1.2.3-alpha+build.01", valid: true},
		{tag: "v1.2.3-."},
		{tag: "v1.2.3-01"},
		{tag: "v1.2.3-a..b"},
		{tag: "v1.2.3+.."},
		{tag: "v01.2.3"},
		{tag: "1.2.3"},
		{tag: "v1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			command := exec.Command("bash", script, tt.tag)
			err := command.Run()
			if tt.valid && err != nil {
				t.Errorf("valid tag rejected: %v", err)
			}
			if !tt.valid && err == nil {
				t.Error("invalid tag accepted")
			}
		})
	}
}

func TestDocumentationDescribesStockDesktopWrapper(t *testing.T) {
	root := repositoryRoot(t)
	read := func(name string) string {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	readme := read("README.md")
	architecture := read("docs/architecture.md")
	combined := readme + "\n" + architecture

	for _, required := range []string{
		"WebSocket", "ln -s", "app-server configuration",
		"v0.1.0", "Uninstall", "64 MiB",
		"v0.2.0 is also incompatible with stock Desktop Remote SSH",
		"`codex app-server proxy` without `--sock`",
		"CODEX_PROVIDER_SWITCHER_SOCKET", "--strict-config",
		"same Unix user", "downstream `request.Host`", "THIRD_PARTY_NOTICES",
		"send-time provider handoff", "`turn/start`", "Codex CLI 0.146.0",
		"active turn", "-32090", "/tmp/cps-", "resubscribe detached peers",
		"previously open Desktop views", "same-provider",
		"prepareHandoff",
		"dirty marker", "best-effort `restore`", "prepareHandoffV2",
		"one SSH alias", "`/provider status`", "`/provider switch openai`", "`/provider switch sub2api`",
		"$HOME/.agents/skills/provider", "Provider switched to sub2api.",
		"Runtime provider: unknown.", "Selected provider: app-server configuration.",
		"will be applied before the next model turn",
		"does not invoke a model", "disappears after reopening",
		"CODEX_PROVIDER_SWITCHER_STATE_DIR",
		"does not read or parse `config.toml`", "optional `--provider`",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("documentation missing %q", required)
		}
	}
	for _, obsolete := range []string{
		"runs the official stdio proxy",
		"Server output is copied byte-for-byte",
		"Configure each Desktop Remote SSH entry point to launch the switcher as its stdio proxy",
		"The upstream Host is the fixed local URL placeholder `localhost`",
		"AcceptEnv CODEX_PROVIDER_SWITCHER_PROVIDER",
		"CODEX_PROVIDER_SWITCHER_PROVIDER",
		"`/provider openai`",
		"`/provider sub2api`",
		"`/ provider status`",
		"`/ provider switch <name>`",
	} {
		if strings.Contains(combined, obsolete) {
			t.Errorf("documentation retains obsolete claim %q", obsolete)
		}
	}
}

func TestProviderPluginExposesExplicitStatusAndSwitchCommands(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(
		repositoryRoot(t), "plugins", "codex-provider-switcher", ".codex-plugin", "plugin.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version   string `json:"version"`
		Interface struct {
			DefaultPrompt []string `json:"defaultPrompt"`
		} `json:"interface"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "0.5.1" {
		t.Fatalf("plugin version = %q, want 0.5.1", manifest.Version)
	}
	want := []string{
		"Use /provider status to show this task's current provider.",
		"Use /provider switch sub2api to switch this task to sub2api.",
	}
	if len(manifest.Interface.DefaultPrompt) != len(want) {
		t.Fatalf("default prompts = %#v", manifest.Interface.DefaultPrompt)
	}
	for index := range want {
		if manifest.Interface.DefaultPrompt[index] != want[index] {
			t.Fatalf("default prompt %d = %q, want %q", index, manifest.Interface.DefaultPrompt[index], want[index])
		}
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
