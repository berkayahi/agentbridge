#!/usr/bin/env bash
set -euo pipefail

mode="${1:-}"
[[ "$mode" == "--tag-trust-only" ]] || { echo "usage: $0 --tag-trust-only" >&2; exit 2; }
root="$(git rev-parse --show-toplevel)"
policy="$root/deploy/release/policy.json"
trust="$root/deploy/trust/release-signers.json"
bash "$root/scripts/release/verify-canonical-remote.sh" --policy "$policy" --repository "$root" >/dev/null
[[ "$(sed -n 's/.*"allow_force_update"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p' "$policy")" == false ]]
[[ "$(sed -n 's/.*"allow_delete"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p' "$policy")" == false ]]
[[ -s "$trust" ]]
echo "release trust policy preflight passed"
