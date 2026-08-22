#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository="${CPS_SOURCE_DIR:-$(cd -- "$script_dir/.." && pwd -P)}"
install_root="${CPS_INSTALL_ROOT:-${HOME}/.local/lib/codex-provider-switcher}"
installed_binary="$install_root/codex-provider-switcher"
wrapper_link="$install_root/bin/codex"
temporary_root=""
staged_binary=""

fail() {
  printf 'codex-provider-switcher: %s\n' "$1" >&2
  exit 1
}

check_source() {
  local branch head remote_head

  branch="$(git -C "$repository" symbolic-ref --quiet --short HEAD 2>/dev/null)" || \
    fail "deployment requires branch main"
  [[ "$branch" == "main" ]] || fail "deployment requires branch main; found $branch"

  [[ -z "$(git -C "$repository" status --porcelain --untracked-files=normal)" ]] || \
    fail "deployment requires a clean worktree"

  git -C "$repository" show-ref --verify --quiet refs/remotes/origin/main || \
    fail "deployment requires local origin/main"

  head="$(git -C "$repository" rev-parse HEAD)"
  remote_head="$(git -C "$repository" rev-parse refs/remotes/origin/main)"
  [[ "$head" == "$remote_head" ]] || \
    fail "deployment requires HEAD to equal origin/main"

  printf 'source check passed: main@%s\n' "$head"
}

run_go_and_clean() {
  local status
  if "$@"; then
    status=0
  else
    status=$?
  fi
  go clean -cache -testcache || true
  return "$status"
}

cleanup() {
  if [[ -n "$temporary_root" ]]; then
    rm -rf -- "$temporary_root"
  fi
  if [[ -n "$staged_binary" && -e "$staged_binary" ]]; then
    rm -f -- "$staged_binary"
  fi
}

deploy() {
  local head short_commit version built_at ldflags candidate candidate_info expected_info

  check_source
  [[ "$(uname -s)" == "Linux" && "$(uname -m)" == "aarch64" ]] || \
    fail "local deployment target must be linux/arm64"

  head="$(git -C "$repository" rev-parse HEAD)"
  short_commit="${head:0:7}"
  version="main-${short_commit}"
  built_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  ldflags="-s -w -X main.version=${version} -X main.commit=${head} -X main.source=main -X main.builtAt=${built_at}"
  temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/cps-deploy.XXXXXX")"
  candidate="$temporary_root/codex-provider-switcher"

  run_go_and_clean go test ./... || fail "Go tests failed; installed binary was not changed"
  run_go_and_clean go vet ./... || fail "Go vet failed; installed binary was not changed"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 run_go_and_clean go build -trimpath \
    -ldflags "$ldflags" -o "$candidate" ./cmd/codex-provider-switcher || \
    fail "Go build failed; installed binary was not changed"

  candidate_info="$("$candidate" --build-info)" || fail "candidate metadata could not be read"
  expected_info="$(printf '{\"version\":\"%s\",\"commit\":\"%s\",\"source\":\"main\",\"builtAt\":\"%s\"}' "$version" "$head" "$built_at")"
  [[ "$candidate_info" == "$expected_info" ]] || fail "candidate metadata does not match source commit"

  install -d -m 0755 -- "$install_root"
  staged_binary="$install_root/.codex-provider-switcher.stage.$$"
  install -m 0755 -- "$candidate" "$staged_binary"
  mv -f -- "$staged_binary" "$installed_binary"
  staged_binary=""

  install -d -m 0755 -- "$(dirname -- "$wrapper_link")"
  ln -sfn -- ../codex-provider-switcher "$wrapper_link"
  [[ "$(readlink -- "$wrapper_link")" == "../codex-provider-switcher" ]] || \
    fail "wrapper link does not point to the installed switcher"

  [[ "$("$installed_binary" --build-info)" == "$expected_info" ]] || \
    fail "installed metadata does not match candidate"
  printf 'deployed %s (commit %s, source main)\n' "$installed_binary" "$head"
  printf 'reconnect Desktop Remote SSH to load the new switcher binary\n'
}

case "${1:-}" in
  --check-source)
    [[ "$#" -eq 1 ]] || fail "usage: scripts/deploy-local.sh [--check-source]"
    check_source
    ;;
  "")
    [[ "$#" -eq 0 ]] || fail "usage: scripts/deploy-local.sh [--check-source]"
    trap cleanup EXIT
    deploy
    ;;
  *)
    fail "usage: scripts/deploy-local.sh [--check-source]"
    ;;
esac
