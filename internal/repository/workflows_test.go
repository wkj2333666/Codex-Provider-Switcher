package repository

import (
	"os"
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
		"contents: read", "pull_request:", "workflow_dispatch:",
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
	} {
		if !strings.Contains(content, required) {
			t.Errorf("release.yml missing %q", required)
		}
	}
	if count := strings.Count(content, "contents: write"); count != 1 {
		t.Errorf("release.yml has %d contents: write grants, want 1", count)
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	content, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
