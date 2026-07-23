#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'test-required: %s\n' "$*" >&2
  exit 1
}

run_pattern=
declare -a list_args=()
declare -a test_args=()

while (($#)); do
  case "$1" in
    -run)
      (($# >= 2)) || die '-run requires a pattern'
      [[ -z "$run_pattern" ]] || die 'only one -run pattern is supported'
      run_pattern=$2
      list_args+=(-list "$2")
      test_args+=("$1" "$2")
      shift 2
      ;;
    -run=*)
      [[ -z "$run_pattern" ]] || die 'only one -run pattern is supported'
      run_pattern=${1#-run=}
      list_args+=("-list=$run_pattern")
      test_args+=("$1")
      shift
      ;;
    -json)
      test_args+=("$1")
      shift
      ;;
    *)
      list_args+=("$1")
      test_args+=("$1")
      shift
      ;;
  esac
done

[[ -n "$run_pattern" ]] || die 'a non-empty -run pattern is required'

listing_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-test-list.XXXXXX")
result_file=$(mktemp "${TMPDIR:-/tmp}/agentbridge-test-result.XXXXXX")
cleanup() {
  rm -f "$listing_file" "$result_file"
}
trap cleanup EXIT

if ! go test "${list_args[@]}" >"$listing_file" 2>&1; then
  cat "$listing_file" >&2
  die 'go test -list failed'
fi

declare -a listed_tests=()
while IFS= read -r test_name; do
  listed_tests+=("$test_name")
done < <(
  sed -nE '/^(Test|Benchmark|Fuzz|Example)[[:alnum:]_]*$/p' "$listing_file" |
    sort -u
)
((${#listed_tests[@]} > 0)) || die "pattern matched no tests"

declare -a requirements=()
segment=
depth=0
escaped=0
for ((index = 0; index < ${#run_pattern}; index++)); do
  character=${run_pattern:index:1}
  if ((escaped)); then
    segment+=$character
    escaped=0
    continue
  fi
  case "$character" in
    '\\')
      segment+=$character
      escaped=1
      ;;
    '(' | '[' | '{')
      segment+=$character
      depth=$((depth + 1))
      ;;
    ')' | ']' | '}')
      segment+=$character
      if ((depth > 0)); then
        depth=$((depth - 1))
      fi
      ;;
    '|')
      if ((depth == 0)); then
        [[ -n "$segment" ]] || die 'empty top-level alternative'
        requirements+=("$segment")
        segment=
      else
        segment+=$character
      fi
      ;;
    *)
      segment+=$character
      ;;
  esac
done
[[ -n "$segment" ]] || die 'empty top-level alternative'
requirements+=("$segment")

declare -a required_tests=()
for requirement in "${requirements[@]}"; do
  matched=0
  for test_name in "${listed_tests[@]}"; do
    if [[ "$test_name" =~ $requirement ]]; then
      required_tests+=("$test_name")
      matched=1
    fi
  done
  ((matched == 1)) || die "required alternative matched no tests"
done

if ! go test -json "${test_args[@]}" >"$result_file"; then
  cat "$result_file"
  die 'go test failed'
fi
cat "$result_file"

declare -a unique_required_tests=()
while IFS= read -r test_name; do
  unique_required_tests+=("$test_name")
done < <(printf '%s\n' "${required_tests[@]}" | sort -u)
required_tests=("${unique_required_tests[@]}")
for test_name in "${required_tests[@]}"; do
  if grep -F "\"Test\":\"$test_name\"" "$result_file" | grep -Fq '"Action":"skip"'; then
    die "required test was skipped: $test_name"
  fi
  if ! grep -F "\"Test\":\"$test_name\"" "$result_file" | grep -Fq '"Action":"pass"'; then
    die "required test did not pass: $test_name"
  fi
done
