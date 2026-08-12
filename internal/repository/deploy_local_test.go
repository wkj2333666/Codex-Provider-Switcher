package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployLocalAcceptsCleanSynchronizedMain(t *testing.T) {
	repository := synchronizedRepository(t)
	output, err := checkDeploySource(t, repository)
	if err != nil {
		t.Fatalf("clean synchronized main rejected: %v\n%s", err, output)
	}
	if !strings.Contains(output, "source check passed: main@") {
		t.Fatalf("success output = %q", output)
	}
}

func TestDeployLocalRejectsUnsafeSources(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, string)
		message string
	}{
		{
			name: "non-main branch",
			mutate: func(t *testing.T, repository string) {
				runGit(t, repository, "switch", "-c", "topic")
			},
			message: "deployment requires branch main",
		},
		{
			name: "tracked dirt",
			mutate: func(t *testing.T, repository string) {
				writeFile(t, filepath.Join(repository, "tracked.txt"), "changed\n")
			},
			message: "deployment requires a clean worktree",
		},
		{
			name: "untracked dirt",
			mutate: func(t *testing.T, repository string) {
				writeFile(t, filepath.Join(repository, "untracked.txt"), "new\n")
			},
			message: "deployment requires a clean worktree",
		},
		{
			name: "missing origin main",
			mutate: func(t *testing.T, repository string) {
				runGit(t, repository, "update-ref", "-d", "refs/remotes/origin/main")
			},
			message: "deployment requires local origin/main",
		},
		{
			name: "unpushed main",
			mutate: func(t *testing.T, repository string) {
				writeFile(t, filepath.Join(repository, "tracked.txt"), "ahead\n")
				runGit(t, repository, "add", "tracked.txt")
				runGit(t, repository, "commit", "-m", "ahead")
			},
			message: "deployment requires HEAD to equal origin/main",
		},
		{
			name: "stale main",
			mutate: func(t *testing.T, repository string) {
				writeFile(t, filepath.Join(repository, "tracked.txt"), "remote-newer\n")
				runGit(t, repository, "add", "tracked.txt")
				runGit(t, repository, "commit", "-m", "remote newer")
				runGit(t, repository, "push", "origin", "main")
				runGit(t, repository, "reset", "--hard", "HEAD~1")
			},
			message: "deployment requires HEAD to equal origin/main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repository := synchronizedRepository(t)
			tt.mutate(t, repository)
			output, err := checkDeploySource(t, repository)
			if err == nil {
				t.Fatalf("unsafe source accepted:\n%s", output)
			}
			if !strings.Contains(output, tt.message) {
				t.Fatalf("failure output = %q, want %q", output, tt.message)
			}
		})
	}
}

func TestDeployLocalInstallsVerifiedCandidateAtomically(t *testing.T) {
	repository := synchronizedRepository(t)
	installRoot := filepath.Join(t.TempDir(), "install")
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "go"), fakeGoScript)

	output, err := runDeployLocal(t, repository, installRoot, fakeBin)
	if err != nil {
		t.Fatalf("deployment failed: %v\n%s", err, output)
	}
	installed := filepath.Join(installRoot, "codex-provider-switcher")
	metadata, err := exec.Command(installed, "--build-info").Output()
	if err != nil {
		t.Fatalf("installed --build-info: %v", err)
	}
	wantCommit := runGitOutput(t, repository, "rev-parse", "HEAD")
	if !strings.Contains(string(metadata), `"commit":"`+wantCommit+`"`) || !strings.Contains(string(metadata), `"source":"main"`) {
		t.Fatalf("installed metadata = %q, want commit %q and main source", metadata, wantCommit)
	}
	if info, err := os.Stat(installed); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %o, want 755", info.Mode().Perm())
	}
	entries, err := os.ReadDir(installRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "codex-provider-switcher" {
		t.Fatalf("install directory contains unexpected artifacts: %v", entries)
	}
	if !strings.Contains(output, "reconnect Desktop Remote SSH") {
		t.Fatalf("deployment output does not request reconnect: %q", output)
	}
}

func checkDeploySource(t *testing.T, repository string) (string, error) {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "scripts", "deploy-local.sh")
	command := exec.Command("bash", script, "--check-source")
	command.Env = append(os.Environ(), "CPS_SOURCE_DIR="+repository)
	output, err := command.CombinedOutput()
	return string(output), err
}

func synchronizedRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repository := filepath.Join(root, "source")
	runGit(t, root, "init", "--bare", origin)
	runGit(t, root, "init", "-b", "main", repository)
	runGit(t, repository, "config", "user.name", "Deployment Test")
	runGit(t, repository, "config", "user.email", "deployment-test@example.invalid")
	writeFile(t, filepath.Join(repository, "tracked.txt"), "base\n")
	runGit(t, repository, "add", "tracked.txt")
	runGit(t, repository, "commit", "-m", "base")
	runGit(t, repository, "remote", "add", "origin", origin)
	runGit(t, repository, "push", "-u", "origin", "main")
	return repository
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func runGitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(arguments, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func runDeployLocal(t *testing.T, repository, installRoot, fakeBin string) (string, error) {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "scripts", "deploy-local.sh")
	command := exec.Command("bash", script)
	command.Env = append(os.Environ(),
		"CPS_SOURCE_DIR="+repository,
		"CPS_INSTALL_ROOT="+installRoot,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

const fakeGoScript = `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "build" ]]; then
  output=""
  flags=""
  previous=""
  for argument in "$@"; do
    if [[ "$previous" == "-o" ]]; then output="$argument"; fi
    if [[ "$previous" == "-ldflags" ]]; then flags="$argument"; fi
    previous="$argument"
  done
  version="$(sed -n 's/.*-X main.version=\([^ ]*\).*/\1/p' <<<"$flags")"
  commit="$(sed -n 's/.*-X main.commit=\([^ ]*\).*/\1/p' <<<"$flags")"
  source="$(sed -n 's/.*-X main.source=\([^ ]*\).*/\1/p' <<<"$flags")"
  built_at="$(sed -n 's/.*-X main.builtAt=\([^ ]*\).*/\1/p' <<<"$flags")"
  cat >"$output" <<EOF
#!/usr/bin/env bash
if [[ "\${1:-}" == "--build-info" ]]; then
  printf '%s\\n' '{"version":"$version","commit":"$commit","source":"$source","builtAt":"$built_at"}'
elif [[ "\${1:-}" == "--version" ]]; then
  printf '%s\\n' 'codex-provider-switcher $version (commit ${commit:0:7})'
fi
EOF
  chmod 0755 "$output"
fi
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
