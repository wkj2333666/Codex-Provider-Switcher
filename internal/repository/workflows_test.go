package repository

import (
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
	for _, required := range []string{
		"github.com/coder/websocket v1.8.15",
		"Copyright (c) 2025 Coder",
		"Permission to use, copy, modify, and distribute this software",
		`THE SOFTWARE IS PROVIDED "AS IS"`,
	} {
		if !strings.Contains(string(content), required) {
			t.Errorf("THIRD_PARTY_NOTICES missing %q", required)
		}
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
		"WebSocket", "ln -s", "CODEX_PROVIDER_SWITCHER_PROVIDER",
		"AcceptEnv", "v0.1.0", "Uninstall", "64 MiB",
		"v0.2.0 is also incompatible with stock Desktop Remote SSH",
		"`codex app-server proxy` without `--sock`",
		"CODEX_PROVIDER_SWITCHER_SOCKET", "--strict-config",
		"same Unix user", "downstream `request.Host`", "THIRD_PARTY_NOTICES",
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
	} {
		if strings.Contains(combined, obsolete) {
			t.Errorf("documentation retains obsolete claim %q", obsolete)
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
