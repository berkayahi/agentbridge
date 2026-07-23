#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
runner="$script_dir/test-required.sh"
fixture_root=$(mktemp -d "${TMPDIR:-/tmp}/agentbridge-test-required.XXXXXX")
cleanup() {
  rm -rf "$fixture_root"
}
trap cleanup EXIT

printf 'module example.com/testrequired\n\ngo 1.26.0\n' >"$fixture_root/go.mod"
printf '%s\n' \
  'package fixture' \
  '' \
  'import "testing"' \
  '' \
  'func TestPass(*testing.T) {}' \
  'func TestAlsoPass(*testing.T) {}' \
  'func TestSkip(t *testing.T) { t.Skip("intentional") }' \
  'func TestFail(t *testing.T) { t.Fatal("intentional") }' \
  >"$fixture_root/fixture_test.go"

expect_failure() {
  description=$1
  shift
  if (cd "$fixture_root" && "$runner" "$@") >/dev/null 2>&1; then
    printf 'test-required self-test: expected failure: %s\n' "$description" >&2
    exit 1
  fi
}

expect_failure 'zero match' ./... -run TestMissing -count=1
expect_failure 'missing alternative' ./... -run 'TestPass|TestMissing' -count=1
expect_failure 'all skipped' ./... -run TestSkip -count=1
expect_failure 'malformed pattern' ./... -run '[' -count=1
expect_failure 'package failure' ./... -run TestFail -count=1

(cd "$fixture_root" && "$runner" ./... -run 'TestPass|TestAlsoPass' -count=1) >/dev/null
printf 'test-required self-test: PASS\n'
