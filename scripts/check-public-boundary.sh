#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'public-boundary: %s\n' "$*" >&2
  exit 1
}

repository_root=$(git rev-parse --show-toplevel)
cd "$repository_root"

check_generic() {
  if git ls-files | grep -Eq '^deploy/(cloud|railway)/'; then
    die 'non-public deployment directory detected'
  fi
  if git grep -a -q '\[PL\]' -- . ':(exclude)scripts/check-public-boundary.sh'; then
    die 'private planning marker detected'
  fi
  if rg --hidden -a -q '\[PL\]' --glob '!.git/**' --glob '!scripts/check-public-boundary.sh' .; then
    die 'private planning marker detected'
  fi
  for module_file in go.mod go.work; do
    [[ -f "$module_file" ]] || continue
    if rg -q '^[[:space:]]*replace[[:space:]].*=>[[:space:]]*(/|[.][.]?/)' "$module_file"; then
      die 'local module replacement detected'
    fi
  done
  printf 'public-boundary: generic PASS\n'
}

digest_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  shasum -a 256 "$1" | awk '{print $1}'
}

check_release_range() {
  release_range=$1
  denylist_file=${AGENTBRIDGE_PRIVATE_DENYLIST_FILE:-}
  [[ -n "$denylist_file" ]] || die 'private denylist file is required'
  [[ -f "$denylist_file" && -s "$denylist_file" ]] || die 'private denylist file is unavailable or empty'
  git rev-list "$release_range" >/dev/null

  object_list=$(mktemp "${TMPDIR:-/tmp}/agentbridge-public-objects.XXXXXX")
  metadata_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-public-metadata.XXXXXX")
  blob_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-public-blob.XXXXXX")
  cleanup() {
    rm -f "$object_list" "$metadata_file" "$blob_file"
  }
  trap cleanup EXIT
  git rev-list --objects "$release_range" >"$object_list"
  git log --format=fuller "$release_range" >"$metadata_file"

  matches_denylist() {
    inspected_file=$1
    while IFS= read -r private_token || [[ -n "$private_token" ]]; do
      [[ -n "$private_token" && ${private_token:0:1} != '#' ]] || continue
      if grep -a -Fq -- "$private_token" "$inspected_file"; then
        return 0
      fi
    done <"$denylist_file"
    return 1
  }

  if matches_denylist "$object_list" || matches_denylist "$metadata_file"; then
    die 'private denylist match detected'
  fi
  while IFS=' ' read -r object_id _; do
    [[ -n "$object_id" ]] || continue
    object_type=$(git cat-file -t "$object_id") || die 'release object cannot be inspected'
    [[ "$object_type" == blob ]] || continue
    git cat-file blob "$object_id" >"$blob_file" || die 'release blob cannot be inspected'
    if matches_denylist "$blob_file"; then
      die 'private denylist match detected'
    fi
  done <"$object_list"

  printf 'public-boundary: release denylist sha256=%s PASS\n' "$(digest_file "$denylist_file")"
}

case "${1:-}" in
  --generic)
    (($# == 1)) || die '--generic takes no additional arguments'
    check_generic
    ;;
  --release-range)
    (($# == 2)) || die '--release-range requires exactly one Git range'
    check_generic
    check_release_range "$2"
    ;;
  *)
    die 'usage: check-public-boundary.sh --generic | --release-range <base..head>'
    ;;
esac
