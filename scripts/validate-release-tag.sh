#!/usr/bin/env bash
set -euo pipefail

tag="${1:-}"
pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?(\+([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$'

if [[ ! "$tag" =~ $pattern ]]; then
  printf 'release tag must be a semantic version prefixed by v\n' >&2
  exit 1
fi

version="${tag#v}"
without_build="${version%%+*}"
if [[ "$without_build" == *-* ]]; then
  prerelease="${without_build#*-}"
  IFS='.' read -r -a identifiers <<<"$prerelease"
  for identifier in "${identifiers[@]}"; do
    if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" == 0* && "$identifier" != "0" ]]; then
      printf 'numeric prerelease identifiers must not contain leading zeroes\n' >&2
      exit 1
    fi
  done
fi
