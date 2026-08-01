# User-Friendly Bilingual README Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the implementation-heavy README with concise, matching English and Simplified Chinese guides centered on release installation and ordinary Desktop use.

**Architecture:** `README.md` remains the canonical English landing page and `README.zh-CN.md` mirrors it in natural Simplified Chinese. User workflows remain in both README files; protocol, routing, handoff, and security internals live in `docs/architecture.md`, with repository tests enforcing the bilingual structure and essential deployment content.

**Tech Stack:** Markdown, POSIX shell snippets, Go repository tests, GitHub Releases.

## Global Constraints

- The normal deployment path is a manually downloaded GitHub Release, not a remote one-line installer.
- Support Linux and macOS on amd64 and arm64.
- Preserve the real Codex executable and install the wrapper separately under `$HOME/.local/lib/codex-provider-switcher`.
- Install the explicit provider skill under `$HOME/.agents/skills/provider`.
- Do not preserve an old-version backup by default during upgrade.
- Keep `README.md` and `README.zh-CN.md` structurally and operationally equivalent.
- Keep advanced implementation details in `docs/architecture.md` and preserve all existing accuracy requirements.
- Do not reintroduce obsolete `/provider` command forms.

---

### Task 1: Define The Release Packaging Contract

**Files:**
- Modify: `internal/repository/workflows_test.go`
- Test: `internal/repository/workflows_test.go`

**Interfaces:**
- Consumes: the existing release workflow source assertions.
- Produces: a failing contract that prevents release archives from omitting the Simplified Chinese guide.

- [ ] **Step 1: Require the Chinese guide in release archives**

In `TestReleaseContainsRequiredTargetsChecksumsAndPermissions`, replace the
existing README copy requirement with these two requirements:

```go
`cp README.md README.zh-CN.md LICENSE THIRD_PARTY_NOTICES "$root/"`,
`tar -tzf "dist/${name}.tar.gz" | grep -Fx "${name}/README.zh-CN.md"`,
```

- [ ] **Step 2: Run the test to verify RED**

Run: `go test ./internal/repository -run TestReleaseContainsRequiredTargetsChecksumsAndPermissions -count=1`

Expected: FAIL because `.github/workflows/release.yml` neither copies nor checks
`README.zh-CN.md`.

- [ ] **Step 3: Commit the failing documentation contract**

```bash
git add internal/repository/workflows_test.go
git commit -m "test: require Chinese guide in release archives"
```

---

### Task 2: Rewrite The English User Guide

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Test: `internal/repository/workflows_test.go`

**Interfaces:**
- Consumes: the section and command contract from Task 1 plus the approved design.
- Produces: the canonical concise English guide and an architecture document containing any advanced accuracy claims removed from the old README.

- [ ] **Step 1: Replace the English README with the user-facing structure**

Write these sections in order:

```markdown
# Codex Provider Switcher

[简体中文](README.zh-CN.md)

short description and independent-project disclaimer

## What It Does
## Requirements
## Install
### 1. Download and verify a release
### 2. Install the wrapper and provider skill
### 3. Configure the login shell
### 4. Verify and reconnect Desktop
## Use One SSH Host
## Use The Provider Command
## Upgrade
## Troubleshooting
## Uninstall
## Development
## License
```

The download block must detect the release target and verify checksums on both
Linux and macOS:

```bash
VERSION="0.5.2"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

PACKAGE="codex-provider-switcher-${VERSION}-${OS}-${ARCH}"
RELEASE_URL="https://github.com/wkj2333666/Codex-Provider-Switcher/releases/download/v${VERSION}"
WORK_DIR="$(mktemp -d)"
cd "$WORK_DIR"

curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz"
curl -fLO "$RELEASE_URL/$PACKAGE.tar.gz.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "$PACKAGE.tar.gz.sha256"
else
  shasum -a 256 -c "$PACKAGE.tar.gz.sha256"
fi
tar -xzf "$PACKAGE.tar.gz"
cd "$PACKAGE"
```

The install block must record the real Codex before PATH changes, install the
wrapper and symlink, and replace the installed skill without a backup:

```bash
REAL_CODEX="$(command -v codex)"
case "$REAL_CODEX" in /*) ;; *) echo "Codex path must be absolute" >&2; exit 1 ;; esac

INSTALL_ROOT="$HOME/.local/lib/codex-provider-switcher"
mkdir -p "$INSTALL_ROOT/bin"
install -m 0755 codex-provider-switcher "$INSTALL_ROOT/codex-provider-switcher"
ln -sfn ../codex-provider-switcher "$INSTALL_ROOT/bin/codex"

mkdir -p "$HOME/.agents/skills"
rm -rf "$HOME/.agents/skills/provider"
cp -R plugins/codex-provider-switcher/skills/provider "$HOME/.agents/skills/provider"
```

Show these login-shell exports with `REAL_CODEX` replaced by its printed absolute
value:

```bash
export CODEX_PROVIDER_SWITCHER_CODEX="/absolute/path/to/real/codex"
export PATH="$HOME/.local/lib/codex-provider-switcher/bin:$PATH"
```

