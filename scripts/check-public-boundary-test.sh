#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-public-boundary.sh"
fixture_root=$(mktemp -d "${TMPDIR:-/tmp}/agentbridge-boundary.XXXXXX")
denylist_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-boundary-denylist.XXXXXX")
output_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-boundary-output.XXXXXX")
cleanup() {
  rm -rf "$fixture_root"
  rm -f "$denylist_file" "$output_file"
}
trap cleanup EXIT

git -C "$fixture_root" init -q -b main
git -C "$fixture_root" config user.name "AgentBridge boundary test"
git -C "$fixture_root" config user.email "boundary-test@example.invalid"
printf 'public fixture\n' >"$fixture_root/README.md"
git -C "$fixture_root" add README.md
git -C "$fixture_root" commit -q -m "test: create public fixture"
base_commit=$(git -C "$fixture_root" rev-parse HEAD)

mkdir -p "$fixture_root/docs/provenance"
printf '[%s]\n' 'PL' >"$fixture_root/docs/provenance/donor-commits.md"
if (cd "$fixture_root" && "$checker" --generic) >/dev/null 2>&1; then
  printf 'public-boundary self-test: provenance marker bypassed generic check\n' >&2
  exit 1
fi
rm -f "$fixture_root/docs/provenance/donor-commits.md"

mkdir -p "$fixture_root/.github"
printf '[%s]\n' 'PL' >"$fixture_root/.github/private-marker.txt"
git -C "$fixture_root" add .github/private-marker.txt
if (cd "$fixture_root" && "$checker" --generic) >/dev/null 2>&1; then
  printf 'public-boundary self-test: hidden tracked marker bypassed generic check\n' >&2
  exit 1
fi
git -C "$fixture_root" reset -q
rm -f "$fixture_root/.github/private-marker.txt"

mkdir -p "$fixture_root/ignored"
printf 'ignored/\n' >"$fixture_root/.gitignore"
printf '[%s]\n' 'PL' >"$fixture_root/ignored/private-marker.txt"
git -C "$fixture_root" add .gitignore
git -C "$fixture_root" add -f ignored/private-marker.txt
if (cd "$fixture_root" && "$checker" --generic) >/dev/null 2>&1; then
  printf 'public-boundary self-test: tracked ignored marker bypassed generic check\n' >&2
  exit 1
fi
git -C "$fixture_root" reset -q
rm -rf "$fixture_root/ignored"
rm -f "$fixture_root/.gitignore"

printf 'prefix\0private-boundary-token\0suffix\n' >"$fixture_root/artifact.bin"
git -C "$fixture_root" add artifact.bin
git -C "$fixture_root" commit -q -m "test: add binary fixture"
printf 'private-boundary-token\n' >"$denylist_file"
if (
  cd "$fixture_root"
  AGENTBRIDGE_PRIVATE_DENYLIST_FILE="$denylist_file" \
    "$checker" --release-range "$base_commit..HEAD"
) >"$output_file" 2>&1; then
  printf 'public-boundary self-test: binary denylist token bypassed release check\n' >&2
  exit 1
fi
if grep -Fq 'private-boundary-token' "$output_file"; then
  printf 'public-boundary self-test: denylist token leaked to output\n' >&2
  exit 1
fi

printf 'public-boundary self-test: PASS\n'