The upgrade section reuses the verified release package and runs only the
binary install plus exact skill replacement. It explicitly says not to replace
`CODEX_PROVIDER_SWITCHER_CODEX` with the wrapper path.

- [ ] **Step 2: Preserve advanced facts in architecture.md**

Compare the old README against `docs/architecture.md`. Move only missing
advanced claims required by `TestDocumentationDescribesStockDesktopWrapper`,
including exact compatibility history, wrapper option precedence, runtime
coordination, security boundary, and transport limits. Do not duplicate user
installation prose in the architecture document.

- [ ] **Step 3: Run the existing documentation test**

Run: `go test ./internal/repository -run TestDocumentationDescribesStockDesktopWrapper -count=1`

Expected: PASS, proving advanced accuracy claims were retained across the
English README and architecture document.

- [ ] **Step 4: Commit the English guide**

```bash
git add README.md docs/architecture.md
git commit -m "docs: simplify the English user guide"
```

---

### Task 3: Add The Simplified Chinese Guide

**Files:**
- Create: `README.zh-CN.md`
- Modify: `.github/workflows/release.yml`
- Test: `internal/repository/workflows_test.go`

**Interfaces:**
- Consumes: the final English section order and exact shell commands from Task 2.
- Produces: a natural Simplified Chinese guide with identical operational behavior and reciprocal language navigation.

- [ ] **Step 1: Write README.zh-CN.md**

Mirror every English section and command. Translate explanations naturally but
keep shell blocks, paths, environment variables, provider examples, URLs, and
version values identical. Use this section order:

```markdown
# Codex Provider Switcher

[English](README.md)

简短说明与非官方项目声明

## 功能
## 环境要求
## 安装
### 1. 下载并校验 Release
### 2. 安装 wrapper 和 provider skill
### 3. 配置登录 Shell
### 4. 验证并重新连接 Desktop
## 只使用一个 SSH 主机
## 使用 Provider 命令
## 升级
## 常见问题
## 卸载
## 开发
## 许可证
```

Use `/provider status`, `/provider switch openai`, and `/provider switch
sub2api` as literal examples. Explain that control responses are local and do
not invoke a model, and that users must reconnect the Desktop host after an
install or upgrade.

- [ ] **Step 2: Run the documentation and packaging tests to verify GREEN**

Run: `go test ./internal/repository -run 'TestReleaseContainsRequiredTargetsChecksumsAndPermissions|TestDocumentationDescribesStockDesktopWrapper' -count=1`

Expected: PASS.

- [ ] **Step 3: Include both guides in release archives**

Change the release copy command to:

```yaml
cp README.md README.zh-CN.md LICENSE THIRD_PARTY_NOTICES "$root/"
```

Update `TestReleaseContainsRequiredTargetsChecksumsAndPermissions` to require
that exact command and add this archive assertion to the workflow plus test:

```yaml
tar -tzf "dist/${name}.tar.gz" | grep -Fx "${name}/README.zh-CN.md"
```

- [ ] **Step 4: Compare headings and shell commands**

Run:

```bash
rg '^##' README.md README.zh-CN.md
rg -n 'VERSION=|RELEASE_URL=|INSTALL_ROOT=|CODEX_PROVIDER_SWITCHER_CODEX|/provider ' README.md README.zh-CN.md
```

Expected: matching section counts and identical deployment variables, paths,
URLs, and provider command examples.

- [ ] **Step 5: Commit the Chinese guide, packaging, and contract test**

```bash
git add README.zh-CN.md .github/workflows/release.yml internal/repository/workflows_test.go
git commit -m "docs: add the Simplified Chinese user guide"
```

---

### Task 4: Validate The Complete Documentation Change

**Files:**
- Verify: `README.md`
- Verify: `README.zh-CN.md`
- Verify: `docs/architecture.md`
- Verify: `.github/workflows/release.yml`
- Verify: `internal/repository/workflows_test.go`

**Interfaces:**
- Consumes: all documentation and tests from Tasks 1–3.
- Produces: a clean, reviewable branch ready for publication.

- [ ] **Step 1: Check formatting and completeness**

```bash
git diff --check main...HEAD
```

Expected: `git diff --check` exits zero. Read the three documents once for
unfinished markers or incomplete instructions and correct any found.

- [ ] **Step 2: Run repository tests and vet**

```bash
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [ ] **Step 3: Verify repository links and final scope**

Confirm both README files link to the counterpart language file,
`docs/architecture.md`, `LICENSE`, and `THIRD_PARTY_NOTICES`. Review the diff to
ensure README prose is user-facing and advanced implementation details appear
only in architecture.

- [ ] **Step 4: Commit any final documentation corrections**

If verification required corrections:

```bash
git add README.md README.zh-CN.md docs/architecture.md .github/workflows/release.yml internal/repository/workflows_test.go
git commit -m "docs: polish bilingual deployment guides"
```

If no correction was required, do not create an empty commit.
